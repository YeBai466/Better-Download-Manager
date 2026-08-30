package downloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/yebai/better-download-manager/internal/proxy"
)

// DefaultConnections is the number of parallel connections used per task when
// the caller does not specify one (matches IDM's common default).
const DefaultConnections = 8

// Config configures an Engine. The callbacks let the host (Wails service) react
// to task updates without the engine depending on the UI or storage layers.
type Config struct {
	MaxConcurrent  int                               // max simultaneously downloading tasks
	MaxConnections int                               // max simultaneous HTTP transfers across all tasks
	SpeedLimit     int64                             // global bytes/sec, 0 = unlimited
	Retries        int                               // retries per ranged chunk
	StallTimeout   time.Duration                     // no-progress timeout per response
	MetaInterval   time.Duration                     // periodic resume-state flush interval
	ClientFactory  func(proxy.Settings) *http.Client // builds the HTTP client (proxy-aware)
	OnUpdate       func(info TaskInfo)               // throttled progress + status changes
	OnPersist      func(rec Record)                  // durable state changes (status only)
	OnRemoved      func(id string)                   // task removed
}

// Engine manages the task queue, scheduling and lifecycle of downloads.
type Engine struct {
	cfg Config

	mu          sync.Mutex
	tasks       map[string]*managed
	order       []string
	activeCount int
	closed      bool

	limiter     *speedLimiter
	connections *connectionLimiter
}

type managed struct {
	task    *Task
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
	removed bool
}

// NewEngine creates an engine with sensible defaults applied to cfg.
func NewEngine(cfg Config) *Engine {
	rt := normalizeRuntimeConfig(RuntimeConfig{
		MaxConcurrent:  cfg.MaxConcurrent,
		MaxConnections: cfg.MaxConnections,
		SpeedLimit:     cfg.SpeedLimit,
		Retries:        cfg.Retries,
		StallTimeout:   cfg.StallTimeout,
		MetaInterval:   cfg.MetaInterval,
	})
	cfg.MaxConcurrent = rt.MaxConcurrent
	cfg.MaxConnections = rt.MaxConnections
	cfg.SpeedLimit = rt.SpeedLimit
	cfg.Retries = rt.Retries
	cfg.StallTimeout = rt.StallTimeout
	cfg.MetaInterval = rt.MetaInterval
	if cfg.ClientFactory == nil {
		cfg.ClientFactory = func(proxy.Settings) *http.Client { return &http.Client{} }
	}
	if cfg.OnUpdate == nil {
		cfg.OnUpdate = func(TaskInfo) {}
	}
	if cfg.OnPersist == nil {
		cfg.OnPersist = func(Record) {}
	}
	if cfg.OnRemoved == nil {
		cfg.OnRemoved = func(string) {}
	}
	return &Engine{
		cfg:         cfg,
		tasks:       map[string]*managed{},
		limiter:     newSpeedLimiter(cfg.SpeedLimit),
		connections: newConnectionLimiter(cfg.MaxConnections),
	}
}

// ErrNotFound is returned when an operation references an unknown task id.
var ErrNotFound = errors.New("task not found")

// UpdateRuntime applies settings that can change while the app is running.
// Speed limits apply to in-flight transfers immediately (shared limiter); a
// changed connection cap only applies to transfers started afterwards — the
// old limiter stays with running workers so its accounting stays consistent.
func (e *Engine) UpdateRuntime(rt RuntimeConfig) {
	rt = normalizeRuntimeConfig(rt)
	e.mu.Lock()
	e.cfg.MaxConcurrent = rt.MaxConcurrent
	e.cfg.MaxConnections = rt.MaxConnections
	e.cfg.SpeedLimit = rt.SpeedLimit
	e.cfg.Retries = rt.Retries
	e.cfg.StallTimeout = rt.StallTimeout
	e.cfg.MetaInterval = rt.MetaInterval
	e.connections = newConnectionLimiter(rt.MaxConnections)
	e.mu.Unlock()
	e.limiter.SetRate(rt.SpeedLimit)
	e.schedule()
}

// AddOptions describes a new download to add.
type AddOptions struct {
	ID          string
	URL         string
	Filename    string
	SavePath    string
	Category    string
	Connections int
	Headers     map[string]string
	Proxy       proxy.Settings
	AutoStart   bool
}

// Add registers a new task. When AutoStart is true it is queued immediately.
func (e *Engine) Add(opts AddOptions) (TaskInfo, error) {
	conns := opts.Connections
	if conns < 1 {
		conns = DefaultConnections
	}
	t := &Task{
		ID:          opts.ID,
		URL:         opts.URL,
		Filename:    opts.Filename,
		SavePath:    opts.SavePath,
		Category:    opts.Category,
		TotalSize:   -1,
		Connections: conns,
		Headers:     opts.Headers,
		Proxy:       opts.Proxy,
		Status:      StatusQueued,
		CreatedAt:   time.Now(),
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return TaskInfo{}, errors.New("engine closed")
	}
	if _, dup := e.tasks[t.ID]; dup {
		e.mu.Unlock()
		return TaskInfo{}, fmt.Errorf("duplicate task id %q", t.ID)
	}
	e.tasks[t.ID] = &managed{task: t}
	e.order = append(e.order, t.ID)
	e.mu.Unlock()

	if !opts.AutoStart {
		t.setStatus(StatusPaused, "")
	}
	// Emit an update so any window (incl. the main list) shows the new task,
	// then persist it. AutoStart tasks are then scheduled to run.
	e.emit(t)
	if opts.AutoStart {
		e.schedule()
	}
	return t.Snapshot(), nil
}

