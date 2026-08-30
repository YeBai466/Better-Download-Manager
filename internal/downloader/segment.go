package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errStalled = errors.New("download stalled")

type transferPlan struct {
	workers int
	chunks  []*Chunk
	lanes   []*Segment
}

type transferOptions struct {
	Retries      int
	StallTimeout time.Duration
	Limiter      *speedLimiter
	Connections  *connectionLimiter
	Identity     responseIdentity
	Cancel       context.CancelFunc
}

type responseIdentity struct {
	ETag         string
	LastModified string
	FinalURL     string
	TotalSize    int64
}

// urlHolder shares the resolved request URL between chunk workers so ranged
// requests hit the redirect target directly instead of re-walking the redirect
// chain once per chunk — while still being able to refresh an expired signed
// URL (S3/CDN style) by re-resolving the origin.
type urlHolder struct {
	mu        sync.Mutex
	current   string
	origin    string
	headers   map[string]string
	gen       int
	refreshes int
	inflight  chan struct{} // non-nil while a refresh is running
}

// maxURLRefreshes bounds origin re-resolution so a permanently broken signed
// URL can't loop forever.
const maxURLRefreshes = 4

func newURLHolder(current, origin string, headers map[string]string) *urlHolder {
	if current == "" {
		current = origin
	}
	return &urlHolder{current: current, origin: origin, headers: headers}
}

func (h *urlHolder) get() (string, int) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.current, h.gen
}

// refreshable reports whether a rejected URL can be re-resolved: either the
// URL came from a redirect (the origin can hand out a fresh one) or another
// worker already refreshed since gen was read.
func (h *urlHolder) refreshable(gen int) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.gen != gen || h.current != h.origin
}

// refresh re-resolves the origin URL after a worker saw an auth-style
// rejection on the shared URL. It is single-flight: when N workers hit the
// same expiry at once, exactly ONE re-resolves the origin and the others wait
// for it and then reuse the new URL. Without that, one expiry event would fire
// N origin requests and burn the whole refresh budget instantly.
func (h *urlHolder) refresh(ctx context.Context, client *http.Client, gen int, id responseIdentity) error {
	h.mu.Lock()
	for {
		if h.gen != gen { // someone already refreshed past the URL we used
			h.mu.Unlock()
			return nil
		}
		if h.inflight == nil {
			break
		}
		// Another worker is refreshing this same generation — wait it out.
		wait := h.inflight
		h.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-wait:
		}
		h.mu.Lock()
	}
	if h.refreshes >= maxURLRefreshes {
		h.mu.Unlock()
		return fatalError{fmt.Errorf("download URL kept expiring after %d refreshes", h.refreshes)}
	}
	h.refreshes++
	done := make(chan struct{})
	h.inflight = done
	origin := h.origin
	headers := h.headers
	h.mu.Unlock()

	fresh, err := resolveFreshURL(ctx, client, origin, headers, id)

	h.mu.Lock()
	if err == nil {
		h.current = fresh
		h.gen++
	}
	h.inflight = nil
	h.mu.Unlock()
	close(done) // wake the waiters, successfully refreshed or not
	return err
}

// resolveFreshURL re-walks the origin's redirect chain and verifies the target
// still serves the same representation.
func resolveFreshURL(ctx context.Context, client *http.Client, origin string, headers map[string]string, id responseIdentity) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin, nil)
	if err != nil {
		return "", err
	}
	applyHeaders(req, headers)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusPartialContent && resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("origin returned %s during URL refresh", resp.Status)
	}
	// The refreshed target must still serve the same representation.
	if resp.StatusCode == http.StatusPartialContent {
		if _, _, total, ok := parseContentRange(resp.Header.Get("Content-Range")); ok && total > 0 && id.TotalSize > 0 && total != id.TotalSize {
			return "", identityChangedError{fmt.Errorf("remote size changed during URL refresh: got %d want %d", total, id.TotalSize)}
		}
	}
	if id.ETag != "" && resp.Header.Get("ETag") != "" && resp.Header.Get("ETag") != id.ETag {
		return "", identityChangedError{fmt.Errorf("remote content changed during URL refresh")}
	}
	if resp.Request != nil && resp.Request.URL != nil {
		return resp.Request.URL.String(), nil
	}
	return origin, nil
}