// Restore re-registers a persisted task (e.g. on startup) without auto-starting.
func (e *Engine) Restore(t *Task) {
	if t.Status == StatusDownloading || t.Status == StatusConnecting {
		t.Status = StatusPaused // we were not cleanly stopped
	}
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return
	}
	e.tasks[t.ID] = &managed{task: t}
	e.order = append(e.order, t.ID)
	e.mu.Unlock()
}

// Start queues a paused/errored task for download.
func (e *Engine) Start(id string) error {
	e.mu.Lock()
	m, ok := e.tasks[id]
	e.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	switch m.task.getStatus() {
	case StatusDownloading, StatusConnecting, StatusCompleted:
		return nil
	}
	m.task.setStatus(StatusQueued, "")
	e.emitManaged(m)
	e.schedule()
	return nil
}

// Pause stops an active task; its progress is preserved for resume. It returns
// immediately: the status flips to Paused at once for instant UI feedback while
// the worker goroutine unwinds and flushes its resume metadata in the
// background.
func (e *Engine) Pause(id string) error {
	e.mu.Lock()
	m, ok := e.tasks[id]
	e.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	for {
		switch m.task.getStatus() {
		case StatusDownloading, StatusConnecting:
			// Re-read the cancel func under the engine lock: the launch that
			// flipped the status also assigned it, possibly after our first
			// read of m.
			e.mu.Lock()
			cancel := m.cancel
			e.mu.Unlock()
			if cancel != nil {
				cancel()
			}
			m.task.recalcDownloaded() // reflect current segment progress immediately
			m.task.mu.Lock()
			m.task.Speed = 0
			m.task.mu.Unlock()
			m.task.setStatus(StatusPaused, "")
			e.emitManaged(m)
			return nil
		case StatusQueued:
			// CAS so a concurrent launch can't overwrite the pause: if the
			// worker won the race and already flipped Queued->Connecting, loop
			// and take the cancel path instead.
			if m.task.casStatus(StatusQueued, StatusPaused) {
				e.emitManaged(m)
				return nil
			}
		default:
			return nil
		}
	}
}

// Remove cancels (if running) and deletes a task. It returns immediately; the
// worker is cancelled and file cleanup (when deleteFile is set) runs in the
// background once the worker has fully stopped, so the UI updates instantly.
func (e *Engine) Remove(id string, deleteFile bool) error {
	e.mu.Lock()
	m, ok := e.tasks[id]
	if !ok {
		e.mu.Unlock()
		return ErrNotFound
	}
	m.removed = true
	delete(e.tasks, id)
	for i, oid := range e.order {
		if oid == id {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	cancel := m.cancel
	done := m.done
	running := m.running
	e.mu.Unlock()

	if running && cancel != nil {
		cancel()
	}
	e.cfg.OnRemoved(id)

	go func() {
		if running && done != nil {
			<-done // let the worker finish writing before we touch the files
		}
		if deleteFile {
			removeFinal(m.task.SavePath)
			removeMeta(m.task.SavePath)
			removePartial(m.task.SavePath)
		}
	}()
	return nil
}

// List returns snapshots of all tasks in insertion order.
func (e *Engine) List() []TaskInfo {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]TaskInfo, 0, len(e.order))
	for _, id := range e.order {
		if m, ok := e.tasks[id]; ok {
			out = append(out, m.task.Snapshot())
		}
	}
	return out
}

// Get returns a single task snapshot.
func (e *Engine) Get(id string) (TaskInfo, error) {
	e.mu.Lock()
	m, ok := e.tasks[id]
	e.mu.Unlock()
	if !ok {
		return TaskInfo{}, ErrNotFound
	}
	return m.task.Snapshot(), nil
}

// Shutdown pauses all active tasks and prevents new ones from starting.
func (e *Engine) Shutdown() {
	// Snapshot cancel/done under the lock: a worker's deferred cleanup writes
	// m.cancel concurrently (also under the lock).
	type stopper struct {
		cancel context.CancelFunc
		done   chan struct{}
	}
	e.mu.Lock()
	e.closed = true
	running := make([]stopper, 0)
	for _, m := range e.tasks {
		if m.running {
			running = append(running, stopper{cancel: m.cancel, done: m.done})
		}
	}
	e.mu.Unlock()
	for _, s := range running {
		if s.cancel != nil {
			s.cancel()
		}
	}
	for _, s := range running {
		<-s.done
	}
}

// schedule launches queued tasks until the concurrency limit is reached.
func (e *Engine) schedule() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return
	}
	for _, id := range e.order {
		if e.activeCount >= e.cfg.MaxConcurrent {
			return
		}
		m := e.tasks[id]
		if m == nil || m.running {
			continue
		}
		if m.task.getStatus() != StatusQueued {
			continue
		}
		e.launchLocked(m)
	}
}

// launchLocked starts a task's worker. Caller must hold e.mu.
func (e *Engine) launchLocked(m *managed) {
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	m.running = true
	e.activeCount++
	go e.run(ctx, m)
}

// run performs the full download for one task and reschedules on completion.
func (e *Engine) run(ctx context.Context, m *managed) {
	t := m.task
	defer func() {
		e.mu.Lock()
		m.running = false
		m.cancel = nil
		e.activeCount--
		e.mu.Unlock()
		close(m.done)
		e.schedule()
	}()

	// Queued->Connecting is a CAS: a Pause that landed between scheduling and
	// this goroutine starting must win, not be silently clobbered.
	if !t.casStatus(StatusQueued, StatusConnecting) {
		return
	}
	client := e.cfg.ClientFactory(t.Proxy)
	e.emitManaged(m)

	// One transparent restart when the remote content changed mid-transfer
	// (validator/size mismatch): throw the mismatched partial data away and
	// download the new file instead of stranding the task in an error state.
	for attempt := 0; ; attempt++ {
		err := e.runOnce(ctx, client, t, m)
		if err == nil {
			removeMeta(t.SavePath)
			t.recalcDownloaded()
			t.setStatus(StatusCompleted, "")
			e.emitManaged(m)
			return
		}
		if isIdentityChanged(err) && ctx.Err() == nil && attempt == 0 {
			e.resetPartial(t)
			continue
		}
		e.fail(t, ctx, err)
		return
	}
}

// runOnce performs a single resume-or-fresh transfer attempt to completion.
func (e *Engine) runOnce(ctx context.Context, client *http.Client, t *Task, m *managed) error {
	// Resume path: a task we already know the layout of (paused→resumed in the
	// same session, restored from the DB, or with a sidecar .bdmeta) skips
	// probing and just refetches the remaining ranges. Fresh tasks take the
	// fast-start path, which starts streaming bytes on the very first
	// connection. A network-level validation failure aborts WITHOUT resetting:
	// a blip at resume time must not cost the user their partial data.
	resumable, rerr := e.loadResume(ctx, client, t)
	if rerr != nil {
		return rerr
	}

	w, err := openPartFile(t.SavePath)
	if err != nil {
		return err
	}
	if !resumable {
		// Fresh start over a possibly stale .part (earlier failed attempt,
		// leftover from a deleted DB row): clear it, or a smaller new file
		// would inherit the old tail after finalize. Shrinking is cheap —
		// only preallocation (growing) is slow on Windows, see openPartFile.
		if err := w.Truncate(0); err != nil {
			w.Close()
			return err
		}
	}

	var xferErr error
	if resumable {
		t.setStatus(StatusDownloading, "")
		e.emitManaged(m)
		xferErr = e.transferV2(ctx, client, t, w)
	} else {
		xferErr = e.fastStartV2(ctx, client, t, w, m)
	}
	if xferErr != nil {
		w.Close()
		_ = writeMeta(t)
		return xferErr
	}
	// Completeness gate. Workers only return nil once their chunk is full, so
	// reaching here should imply a whole file — but that is derived, not
	// checked. Verify it explicitly: a silently truncated or hole-punched
	// "completed" file is the worst failure this engine can produce, and the
	// check costs one in-memory pass plus a Stat.
	if err := verifyComplete(t); err != nil {
		w.Close()
		_ = writeMeta(t)
		return err
	}
	return finalize(w, t.SavePath)
}

// verifyComplete reports whether every planned byte was actually fetched.
func verifyComplete(t *Task) error {
	t.mu.RLock()
	total := t.TotalSize
	chunks := append([]*Chunk(nil), t.Chunks...)
	savePath := t.SavePath
	t.mu.RUnlock()
	if total <= 0 {
		return nil // unknown length: the stream's EOF is the only authority
	}
	var got int64
	for _, c := range chunks {
		if c == nil {
			continue
		}
		if !c.Complete() {
			return fmt.Errorf("incomplete transfer: chunk %d has %d/%d bytes", c.Index, c.loaded(), c.size())
		}
		got += c.loaded()
	}
	if got != total {
		return fmt.Errorf("incomplete transfer: %d of %d bytes", got, total)
	}
	if info, err := os.Stat(partPath(savePath)); err == nil && info.Size() != total {
		return fmt.Errorf("partial file is %d bytes, expected %d", info.Size(), total)
	}
	return nil
}

// loadResume validates saved byte ranges before allowing a ranged resume.
// A non-nil error means validation could not be performed (network failure) —
// the caller must abort without touching the partial data, so the user can
// simply retry once the connection is back.
func (e *Engine) loadResume(ctx context.Context, client *http.Client, t *Task) (bool, error) {
	t.mu.RLock()
	hasChunks := len(t.Chunks) > 0
	hasSegments := len(t.Segments) > 0
	ranged := t.Resumable
	t.mu.RUnlock()

	if hasChunks && ranged {
		ok, err := e.validateResume(ctx, client, t)
		if err != nil {
			return false, err
		}
		if ok {
			t.recalcDownloaded()
			return true, nil
		}
	}
	if hasSegments && !hasChunks && ranged {
		e.migrateSegmentsToChunks(t)
		ok, err := e.validateResume(ctx, client, t)
		if err != nil {
			return false, err
		}
		if ok {
			t.recalcDownloaded()
			return true, nil
		}
	}

	if m, err := readMeta(t.SavePath); err == nil && m.TotalSize > 0 {
		ok, verr := e.applyMetaIfSafe(ctx, client, t, m)
		if verr != nil {
			return false, verr
		}
		if ok {
			t.recalcDownloaded()
			return true, nil
		}
		e.resetPartial(t)
		return false, nil
	}
	if hasChunks || hasSegments {
		e.resetPartial(t)
	}
	return false, nil
}