// chunkQueue hands chunks to workers. While holdLowest is set, the lowest
// unclaimed chunk is reserved for the fast-start streamer, which extends its
// already-open whole-file response through contiguous chunks instead of paying
// a request round-trip (and often a fresh connection) per chunk.
type chunkQueue struct {
	mu         sync.Mutex
	chunks     []*Chunk
	claimed    []bool
	holdLowest bool
}

func newChunkQueue(chunks []*Chunk, holdLowest bool) *chunkQueue {
	return &chunkQueue{chunks: chunks, claimed: make([]bool, len(chunks)), holdLowest: holdLowest}
}

// nextChunk hands out the next unclaimed incomplete chunk for a ranged worker,
// skipping the streamer's reserved (lowest) chunk unless it is the only work
// left — a worker should steal it rather than idle while the streamer crawls.
func (q *chunkQueue) nextChunk() *Chunk {
	q.mu.Lock()
	defer q.mu.Unlock()
	first := -1
	for i, c := range q.chunks {
		if q.claimed[i] || c.Complete() {
			continue
		}
		if first == -1 {
			first = i
			if !q.holdLowest {
				break
			}
			continue
		}
		q.claimed[i] = true
		return q.chunks[i]
	}
	if first == -1 {
		return nil
	}
	q.holdLowest = false // reserved chunk was the only one left; release it
	q.claimed[first] = true
	return q.chunks[first]
}

// nextContiguous claims the reserved lowest chunk iff it starts exactly where
// the streamer's open response is positioned; otherwise streaming mode ends.
func (q *chunkQueue) nextContiguous(pos int64) *Chunk {
	q.mu.Lock()
	defer q.mu.Unlock()
	if !q.holdLowest {
		return nil
	}
	for i, c := range q.chunks {
		if q.claimed[i] || c.Complete() {
			continue
		}
		if c.Start == pos {
			q.claimed[i] = true
			return q.chunks[i]
		}
		break
	}
	q.holdLowest = false
	return nil
}

func (q *chunkQueue) streamerDone() {
	q.mu.Lock()
	q.holdLowest = false
	q.mu.Unlock()
}

// transientStatusError marks an HTTP status worth retrying (408/429/5xx),
// optionally carrying the server's Retry-After hint.
type transientStatusError struct {
	status     string
	retryAfter time.Duration
}

func (e transientStatusError) Error() string { return "server returned " + e.status }