func (e *Engine) applyMetaIfSafe(ctx context.Context, client *http.Client, t *Task, m *metaFile) (bool, error) {
	if m.URL != "" && m.URL != t.URL {
		return false, nil
	}
	if !m.Resumable || m.TotalSize <= 0 {
		return false, nil
	}
	t.mu.Lock()
	t.TotalSize = m.TotalSize
	t.Resumable = m.Resumable
	t.MIME = m.MIME
	t.ETag = m.ETag
	t.LastModified = m.LastModified
	t.FinalURL = m.FinalURL
	if t.Filename == "" {
		t.Filename = m.Filename
	}
	t.Segments = make([]*Segment, len(m.Segments))
	for i := range m.Segments {
		s := m.Segments[i]
		t.Segments[i] = &s
	}
	if len(m.Chunks) > 0 {
		t.Chunks = make([]*Chunk, len(m.Chunks))
		for i := range m.Chunks {
			c := m.Chunks[i]
			t.Chunks[i] = &c
		}
	} else {
		t.Chunks = nil
	}
	t.mu.Unlock()
	if len(m.Chunks) == 0 {
		e.migrateSegmentsToChunks(t)
	}
	return e.validateResume(ctx, client, t)
}

// validateResume re-checks the remote file before reusing partial data.
// It returns (true, nil) to resume, (false, nil) when the content genuinely
// no longer matches (caller restarts from scratch), and (false, err) when the
// check could not be completed — a network blip or a transient 5xx must never
// be mistaken for "the file changed", or the user loses their progress.
func (e *Engine) validateResume(ctx context.Context, client *http.Client, t *Task) (bool, error) {
	t.mu.RLock()
	total := t.TotalSize
	ranged := t.Resumable
	url := t.URL
	headers := t.headersCopy()
	chunks := append([]*Chunk(nil), t.Chunks...)
	storedETag := t.ETag
	storedLM := t.LastModified
	savePath := t.SavePath
	t.mu.RUnlock()
	if !ranged || total <= 0 || len(chunks) == 0 {
		return false, nil
	}
	info, err := os.Stat(partPath(savePath))
	if err != nil || info.Size() > total {
		return false, nil
	}
	if sumChunks(chunks) > info.Size() {
		return false, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, nil
	}
	applyHeaders(req, headers)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, err // unreachable server: keep the partial data
	}
	defer func() {
		// Drain the 1-byte payload so the connection goes back to the pool
		// for the chunk workers instead of being torn down.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
		resp.Body.Close()
	}()
	if isTransientStatus(resp.StatusCode) {
		return false, fmt.Errorf("server returned %s", resp.Status)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return false, nil
	}
	_, _, remoteTotal, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || remoteTotal != total {
		return false, nil
	}
	respETag := resp.Header.Get("ETag")
	respLM := resp.Header.Get("Last-Modified")
	if storedETag != "" && respETag != "" && respETag != storedETag {
		return false, nil
	}
	if storedLM != "" && respLM != "" && respLM != storedLM {
		return false, nil
	}
	// Same-size content swaps are invisible on a server that exposes no
	// validators at all — spot-check by refetching the tail of the largest
	// downloaded chunk and comparing it with the bytes on disk.
	if storedETag == "" && storedLM == "" && respETag == "" && respLM == "" {
		matched, serr := sampleMatchesPart(ctx, client, url, headers, savePath, chunks)
		if serr != nil {
			return false, serr
		}
		if !matched {
			return false, nil
		}
	}
	t.mu.Lock()
	if t.ETag == "" {
		t.ETag = respETag
	}
	if t.LastModified == "" {
		t.LastModified = respLM
	}
	// Always take the freshly resolved redirect target: chunk requests go
	// there directly, and a stale signed URL from a previous session would
	// just burn a refresh round-trip.
	if resp.Request != nil && resp.Request.URL != nil {
		t.FinalURL = resp.Request.URL.String()
	}
	t.mu.Unlock()
	return true, nil
}

// sampleMatchesPart refetches the tail of the most-downloaded chunk and
// compares it byte-for-byte with the partial file. Used only when the server
// exposes neither ETag nor Last-Modified. A non-nil error means the check was
// inconclusive (network), which must not be read as a mismatch.
func sampleMatchesPart(ctx context.Context, client *http.Client, url string, headers map[string]string, savePath string, chunks []*Chunk) (bool, error) {
	var c *Chunk
	for _, cc := range chunks {
		if cc != nil && cc.loaded() > 0 && (c == nil || cc.loaded() > c.loaded()) {
			c = cc
		}
	}
	if c == nil {
		return true, nil // nothing downloaded yet, nothing to mismatch
	}
	const sampleSize = int64(16 << 10)
	end := c.Start + c.loaded() - 1
	start := end - sampleSize + 1
	if start < c.Start {
		start = c.Start
	}
	local := make([]byte, end-start+1)
	f, err := os.Open(partPath(savePath))
	if err != nil {
		return false, nil
	}
	defer f.Close()
	if _, err := io.ReadFull(io.NewSectionReader(f, start, int64(len(local))), local); err != nil {
		return false, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return false, nil
	}
	applyHeaders(req, headers)
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", start, end))
	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, err
	}
	defer resp.Body.Close()
	if isTransientStatus(resp.StatusCode) {
		return false, fmt.Errorf("server returned %s", resp.Status)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return false, nil
	}
	if rs, _, _, ok := parseContentRange(resp.Header.Get("Content-Range")); !ok || rs != start {
		return false, nil
	}
	remote := make([]byte, len(local))
	if _, err := io.ReadFull(resp.Body, remote); err != nil {
		if ctx.Err() != nil {
			return false, ctx.Err()
		}
		return false, err
	}
	return string(remote) == string(local), nil
}

func (e *Engine) migrateSegmentsToChunks(t *Task) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.Chunks = make([]*Chunk, 0, len(t.Segments))
	for _, s := range t.Segments {
		if s == nil {
			continue
		}
		t.Chunks = append(t.Chunks, &Chunk{
			Index: s.Index, Start: s.Start, End: s.End, Downloaded: s.loaded(),
		})
	}
}

func (e *Engine) resetPartial(t *Task) {
	removeMeta(t.SavePath)
	removePartial(t.SavePath)
	t.resetTransferState()
}

func sumChunks(chunks []*Chunk) int64 {
	var total int64
	for _, c := range chunks {
		if c != nil {
			total += c.loaded()
		}
	}
	return total
}

// openInitialResponse issues the fast-start request (Range: bytes=0-),
// retrying transient failures — a momentary DNS hiccup or a single 503 must
// not strand the task in an error state. It returns the open response plus the
// cancel func of the request context (used by the stall watchdog to unblock
// body reads).
func (e *Engine) openInitialResponse(ctx context.Context, client *http.Client, url string, headers map[string]string, retries int) (*http.Response, context.CancelFunc, error) {
	if retries < 1 {
		retries = defaultRetries
	}
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		rctx, rcancel := context.WithCancel(ctx)
		req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
		if err != nil {
			rcancel()
			return nil, nil, err
		}
		applyHeaders(req, headers)
		req.Header.Set("Range", "bytes=0-")
		resp, err := client.Do(req)
		if err == nil {
			switch {
			case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusPartialContent:
				return resp, rcancel, nil
			case isTransientStatus(resp.StatusCode):
				last = fmt.Errorf("server returned %s", resp.Status)
				retryAfter := parseRetryAfter(resp.Header.Get("Retry-After"))
				_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				rcancel()
				if attempt < retries {
					delay := retryDelay(attempt)
					if retryAfter > delay {
						delay = retryAfter
						if delay > maxRetryAfterWait {
							delay = maxRetryAfterWait
						}
					}
					if !sleepCtx(ctx, delay) {
						return nil, nil, ctx.Err()
					}
				}
				continue
			default:
				status := resp.Status
				resp.Body.Close()
				rcancel()
				return nil, nil, fmt.Errorf("server returned %s", status)
			}
		}
		rcancel()
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		last = err
		if attempt < retries && !sleepCtx(ctx, retryDelay(attempt)) {
			return nil, nil, ctx.Err()
		}
	}
	return nil, nil, last
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// confirmRangeSupport verifies Accept-Ranges with a real bytes=0-0 probe.
// Servers legally answer 200 to "bytes=0-" (it means the whole file) while
// honouring real subranges — without this probe those downloads would lose
// both multi-connection transfer and resume.
func confirmRangeSupport(ctx context.Context, client *http.Client, url string, headers map[string]string, total int64, etag string) bool {
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(pctx, http.MethodGet, url, nil)
	if err != nil {
		return false
	}
	applyHeaders(req, headers)
	req.Header.Set("Range", "bytes=0-0")
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer func() {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64))
		resp.Body.Close()
	}()
	if resp.StatusCode != http.StatusPartialContent {
		return false
	}
	rs, _, n, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || rs != 0 || (n > 0 && n != total) {
		return false
	}
	if etag != "" && resp.Header.Get("ETag") != "" && resp.Header.Get("ETag") != etag {
		return false
	}
	return true
}

func (e *Engine) fastStartV2(ctx context.Context, client *http.Client, t *Task, w *fileWriter, m *managed) error {
	headers := t.headersCopy()
	url := t.URL

	// tctx cancels every sibling worker (and unblocks the streamer's body
	// read) the moment one of them hits a fatal error, so no bandwidth is
	// wasted downloading data the failed task would throw away.
	tctx, tcancel := context.WithCancel(ctx)
	defer tcancel()

	retries := e.transferOptions(responseIdentity{}).Retries
	resp, respCancel, err := e.openInitialResponse(tctx, client, url, headers, retries)
	if err != nil {
		return err
	}

	total := int64(-1)
	ranged := false
	if resp.StatusCode == http.StatusPartialContent {
		rs, re, n, ok := parseContentRange(resp.Header.Get("Content-Range"))
		if !ok {
			resp.Body.Close()
			return fmt.Errorf("missing or invalid Content-Range in %s response", resp.Status)
		}
		if rs != 0 {
			// Bytes would land at the wrong offsets — refuse instead of
			// producing a silently corrupt file.
			resp.Body.Close()
			return fmt.Errorf("server answered bytes=0- with range starting at %d", rs)
		}
		if n > 0 {
			total = n
			ranged = true
		} else if re >= 0 {
			// "bytes 0-x/*": the server honoured the open range but doesn't
			// know the total — the range end still tells us the full size.
			total = re + 1
			ranged = true
		}
	} else { // 200 OK: the server ignored the open range
		if resp.ContentLength > 0 {
			total = resp.ContentLength
		}
		if total > 0 && strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes") {
			probeURL := url
			if resp.Request != nil && resp.Request.URL != nil {
				probeURL = resp.Request.URL.String()
			}
			ranged = confirmRangeSupport(tctx, client, probeURL, headers, total, resp.Header.Get("ETag"))
		}
	}
	if total <= 0 {
		ranged = false
	}

	var plan transferPlan
	if ranged {
		plan = buildTransferPlan(total, t.Connections)
	} else {
		plan = transferPlan{workers: 1, chunks: []*Chunk{{Index: 0, Start: 0, End: total - 1}}, lanes: buildWorkerLanes(total, 1)}
	}
	identity := responseIdentity{
		ETag:         resp.Header.Get("ETag"),
		LastModified: resp.Header.Get("Last-Modified"),
		TotalSize:    total,
	}
	if resp.Request != nil && resp.Request.URL != nil {
		identity.FinalURL = resp.Request.URL.String()
	}

	t.mu.Lock()
	t.TotalSize = total
	t.Resumable = ranged
	t.ETag = identity.ETag
	t.LastModified = identity.LastModified
	t.FinalURL = identity.FinalURL
	if t.MIME == "" {
		t.MIME = resp.Header.Get("Content-Type")
	}
	if t.Filename == "" {
		t.Filename = resolveFilename(resp, url)
	}
	t.Chunks = plan.chunks
	t.Segments = plan.lanes
	t.mu.Unlock()

	t.setStatus(StatusDownloading, "")
	e.emitManaged(m)

	var progress int64
	stop := make(chan struct{})
	go e.reportProgress(t, &progress, stop)
	go e.persistProgress(t, stop)
	opts := e.transferOptions(identity)

	if !ranged {
		err := e.streamNoRangeWithRetry(ctx, client, resp, respCancel, url, headers, plan.chunks[0], plan.lanes[0], w, &progress, opts, total)
		close(stop)
		t.recalcDownloaded()
		_ = writeMeta(t)
		return err
	}

	holder := newURLHolder(identity.FinalURL, url, headers)
	q := newChunkQueue(plan.chunks, plan.workers, true)
	errCh := make(chan error, plan.workers+1)
	var wg sync.WaitGroup

	// The fast-start connection already has the whole file streaming on it —
	// keep it: worker 0 extends it through the contiguous chunks of its own
	// region, then joins the others in the steal pool. The remaining workers
	// each open a ranged connection at the head of their own region, so all
	// connections are live and spread across the file from the first second.
	wg.Add(1)
	go func() {
		defer wg.Done()
		if err := streamChunks(tctx, resp, respCancel, client, holder, headers, q, plan.lanes[0], w, &progress, opts); err != nil {
			errCh <- err
			tcancel()
			return
		}
		e.consumeChunks(tctx, client, holder, headers, q, 0, plan.lanes[0], w, &progress, opts, errCh, tcancel)
	}()

	for i := 1; i < plan.workers; i++ {
		worker, lane := i, plan.lanes[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.consumeChunks(tctx, client, holder, headers, q, worker, lane, w, &progress, opts, errCh, tcancel)
		}()
	}

	wg.Wait()
	close(stop)
	close(errCh)
	t.recalcDownloaded()
	_ = writeMeta(t)

	if err := ctx.Err(); err != nil {
		return err
	}
	return pickTransferError(ctx, errCh)
}