func isTransientStatus(code int) bool {
	switch code {
	case http.StatusRequestTimeout, http.StatusTooEarly, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// isAuthExpiryStatus covers the statuses expiring signed URLs produce.
func isAuthExpiryStatus(code int) bool {
	switch code {
	case http.StatusUnauthorized, http.StatusForbidden, http.StatusNotFound, http.StatusGone:
		return true
	}
	return false
}

func parseRetryAfter(v string) time.Duration {
	if v == "" {
		return 0
	}
	if secs, err := time.ParseDuration(strings.TrimSpace(v) + "s"); err == nil && secs > 0 {
		return secs
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// maxRetryAfterWait caps honoring Retry-After so a hostile header can't park a
// worker for an hour.
const maxRetryAfterWait = 30 * time.Second

func downloadChunkWithRetry(
	ctx context.Context,
	client *http.Client,
	holder *urlHolder,
	headers map[string]string,
	chunk *Chunk,
	lane *Segment,
	w *fileWriter,
	progress *int64,
	opts transferOptions,
) error {
	if chunk.Complete() {
		return nil
	}
	retries := opts.Retries
	if retries < 1 {
		retries = defaultRetries
	}
	var last error
	// Progress resets the retry budget (below), so bound the total attempts
	// too: a server dripping a handful of bytes per connection would
	// otherwise keep one worker requesting forever.
	for attempt, total := 0, 0; attempt <= retries; attempt, total = attempt+1, total+1 {
		if total > maxChunkAttempts {
			return fmt.Errorf("chunk %d made too little progress after %d attempts: %w", chunk.Index, total, last)
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		start := chunk.Current()
		err := downloadChunk(ctx, client, holder, headers, chunk, lane, w, progress, opts)
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if isFatalDownloadError(err) || isIdentityChanged(err) {
			return err
		}
		last = err
		if chunk.Current() > start {
			// Forward progress was made: a connection that keeps delivering
			// bytes must never exhaust the retry budget, no matter how often
			// the server drops it (IDM behavior). The budget only counts
			// attempts that got nothing.
			attempt = -1
			continue
		}
		if attempt < retries {
			delay := retryDelay(attempt)
			var tse transientStatusError
			if errors.As(err, &tse) && tse.retryAfter > delay {
				delay = tse.retryAfter
				if delay > maxRetryAfterWait {
					delay = maxRetryAfterWait
				}
			}
			timer := time.NewTimer(delay)
			select {
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			case <-timer.C:
			}
		}
	}
	return fmt.Errorf("chunk %d failed after retries: %w", chunk.Index, last)
}

// downloadSegment is kept for older tests/helpers and the legacy transfer path.
// The engine's active path uses downloadChunkWithRetry and dynamic chunks.
func downloadSegment(
	ctx context.Context,
	client *http.Client,
	rawURL string,
	headers map[string]string,
	seg *Segment,
	w *fileWriter,
	ranged bool,
	progress *int64,
) error {
	if ranged {
		chunk := &Chunk{Index: seg.Index, Start: seg.Start, End: seg.End, Downloaded: seg.loaded()}
		opts := transferOptions{Retries: defaultRetries, StallTimeout: defaultStallTimeout, Identity: responseIdentity{TotalSize: seg.End + 1}}
		return downloadChunkWithRetry(ctx, client, newURLHolder(rawURL, rawURL, headers), headers, chunk, seg, w, progress, opts)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	applyHeaders(req, headers)
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return fmt.Errorf("expected 200 OK, got %s", resp.Status)
	}
	chunk := &Chunk{Index: seg.Index, Start: 0, End: seg.End, Downloaded: seg.loaded()}
	return streamOpenResponse(ctx, resp, chunk, seg, w, progress, transferOptions{StallTimeout: defaultStallTimeout}, seg.End >= 0)
}

// streamSegment is kept for the legacy fast-start path; new downloads use
// streamChunks / copyBody directly with Chunk state.
func streamSegment(
	ctx context.Context,
	resp *http.Response,
	seg *Segment,
	w *fileWriter,
	ranged bool,
	progress *int64,
) error {
	chunk := &Chunk{Index: seg.Index, Start: seg.Start, End: seg.End, Downloaded: seg.loaded()}
	return streamOpenResponse(ctx, resp, chunk, seg, w, progress, transferOptions{StallTimeout: defaultStallTimeout}, ranged && seg.End >= 0)
}

func downloadChunk(
	ctx context.Context,
	client *http.Client,
	holder *urlHolder,
	headers map[string]string,
	chunk *Chunk,
	lane *Segment,
	w *fileWriter,
	progress *int64,
	opts transferOptions,
) error {
	if opts.Connections != nil {
		if err := opts.Connections.Acquire(ctx); err != nil {
			return err
		}
		defer opts.Connections.Release()
	}

	reqCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	localOpts := opts
	localOpts.Cancel = cancel

	url, gen := holder.get()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	applyHeaders(req, headers)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", chunk.Current(), chunk.End))
	if v := ifRangeValue(opts.Identity); v != "" {
		req.Header.Set("If-Range", v)
	}

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusPartialContent:
		// validated below
	case resp.StatusCode == http.StatusRequestedRangeNotSatisfiable:
		// Our ranges were computed against the advertised size; 416 means the
		// remote representation shrank or was replaced.
		return identityChangedError{fmt.Errorf("server no longer satisfies range for chunk %d (%s)", chunk.Index, resp.Status)}
	case resp.StatusCode == http.StatusOK && req.Header.Get("If-Range") != "":
		// If-Range validator no longer matches: the remote file was replaced.
		return identityChangedError{fmt.Errorf("remote content changed (validator mismatch on chunk %d)", chunk.Index)}
	case isTransientStatus(resp.StatusCode):
		return transientStatusError{status: resp.Status, retryAfter: parseRetryAfter(resp.Header.Get("Retry-After"))}
	case isAuthExpiryStatus(resp.StatusCode) && holder.refreshable(gen):
		// Likely an expired signed URL: re-resolve through the origin, then
		// let the retry loop try again against the fresh target.
		if rerr := holder.refresh(ctx, client, gen, opts.Identity); rerr != nil {
			return rerr
		}
		return transientStatusError{status: resp.Status}
	default:
		return fatalError{err: fmt.Errorf("expected 206 Partial Content, got %s", resp.Status)}
	}
	if err := validatePartialResponse(resp, chunk.Current(), chunk.End, opts.Identity); err != nil {
		return err
	}
	return copyBody(reqCtx, resp.Body, chunk.Current(), chunk, lane, w, progress, localOpts, true)
}

// streamChunks consumes consecutive chunks from an already-open whole-file
// response over a single connection. The queue reserves the lowest unclaimed
// chunk for us, so while the transfer is healthy the fast-start connection
// keeps streaming the file front with zero extra round-trips; ranged workers
// chew through the rest and both meet in the middle. On a mid-stream error the
// current chunk is finished with normal ranged retries and the remaining
// chunks fall back to the per-chunk path.
func streamChunks(
	ctx context.Context,
	resp *http.Response,
	respCancel context.CancelFunc,
	client *http.Client,
	holder *urlHolder,
	headers map[string]string,
	q *chunkQueue,
	lane *Segment,
	w *fileWriter,
	progress *int64,
	opts transferOptions,
) error {
	defer q.streamerDone()
	defer resp.Body.Close()

	cur := q.nextContiguous(0)
	if cur == nil {
		return nil
	}
	streamOpts := opts
	streamOpts.Cancel = respCancel
	for {
		if err := copyBody(ctx, resp.Body, cur.Current(), cur, lane, w, progress, streamOpts, true); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			// The open stream died mid-chunk; finish this chunk on fresh
			// ranged connections, then hand control back to the caller's
			// normal per-chunk loop.
			return downloadChunkWithRetry(ctx, client, holder, headers, cur, lane, w, progress, opts)
		}
		next := q.nextContiguous(cur.End + 1)
		if next == nil {
			return nil
		}
		cur = next
	}
}

func streamOpenResponse(
	ctx context.Context,
	resp *http.Response,
	chunk *Chunk,
	lane *Segment,
	w *fileWriter,
	progress *int64,
	opts transferOptions,
	capped bool,
) error {
	defer resp.Body.Close()
	return copyBody(ctx, resp.Body, chunk.Current(), chunk, lane, w, progress, opts, capped)
}

func copyBody(
	ctx context.Context,
	body io.Reader,
	offset int64,
	chunk *Chunk,
	lane *Segment,
	w *fileWriter,
	progress *int64,
	opts transferOptions,
	capped bool,
) error {
	buf := make([]byte, copyBufferSize(opts.Limiter))
	stall := opts.StallTimeout
	if stall <= 0 {
		stall = defaultStallTimeout
	}
	var lastProgressUnix atomic.Int64
	lastProgressUnix.Store(time.Now().UnixNano())
	var stalled atomic.Bool
	if opts.Cancel != nil {
		// Watchdog: body.Read can block forever on a server that sends
		// headers then goes silent; the only way to unblock it is cancelling
		// the request context. The in-loop check below only runs after a Read
		// returns, so it can never catch this case on its own.
		interval := stall / 2
		if interval < 50*time.Millisecond {
			interval = 50 * time.Millisecond
		}
		doneWatch := make(chan struct{})
		go func() {
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for {
				select {
				case <-doneWatch:
					return
				case <-ticker.C:
					last := time.Unix(0, lastProgressUnix.Load())
					if time.Since(last) > stall {
						stalled.Store(true)
						opts.Cancel()
						return
					}
				}
			}
		}()
		defer close(doneWatch)
	}
	for {
		if capped && chunk.Complete() {
			return nil
		}
		if err := ctx.Err(); err != nil {
			if stalled.Load() {
				return errStalled
			}
			return err
		}
		// Never read past the chunk boundary: a shared streaming response
		// continues into the next chunk, and over-reading would silently
		// discard bytes that belong to it.
		readBuf := buf
		if capped {
			if rem := chunk.Remaining(); rem >= 0 && rem < int64(len(readBuf)) {
				readBuf = readBuf[:rem]
			}
		}
		n, readErr := body.Read(readBuf)
		if n > 0 {
			lastProgressUnix.Store(time.Now().UnixNano())
			if opts.Limiter != nil {
				if err := opts.Limiter.Wait(ctx, n); err != nil {
					return err
				}
				// Time spent sleeping in OUR rate limiter is not a server
				// stall — re-stamp so a low speed limit can't make the
				// watchdog kill a perfectly healthy throttled transfer.
				lastProgressUnix.Store(time.Now().UnixNano())
			}
			if _, werr := w.WriteAt(readBuf[:n], offset); werr != nil {
				return werr
			}
			offset += int64(n)
			chunk.add(int64(n))
			if lane != nil {
				lane.add(int64(n))
			}
			atomic.AddInt64(progress, int64(n))
			lastProgressUnix.Store(time.Now().UnixNano())
			if capped && chunk.Complete() {
				return nil
			}
		}
		if readErr == io.EOF {
			if capped && !chunk.Complete() {
				return io.ErrUnexpectedEOF
			}
			return nil
		}
		if readErr != nil {
			if stalled.Load() {
				return errStalled
			}
			return readErr
		}
		if time.Since(time.Unix(0, lastProgressUnix.Load())) > stall {
			return errStalled
		}
	}
}

// buildTransferPlan creates UI lanes and resumable chunks. The UI lane count is
// the worker count; chunks are smaller work units consumed dynamically.
func buildTransferPlan(totalSize int64, requested int) transferPlan {
	workers := smartConnections(totalSize, requested)
	if totalSize <= 0 {
		return transferPlan{
			workers: 1,
			chunks:  []*Chunk{{Index: 0, Start: 0, End: -1}},
			lanes:   []*Segment{{Index: 0, Start: 0, End: -1}},
		}
	}
	if int64(workers) > totalSize {
		workers = int(totalSize)
	}
	if workers < 1 {
		workers = 1
	}
	chunkSize := smartChunkSize(totalSize)
	chunks := make([]*Chunk, 0, (totalSize/chunkSize)+1)
	for start, idx := int64(0), 0; start < totalSize; idx++ {
		end := start + chunkSize - 1
		if end >= totalSize {
			end = totalSize - 1
		}
		chunks = append(chunks, &Chunk{Index: idx, Start: start, End: end})
		start = end + 1
	}
	lanes := buildWorkerLanes(totalSize, workers)
	return transferPlan{workers: workers, chunks: chunks, lanes: lanes}
}

func buildWorkerLanes(totalSize int64, n int) []*Segment {
	if n < 1 {
		n = 1
	}
	lanes := make([]*Segment, n)
	end := totalSize - 1
	if totalSize <= 0 {
		end = -1
	}
	for i := 0; i < n; i++ {
		lanes[i] = &Segment{Index: i, Start: 0, End: end}
	}
	return lanes
}

// buildSegments splits totalSize into n UI lanes.
func buildSegments(totalSize int64, n int) []*Segment {
	if n < 1 {
		n = 1
	}
	if totalSize <= 0 {
		return []*Segment{{Index: 0, Start: 0, End: -1}}
	}
	if int64(n) > totalSize {
		n = int(totalSize)
		if n < 1 {
			n = 1
		}
	}
	segs := make([]*Segment, n)
	base := totalSize / int64(n)
	var start int64
	for i := 0; i < n; i++ {
		end := start + base - 1
		if i == n-1 {
			end = totalSize - 1
		}
		segs[i] = &Segment{Index: i, Start: start, End: end}
		start = end + 1
	}
	return segs
}

func validatePartialResponse(resp *http.Response, start, end int64, id responseIdentity) error {
	rs, re, total, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok {
		return fatalError{err: fmt.Errorf("missing or invalid Content-Range")}
	}
	if rs != start {
		// Bytes would land at the wrong offset — never write these.
		return fatalError{err: fmt.Errorf("range offset mismatch: got %d-%d want start %d", rs, re, start)}
	}
	// Some CDNs legally answer with a SHORTER range than requested; the retry
	// loop keeps requesting the remainder. Only a range extending BEYOND what
	// we asked for is rejected.
	if re < rs || re > end {
		return fatalError{err: fmt.Errorf("range mismatch: got %d-%d want %d-%d", rs, re, start, end)}
	}
	if id.TotalSize > 0 && total > 0 && total != id.TotalSize {
		return identityChangedError{fmt.Errorf("remote size changed: got %d want %d", total, id.TotalSize)}
	}
	if id.ETag != "" && resp.Header.Get("ETag") != "" && resp.Header.Get("ETag") != id.ETag {
		return identityChangedError{fmt.Errorf("remote ETag changed")}
	}
	if id.LastModified != "" && resp.Header.Get("Last-Modified") != "" && resp.Header.Get("Last-Modified") != id.LastModified {
		return identityChangedError{fmt.Errorf("remote Last-Modified changed")}
	}
	return nil
}

// ifRangeValue picks the validator for If-Range. RFC 9110 only allows strong
// validators there; a weak ETag makes compliant servers ignore the condition
// and answer 200 with the full body, which would needlessly kill the transfer.
func ifRangeValue(id responseIdentity) string {
	if et := id.ETag; et != "" && !strings.HasPrefix(et, "W/") && !strings.HasPrefix(et, "w/") {
		return et
	}
	return id.LastModified
}

type fatalError struct {
	err error
}

func (e fatalError) Error() string { return e.err.Error() }
func (e fatalError) Unwrap() error { return e.err }

func isFatalDownloadError(err error) bool {
	var fe fatalError
	return errors.As(err, &fe)
}

// identityChangedError means the remote content is no longer the file this
// transfer started with (validator/size mismatch). The engine reacts by
// throwing the partial data away and restarting from scratch instead of
// surfacing a dead task.
type identityChangedError struct {
	err error
}

func (e identityChangedError) Error() string { return e.err.Error() }
func (e identityChangedError) Unwrap() error { return e.err }

func isIdentityChanged(err error) bool {
	var ie identityChangedError
	return errors.As(err, &ie)
}

func retryDelay(attempt int) time.Duration {
	base := time.Duration(200*(1<<attempt)) * time.Millisecond
	jitter := time.Duration(rand.Intn(120)) * time.Millisecond
	if base > 5*time.Second {
		base = 5 * time.Second
	}
	return base + jitter
}

func copyBufferSize(l *speedLimiter) int {
	if l == nil {
		return 256 * 1024
	}
	l.mu.Lock()
	rate := l.rate
	l.mu.Unlock()
	if rate > 0 {
		// Keep one buffer worth of data under ~0.5s of the limit, so throttled
		// transfers stay responsive to pause and the progress readout doesn't
		// move in coarse jumps.
		size := int(rate / 2)
		if size < 4*1024 {
			size = 4 * 1024
		}
		if size > 64*1024 {
			size = 64 * 1024
		}
		return size
	}
	return 256 * 1024
}