// streamNoRangeWithRetry downloads a file that cannot be split into ranges on
// a single connection, restarting from scratch (the only option) on transient
// mid-stream failures instead of stranding the task.
func (e *Engine) streamNoRangeWithRetry(
	ctx context.Context,
	client *http.Client,
	first *http.Response,
	firstCancel context.CancelFunc,
	url string,
	headers map[string]string,
	chunk *Chunk,
	lane *Segment,
	w *fileWriter,
	progress *int64,
	opts transferOptions,
	total int64,
) error {
	retries := opts.Retries
	if retries < 1 {
		retries = defaultRetries
	}
	resp, cancel := first, firstCancel
	var last error
	for attempt := 0; attempt <= retries; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}
		if resp == nil {
			rctx, rcancel := context.WithCancel(ctx)
			req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
			if err != nil {
				rcancel()
				return err
			}
			applyHeaders(req, headers)
			r2, err := client.Do(req)
			if err != nil {
				rcancel()
				if ctx.Err() != nil {
					return ctx.Err()
				}
				last = err
				if attempt < retries && !sleepCtx(ctx, retryDelay(attempt)) {
					return ctx.Err()
				}
				continue
			}
			if r2.StatusCode != http.StatusOK {
				status := r2.Status
				transient := isTransientStatus(r2.StatusCode)
				_, _ = io.Copy(io.Discard, io.LimitReader(r2.Body, 4096))
				r2.Body.Close()
				rcancel()
				if !transient {
					return fmt.Errorf("server returned %s", status)
				}
				last = fmt.Errorf("server returned %s", status)
				if attempt < retries && !sleepCtx(ctx, retryDelay(attempt)) {
					return ctx.Err()
				}
				continue
			}
			// The new stream starts over from byte 0; wipe the previous
			// partial bytes so nothing stale survives underneath it.
			if err := w.Truncate(0); err != nil {
				r2.Body.Close()
				rcancel()
				return err
			}
			// Note: the shared progress counter is deliberately NOT reset —
			// it only feeds the speed readout; Downloaded is recomputed from
			// the chunk, which does restart at zero.
			chunk.reset()
			if lane != nil {
				lane.reset()
			}
			resp, cancel = r2, rcancel
		}
		sopts := opts
		sopts.Cancel = cancel
		err := func() error {
			defer cancel()
			return streamOpenResponse(ctx, resp, chunk, lane, w, progress, sopts, total > 0)
		}()
		resp, cancel = nil, nil
		if err == nil {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		last = err
		if attempt < retries && !sleepCtx(ctx, retryDelay(attempt)) {
			return ctx.Err()
		}
	}
	return fmt.Errorf("download failed after retries: %w", last)
}

func (e *Engine) transferV2(ctx context.Context, client *http.Client, t *Task, w *fileWriter) error {
	t.mu.RLock()
	chunks := append([]*Chunk(nil), t.Chunks...)
	lanes := append([]*Segment(nil), t.Segments...)
	headers := t.headersCopy()
	url := t.URL
	identity := responseIdentity{ETag: t.ETag, LastModified: t.LastModified, FinalURL: t.FinalURL, TotalSize: t.TotalSize}
	t.mu.RUnlock()
	if len(chunks) == 0 {
		return fmt.Errorf("no resumable chunks")
	}
	if len(lanes) == 0 {
		lanes = buildWorkerLanes(identity.TotalSize, smartConnections(identity.TotalSize, DefaultConnections))
		t.mu.Lock()
		t.Segments = lanes
		t.mu.Unlock()
	}

	var progress int64
	stop := make(chan struct{})
	go e.reportProgress(t, &progress, stop)
	go e.persistProgress(t, stop)

	tctx, tcancel := context.WithCancel(ctx)
	defer tcancel()
	holder := newURLHolder(identity.FinalURL, url, headers)
	workers := len(lanes)
	q := newChunkQueue(chunks, workers, false)
	errCh := make(chan error, workers)
	opts := e.transferOptions(identity)
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		worker, lane := i, lanes[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			e.consumeChunks(tctx, client, holder, headers, q, worker, lane, w, &progress, opts, errCh, tcancel)
		}()
	}

	wg.Wait()
	close(stop)
	close(errCh)
	t.recalcDownloaded()
	_ = writeMeta(t)
	if err := ctx.Err(); err != nil {
		return err
	}
	return pickTransferError(ctx, errCh)
}

func (e *Engine) consumeChunks(
	ctx context.Context,
	client *http.Client,
	holder *urlHolder,
	headers map[string]string,
	q *chunkQueue,
	worker int,
	lane *Segment,
	w *fileWriter,
	progress *int64,
	opts transferOptions,
	errCh chan<- error,
	cancel context.CancelFunc,
) {
	for {
		c := q.nextChunk(worker)
		if c == nil {
			return
		}
		if err := downloadChunkWithRetry(ctx, client, holder, headers, c, lane, w, progress, opts); err != nil {
			errCh <- err
			// The task is going to fail; stop the siblings from spending time
			// and bandwidth on data that would be thrown away.
			if cancel != nil {
				cancel()
			}
			return
		}
	}
}

// pickTransferError chooses the most meaningful error out of what the workers
// reported: an identity change beats everything (it triggers the transparent
// restart), and a real failure beats the context.Canceled noise the sibling
// cancellation produces.
func pickTransferError(taskCtx context.Context, errCh <-chan error) error {
	var real, fallback error
	for err := range errCh {
		if err == nil {
			continue
		}
		if isIdentityChanged(err) {
			return err
		}
		if errors.Is(err, context.Canceled) && taskCtx.Err() == nil {
			if fallback == nil {
				fallback = err
			}
			continue
		}
		if real == nil {
			real = err
		}
	}
	if real != nil {
		return real
	}
	return fallback
}

func (e *Engine) transferOptions(id responseIdentity) transferOptions {
	e.mu.Lock()
	opts := transferOptions{
		Retries:      e.cfg.Retries,
		StallTimeout: e.cfg.StallTimeout,
		Limiter:      e.limiter,
		Connections:  e.connections,
		Identity:     id,
	}
	e.mu.Unlock()
	return opts
}

func (e *Engine) persistProgress(t *Task, stop <-chan struct{}) {
	e.mu.Lock()
	interval := e.cfg.MetaInterval
	e.mu.Unlock()
	if interval <= 0 {
		interval = defaultMetaInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			if e.isActive(t.ID) {
				_ = writeMeta(t)
				e.cfg.OnPersist(t.Record())
			}
		}
	}
}

// fastStart downloads a fresh task with no pre-flight probe: it opens a single
// open-ended request (Range: bytes=0-) whose response headers reveal the size and
// range support, and whose body is streamed straight to disk as segment 0. The
// moment those headers arrive we know the total size, flip to Downloading and —
// if the server supports ranges — fan out the remaining segments on their own
// connections. This removes the dead round-trip that made downloads look stalled
// for the first few seconds, so they start at full speed like IDM.
func (e *Engine) fastStart(ctx context.Context, client *http.Client, t *Task, w *fileWriter, m *managed) error {
	return e.fastStartV2(ctx, client, t, w, m)
}

func (e *Engine) fastStartLegacy(ctx context.Context, client *http.Client, t *Task, w *fileWriter, m *managed) error {
	headers := t.headersCopy()
	url := t.URL

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	applyHeaders(req, headers)
	req.Header.Set("Range", "bytes=0-")

	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	// resp.Body is handed to the segment-0 streamer, which closes it.

	if resp.StatusCode >= 400 {
		resp.Body.Close()
		return fmt.Errorf("server returned %s", resp.Status)
	}

	total := int64(-1)
	ranged := false
	if resp.StatusCode == http.StatusPartialContent {
		ranged = true
		if n := parseContentRangeTotal(resp.Header.Get("Content-Range")); n > 0 {
			total = n
		} else if resp.ContentLength > 0 {
			total = resp.ContentLength
		}
	} else { // 200 OK: server ignored the range request
		if strings.EqualFold(resp.Header.Get("Accept-Ranges"), "bytes") {
			ranged = true
		}
		if resp.ContentLength > 0 {
			total = resp.ContentLength
		}
	}
	// Without a known size we cannot split into ranges; stream on one connection.
	if total <= 0 {
		ranged = false
	}

	t.mu.Lock()
	t.TotalSize = total
	t.Resumable = ranged
	if t.MIME == "" {
		t.MIME = resp.Header.Get("Content-Type")
	}
	if t.Filename == "" {
		t.Filename = resolveFilename(resp, url)
	}
	conns := t.Connections
	if !ranged {
		conns = 1
	}
	t.Segments = buildSegments(total, conns)
	segs := t.Segments
	t.mu.Unlock()

	// Size is known now, so progress, ETA and per-thread bars come alive at once.
	t.setStatus(StatusDownloading, "")
	e.emitManaged(m)

	var progress int64
	var wg sync.WaitGroup
	errCh := make(chan error, len(segs))

	// Segment 0 consumes the already-open response body — bytes are flowing from
	// the first packet, with no extra handshake.
	wg.Add(1)
	go func(s *Segment) {
		defer wg.Done()
		if err := streamSegment(ctx, resp, s, w, ranged, &progress); err != nil {
			errCh <- err
		}
	}(segs[0])

	// Remaining segments open their own ranged connections in parallel.
	for i := 1; i < len(segs); i++ {
		wg.Add(1)
		go func(s *Segment) {
			defer wg.Done()
			if err := downloadSegment(ctx, client, url, headers, s, w, ranged, &progress); err != nil {
				errCh <- err
			}
		}(segs[i])
	}

	stop := make(chan struct{})
	go e.reportProgress(t, &progress, stop)

	wg.Wait()
	close(stop)
	close(errCh)

	t.recalcDownloaded()
	_ = writeMeta(t)

	if err := ctx.Err(); err != nil {
		return err
	}
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// transfer runs the segment workers concurrently and reports progress until all
// complete, the context is cancelled, or one fails.
func (e *Engine) transfer(ctx context.Context, client *http.Client, t *Task, w *fileWriter) error {
	return e.transferV2(ctx, client, t, w)
}

func (e *Engine) transferLegacy(ctx context.Context, client *http.Client, t *Task, w *fileWriter) error {
	t.mu.RLock()
	segs := t.Segments
	ranged := t.Resumable
	headers := t.headersCopy()
	url := t.URL
	t.mu.RUnlock()

	var progress int64
	atomic.StoreInt64(&progress, 0)

	var wg sync.WaitGroup
	errCh := make(chan error, len(segs))
	for _, seg := range segs {
		wg.Add(1)
		go func(s *Segment) {
			defer wg.Done()
			if err := downloadSegment(ctx, client, url, headers, s, w, ranged, &progress); err != nil {
				errCh <- err
			}
		}(seg)
	}

	stop := make(chan struct{})
	go e.reportProgress(t, &progress, stop)

	wg.Wait()
	close(stop)
	close(errCh)

	t.recalcDownloaded()
	_ = writeMeta(t)

	if err := ctx.Err(); err != nil {
		return err
	}
	for err := range errCh {
		if err != nil {
			return err
		}
	}
	return nil
}

// reportProgress periodically computes speed and emits throttled updates. It
// emits one update immediately so the UI (total bar, per-thread bars and speed)
// comes alive the instant the transfer starts instead of after the first tick.
func (e *Engine) reportProgress(t *Task, progress *int64, stop <-chan struct{}) {
	const interval = 250 * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	var lastBytes int64
	lastTime := time.Now()
	// A lighter EMA (higher alpha) so the displayed speed converges to the real
	// rate in ~1s instead of feeling like it slowly creeps up over many seconds.
	const alpha = 0.6

	t.recalcDownloaded()
	if e.isActive(t.ID) {
		e.cfg.OnUpdate(t.Snapshot()) // immediate first paint
	}

	for {
		select {
		case <-stop:
			return
		case now := <-ticker.C:
			cur := atomic.LoadInt64(progress)
			dt := now.Sub(lastTime).Seconds()
			if dt <= 0 {
				continue
			}
			instant := float64(cur-lastBytes) / dt
			lastBytes = cur
			lastTime = now

			t.mu.Lock()
			// Snap to the real rate on the first sample so the readout shows the
			// true speed immediately instead of creeping up from zero; smooth
			// thereafter.
			if t.Speed == 0 {
				t.Speed = int64(instant)
			} else {
				t.Speed = int64(alpha*instant + (1-alpha)*float64(t.Speed))
			}
			t.mu.Unlock()
			t.recalcDownloaded()
			if e.isActive(t.ID) {
				e.cfg.OnUpdate(t.Snapshot())
			}
		}
	}
}

func (e *Engine) fail(t *Task, ctx context.Context, err error) {
	t.mu.Lock()
	t.Speed = 0
	t.mu.Unlock()
	if ctx.Err() != nil {
		// Cancellation comes from the user (Pause/Remove/Shutdown), which has
		// already set the desired status (Paused, or Queued if resumed mid-unwind).
		// Don't clobber it — just persist the flushed progress.
	} else {
		t.setStatus(StatusError, err.Error())
	}
	e.emitIfActive(t)
}

// emit pushes both a UI update and a durable persist for a task.
func (e *Engine) emit(t *Task) {
	e.cfg.OnUpdate(t.Snapshot())
	e.cfg.OnPersist(t.Record())
}

func (e *Engine) emitManaged(m *managed) {
	e.mu.Lock()
	removed := m.removed
	e.mu.Unlock()
	if removed {
		return
	}
	e.emit(m.task)
}

func (e *Engine) emitIfActive(t *Task) {
	if !e.isActive(t.ID) {
		return
	}
	e.emit(t)
}

func (e *Engine) isActive(id string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	m := e.tasks[id]
	return m != nil && !m.removed
}

func (t *Task) headersCopy() map[string]string {
	t.mu.RLock()
	defer t.mu.RUnlock()
	if t.Headers == nil {
		return nil
	}
	cp := make(map[string]string, len(t.Headers))
	for k, v := range t.Headers {
		cp[k] = v
	}
	return cp
}
