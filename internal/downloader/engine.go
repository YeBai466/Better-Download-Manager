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
	AdaptiveConnections bool
	MaxConcurrent       int                               // max simultaneously downloading tasks
	MaxConnections      int                               // max simultaneous HTTP transfers across all tasks
	SpeedLimit          int64                             // global bytes/sec, 0 = unlimited
	Retries             int                               // retries per ranged chunk
	StallTimeout        time.Duration                     // no-progress timeout per response
	MetaInterval        time.Duration                     // periodic resume-state flush interval
	ClientFactory       func(proxy.Settings) *http.Client // builds the HTTP client (proxy-aware)
	OnUpdate            func(info TaskInfo)               // throttled progress + status changes
	Persist             func(Record) error
	RemoveRecord        func(string) error
	OnPersist           func(rec Record) // legacy persistence callback; prefer Persist to report failures
	OnRemoved           func(id string)  // task removed
}

// Engine manages the task queue, scheduling and lifecycle of downloads.
type Engine struct {
	cfg Config

	mu          sync.Mutex
	tasks       map[string]*managed
	order       []string
	activeCount int
	closed      bool
	wg          sync.WaitGroup
	emitMu      sync.Mutex
	retired     map[string]bool

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
		retired:     map[string]bool{},
	}
}

// ErrNotFound is returned when an operation references an unknown task id.
var ErrNotFound = errors.New("task not found")

// UpdateRuntime applies settings that can change while the app is running.
// Speed limits apply to in-flight transfers immediately (shared limiter); a
// the shared connection budget is resized in place without losing active leases.
func (e *Engine) UpdateRuntime(rt RuntimeConfig) {
	rt = normalizeRuntimeConfig(rt)
	e.mu.Lock()
	e.cfg.MaxConcurrent = rt.MaxConcurrent
	e.cfg.MaxConnections = rt.MaxConnections
	e.cfg.SpeedLimit = rt.SpeedLimit
	e.cfg.Retries = rt.Retries
	e.cfg.StallTimeout = rt.StallTimeout
	e.cfg.MetaInterval = rt.MetaInterval
	e.cfg.AdaptiveConnections = rt.AdaptiveConnections
	e.connections.SetLimit(rt.MaxConnections)
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
	conns = min(conns, 32)
	t := &Task{
		ID:          opts.ID,
		URL:         opts.URL,
		Filename:    opts.Filename,
		SavePath:    opts.SavePath,
		Category:    opts.Category,
		TotalSize:   -1,
		Connections: conns,
		Headers:     cloneHeaders(opts.Headers),
		Proxy:       opts.Proxy,
		Status:      StatusQueued,
		CreatedAt:   time.Now(),
	}

	if !opts.AutoStart {
		t.Status = StatusPaused
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
	if err := e.emit(t); err != nil {
		e.mu.Lock()
		delete(e.tasks, t.ID)
		for i, id := range e.order {
			if id == t.ID {
				e.order = append(e.order[:i], e.order[i+1:]...)
				break
			}
		}
		e.mu.Unlock()
		e.cfg.OnRemoved(t.ID)
		return TaskInfo{}, err
	}
	if opts.AutoStart {
		e.schedule()
	}
	return t.Snapshot(), nil
}

// Restore re-registers a persisted task (e.g. on startup) without auto-starting.
func (e *Engine) Restore(t *Task) {
	if t.Status == StatusDownloading || t.Status == StatusConnecting || t.Status == StatusQueued {
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
	if !ok {
		e.mu.Unlock()
		return ErrNotFound
	}
	if e.closed {
		e.mu.Unlock()
		return errors.New("engine closed")
	}
	switch m.task.getStatus() {
	case StatusDownloading, StatusConnecting, StatusCompleted, StatusQueued:
		e.mu.Unlock()
		return nil
	}
	m.task.setStatus(StatusQueued, "")
	e.mu.Unlock()
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
	if !ok {
		e.mu.Unlock()
		return ErrNotFound
	}
	switch m.task.getStatus() {
	case StatusQueued, StatusConnecting, StatusDownloading:
		if m.cancel != nil {
			m.cancel()
		}
		m.task.mu.Lock()
		m.task.Status, m.task.Speed = StatusPaused, 0
		m.task.mu.Unlock()
		m.task.recalcDownloaded()
	default:
		e.mu.Unlock()
		return nil
	}
	running := m.running
	e.mu.Unlock()
	// Worker shutdown checkpoints the progress; UI feedback never waits for disk.
	e.emitUpdate(m.task)
	if !running {
		e.mu.Lock()
		if !e.closed {
			e.wg.Add(1)
			go func() { defer e.wg.Done(); e.emitManaged(m) }()
		}
		e.mu.Unlock()
	}
	return nil
}

// Remove cancels (if running) and deletes a task. It returns immediately; the
// worker is cancelled and file cleanup (when deleteFile is set) runs in the
// background once the worker has fully stopped, so the UI updates instantly.
func (e *Engine) Remove(id string, deleteFile bool) error {
	e.emitMu.Lock()
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		e.emitMu.Unlock()
		return errors.New("engine closed")
	}
	m, ok := e.tasks[id]
	if !ok {
		e.mu.Unlock()
		e.emitMu.Unlock()
		return ErrNotFound
	}
	if e.cfg.RemoveRecord != nil {
		e.mu.Unlock()
		err := e.cfg.RemoveRecord(id)
		e.mu.Lock()
		if err != nil {
			e.mu.Unlock()
			e.emitMu.Unlock()
			return err
		}
	}
	m.removed = true
	delete(e.tasks, id)
	e.retired[m.task.SavePath] = true
	for i, oid := range e.order {
		if oid == id {
			e.order = append(e.order[:i], e.order[i+1:]...)
			break
		}
	}
	if m.cancel != nil {
		m.cancel()
	}
	done, running := m.done, m.running
	e.wg.Add(1)
	e.mu.Unlock()
	e.cfg.OnRemoved(id)
	e.emitMu.Unlock()
	go func() {
		defer e.wg.Done()
		if running {
			<-done
		}
		if deleteFile {
			removeFinal(m.task.SavePath)
			removeMeta(m.task.SavePath)
			removePartial(m.task.SavePath)
		}
		e.mu.Lock()
		delete(e.retired, m.task.SavePath)
		e.mu.Unlock()
	}()
	return nil
}

// PathOccupied includes paths awaiting asynchronous deletion.
func (e *Engine) PathOccupied(path string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	for retired := range e.retired {
		if strings.EqualFold(retired, path) {
			return true
		}
	}
	for _, m := range e.tasks {
		if strings.EqualFold(m.task.SavePath, path) {
			return true
		}
	}
	return false
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
	e.emitMu.Lock()
	e.mu.Lock()
	e.closed = true
	for _, m := range e.tasks {
		if m.cancel != nil {
			m.cancel()
		}
		if m.task.getStatus() != StatusCompleted && m.task.getStatus() != StatusError {
			m.task.setStatus(StatusPaused, "")
		}
	}
	e.mu.Unlock()
	e.emitMu.Unlock()
	e.wg.Wait()
	// Serialize with the last callback before the owner closes its database.
	e.emitMu.Lock()
	e.emitMu.Unlock()
}

// PauseAllAndWait leaves the engine reusable after flushing every active run.
func (e *Engine) PauseAllAndWait() {
	e.mu.Lock()
	var done []chan struct{}
	for _, m := range e.tasks {
		if m.task.getStatus() == StatusQueued || m.running {
			m.task.setStatus(StatusPaused, "")
			if m.cancel != nil {
				m.cancel()
			}
			if m.running {
				done = append(done, m.done)
			}
		}
	}
	e.mu.Unlock()
	for _, ch := range done {
		<-ch
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
	ctx, cancel := context.WithCancel(context.WithValue(context.Background(), connectionOwnerKey{}, m.task.ID))
	m.cancel = cancel
	m.done = make(chan struct{})
	m.running = true
	e.activeCount++
	e.wg.Add(1)
	go e.run(ctx, m, m.done)
}

// run performs the full download for one task and reschedules on completion.
func (e *Engine) run(ctx context.Context, m *managed, done chan struct{}) {
	defer e.wg.Done()
	t := m.task
	defer func() {
		e.mu.Lock()
		m.running = false
		m.cancel = nil
		e.activeCount--
		close(done)
		e.mu.Unlock()
		e.schedule()
	}()

	// Queued->Connecting is a CAS: a Pause that landed between scheduling and
	// this goroutine starting must win, not be silently clobbered.
	e.mu.Lock()
	if ctx.Err() != nil || !t.casStatus(StatusQueued, StatusConnecting) {
		e.mu.Unlock()
		return
	}
	e.mu.Unlock()
	client := e.budgetClient(e.cfg.ClientFactory(t.Proxy))
	e.emitManaged(m)

	// One transparent restart when the remote content changed mid-transfer
	// (validator/size mismatch): throw the mismatched partial data away and
	// download the new file instead of stranding the task in an error state.
	for attempt := 0; ; attempt++ {
		err := e.runOnce(ctx, client, t, m)
		if err == nil {
			t.recalcDownloaded()
			t.setStatus(StatusCompleted, "")
			// Retain the commit receipt until the database acknowledges completion.
			if err := e.emit(t); err == nil {
				removeMeta(t.SavePath)
			}
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
	meta, _ := readMeta(t.SavePath)
	if recoverFinalized(t, meta) {
		return nil
	}
	// Resume path: a task we already know the layout of (paused→resumed in the
	// same session, restored from the DB, or with a sidecar .bdmeta) skips
	// probing and just refetches the remaining ranges. Fresh tasks take the
	// fast-start path, which starts streaming bytes on the very first
	// connection. A network-level validation failure aborts WITHOUT resetting:
	// a blip at resume time must not cost the user their partial data.
	resumable, rerr := e.loadResume(ctx, client, t, meta)
	if rerr != nil {
		return rerr
	}

	w, err := openPartFile(t.SavePath)
	if err != nil {
		return err
	}
	t.metaMu.Lock()
	t.writer = w
	t.metaMu.Unlock()
	defer func() { t.metaMu.Lock(); t.writer = nil; t.metaMu.Unlock(); w.Close() }()

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
		if err := e.markDownloading(ctx, m); err != nil {
			w.Close()
			return err
		}
		e.emitManaged(m)
		xferErr = e.transferV2(ctx, client, t, w)
	} else {
		xferErr = e.fastStartV2(ctx, client, t, w, m)
	}
	if xferErr != nil {
		if err := writeMeta(t); err != nil {
			return fmt.Errorf("%v; checkpoint: %w", xferErr, err)
		}
		return xferErr
	}
	// Completeness gate. Workers only return nil once their chunk is full, so
	// reaching here should imply a whole file — but that is derived, not
	// checked. Verify it explicitly: a silently truncated or hole-punched
	// "completed" file is the worst failure this engine can produce, and the
	// check costs one in-memory pass plus a Stat.
	if err := verifyComplete(t); err != nil {
		if checkpointErr := writeMeta(t); checkpointErr != nil {
			return fmt.Errorf("%v; checkpoint: %w", err, checkpointErr)
		}
		w.synced = err == nil
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
	info, err := os.Stat(partPath(savePath))
	if err != nil {
		return err
	}
	if info.Size() != total {
		return fmt.Errorf("partial file is %d bytes, expected %d", info.Size(), total)
	}
	return nil
}

// loadResume validates saved byte ranges before allowing a ranged resume.
// A non-nil error means validation could not be performed (network failure) —
// the caller must abort without touching the partial data, so the user can
// simply retry once the connection is back.
func (e *Engine) loadResume(ctx context.Context, client *http.Client, t *Task, meta *metaFile) (bool, error) {
	t.mu.RLock()
	hasChunks := len(t.Chunks) > 0
	hasSegments := len(t.Segments) > 0
	ranged := t.Resumable
	t.mu.RUnlock()

	if meta != nil {
		t.mu.RLock()
		seq := t.CheckpointSeq
		t.mu.RUnlock()
		if meta.CheckpointSeq > seq {
			ok, err := e.applyMetaIfSafe(ctx, client, t, meta)
			if err != nil || ok {
				t.recalcDownloaded()
				return ok, err
			}
		}
	}
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

	if meta != nil && meta.TotalSize > 0 {
		ok, verr := e.applyMetaIfSafe(ctx, client, t, meta)
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
	t.CheckpointSeq = m.CheckpointSeq
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
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	t.mu.RLock()
	total := t.TotalSize
	ranged := t.Resumable
	url := t.URL
	headers := cloneHeaders(t.Headers)
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
	if !validChunkLayout(chunks, total, info.Size()) {
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
	if isTransientStatus(resp.StatusCode) || isAuthExpiryStatus(resp.StatusCode) {
		return false, fmt.Errorf("server returned %s", resp.Status)
	}
	if resp.StatusCode != http.StatusPartialContent {
		return false, nil
	}
	rs, re, remoteTotal, ok := parseContentRange(resp.Header.Get("Content-Range"))
	if !ok || rs != 0 || re != 0 || remoteTotal != total {
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
	strongMatch := storedETag != "" && !strings.HasPrefix(strings.ToLower(storedETag), "w/") && storedETag == respETag
	dateMatch := storedLM != "" && storedLM == respLM
	if !strongMatch && !dateMatch {
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
	if isTransientStatus(resp.StatusCode) || isAuthExpiryStatus(resp.StatusCode) {
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
	t.metaMu.Lock()
	t.checkpoint = nil
	t.metaMu.Unlock()
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
	headers := cloneHeaders(t.Headers)
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

	defer func() { resp.Body.Close(); respCancel() }()

	total := int64(-1)
	ranged := false
	if resp.StatusCode == http.StatusPartialContent {
		rs, _, n, ok := parseContentRange(resp.Header.Get("Content-Range"))
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
			if err := validatePartialResponse(resp, 0, n-1, responseIdentity{TotalSize: n}); err != nil {
				return err
			}
			total = n
			ranged = true
		} else {
			resp.Body.Close()
			respCancel()
			resp, respCancel, err = e.openFullResponse(tctx, client, url, headers)
			if err != nil {
				return err
			}
			total = resp.ContentLength

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
			etag := resp.Header.Get("ETag")
			resp.Body.Close()
			respCancel()
			ranged = confirmRangeSupport(tctx, client, probeURL, headers, total, etag)
			resp, respCancel, err = e.openFullResponse(tctx, client, url, headers)
			if err != nil {
				return err
			}
			if resp.ContentLength != total || (etag != "" && resp.Header.Get("ETag") != etag) {
				ranged = false
				total = resp.ContentLength
			}
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
	if ranged && e.adaptiveEnabled() && total >= smallFileThreshold {
		plan.lanes = buildWorkerLanes(total, min(t.Connections, 32))
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

	if err := e.markDownloading(ctx, m); err != nil {
		return err
	}
	e.emitManaged(m)

	var progress int64
	stop := make(chan struct{})
	stopProgress := e.startProgress(t, w, &progress, stop)
	opts := e.transferOptions(identity)

	if !ranged {
		err := e.streamNoRangeWithRetry(t, ctx, client, resp, respCancel, url, headers, plan.chunks[0], plan.lanes[0], w, &progress, opts, total)
		close(stop)
		stopProgress()
		t.recalcDownloaded()
		if checkpointErr := writeMeta(t); checkpointErr != nil {
			return fmt.Errorf("%v; checkpoint: %w", err, checkpointErr)
		}
		return err
	}

	holder := newURLHolder(identity.FinalURL, url, headers)
	q := newChunkQueue(plan.chunks, plan.workers, true)
	err = e.runWorkers(tctx, client, holder, headers, q, plan.lanes, t, w, &progress, opts, plan.workers,
		func(workerCtx context.Context, local transferOptions) error {
			return streamChunks(workerCtx, resp, respCancel, client, holder, headers, q, plan.lanes[0], w, &progress, local)
		}, respCancel)

	close(stop)
	stopProgress()
	t.recalcDownloaded()
	if err := writeMeta(t); err != nil {
		return err
	}
	w.synced = true

	if err := ctx.Err(); err != nil {
		return err
	}
	return err
}

// streamNoRangeWithRetry downloads a file that cannot be split into ranges on
// a single connection, restarting from scratch (the only option) on transient
// mid-stream failures instead of stranding the task.
func (e *Engine) streamNoRangeWithRetry(
	t *Task,
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
			total = r2.ContentLength
			t.mu.Lock()
			chunk.End = total - 1
			if lane != nil {
				lane.End = chunk.End
			}
			t.TotalSize, t.ETag, t.LastModified = total, r2.Header.Get("ETag"), r2.Header.Get("Last-Modified")
			t.mu.Unlock()
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
			t.mu.Lock()
			if total < 0 {
				t.TotalSize = chunk.loaded()
				chunk.End = chunk.loaded() - 1
				if lane != nil {
					lane.End = chunk.End
				}
			}
			t.mu.Unlock()
			return nil
		}
		if isFatalDownloadError(err) {
			return err
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
	headers := cloneHeaders(t.Headers)
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
	stopProgress := e.startProgress(t, w, &progress, stop)

	tctx, tcancel := context.WithCancel(ctx)
	defer tcancel()
	holder := newURLHolder(identity.FinalURL, url, headers)
	workers := len(lanes)
	q := newChunkQueue(chunks, workers, false)
	opts := e.transferOptions(identity)
	err := e.runWorkers(tctx, client, holder, headers, q, lanes, t, w, &progress, opts, min(workers, smartConnections(identity.TotalSize, t.Connections)), nil, nil)

	close(stop)
	stopProgress()
	t.recalcDownloaded()
	if err := writeMeta(t); err != nil {
		return err
	}
	w.synced = true
	if err := ctx.Err(); err != nil {
		return err
	}
	return err
}

func (e *Engine) transferOptions(id responseIdentity) transferOptions {
	e.mu.Lock()
	opts := transferOptions{
		Retries:      e.cfg.Retries,
		StallTimeout: e.cfg.StallTimeout,
		Limiter:      e.limiter,
		Connections:  e.connections,
		Identity:     id,
		Failures:     &atomic.Int64{},
	}
	e.mu.Unlock()
	return opts
}

func (e *Engine) persistProgress(t *Task, stop <-chan struct{}, started time.Time) {
	e.mu.Lock()
	interval := e.cfg.MetaInterval
	e.mu.Unlock()
	if interval <= 0 {
		interval = defaultMetaInterval
	}
	timer := time.NewTimer(max(time.Millisecond, interval-time.Since(started)))
	defer timer.Stop()
	for {
		select {
		case <-stop:
			return
		case <-timer.C:
			if e.isActive(t.ID) {
				e.persistTask(t)
			}
			timer.Reset(interval)
		}
	}
}

// reportProgress periodically computes speed and emits throttled updates. It
// emits one update immediately so the UI (total bar, per-thread bars and speed)
// comes alive the instant the transfer starts instead of after the first tick.
func (e *Engine) reportProgress(t *Task, progress *int64, stop <-chan struct{}, started time.Time) {
	const interval = 250 * time.Millisecond
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	lastBytes := atomic.LoadInt64(progress)
	lastTime := time.Now()
	t.mu.Lock()
	if t.Status == StatusDownloading && lastTime.After(started) {
		t.Speed = int64(float64(lastBytes) / lastTime.Sub(started).Seconds())
	}
	t.mu.Unlock()
	// A lighter EMA (higher alpha) so the displayed speed converges to the real
	// rate in ~1s instead of feeling like it slowly creeps up over many seconds.
	const alpha = 0.6

	t.recalcDownloaded()
	if e.isActive(t.ID) {
		e.emitUpdate(t) // immediate first paint
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
				if t.Status == StatusDownloading {
					t.Speed = int64(instant)
				}
			} else {
				if t.Status == StatusDownloading {
					t.Speed = int64(alpha*instant + (1-alpha)*float64(t.Speed))
				}
			}
			t.mu.Unlock()
			t.recalcDownloaded()
			if e.isActive(t.ID) {
				e.emitUpdate(t)
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
func (e *Engine) emit(t *Task) error {
	e.emitMu.Lock()
	defer e.emitMu.Unlock()
	if !e.isActive(t.ID) {
		return nil
	}
	err := e.persistRecord(t.DurableRecord())
	if err != nil {
		t.mu.Lock()
		t.Error = fmt.Sprintf("save task: %v", err)
		t.mu.Unlock()
	}
	e.emitUpdateLocked(t)
	return err
}
func (e *Engine) emitUpdateLocked(t *Task) { e.cfg.OnUpdate(t.snapshotForUpdate()) }
func (e *Engine) emitUpdate(t *Task) {
	if e.isActive(t.ID) {
		e.cfg.OnUpdate(t.snapshotForUpdate())
	}
}
func (e *Engine) emitManaged(m *managed) { _ = e.emit(m.task) }
func (e *Engine) emitIfActive(t *Task)   { _ = e.emit(t) }

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

func cloneHeaders(headers map[string]string) map[string]string {
	copy := make(map[string]string, len(headers))
	for k, v := range headers {
		copy[k] = v
	}
	return copy
}
func (e *Engine) markDownloading(ctx context.Context, m *managed) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	m.task.setStatus(StatusDownloading, "")
	return nil
}
func (e *Engine) openFullResponse(ctx context.Context, client *http.Client, url string, headers map[string]string) (*http.Response, context.CancelFunc, error) {
	rctx, cancel := context.WithCancel(ctx)
	req, err := http.NewRequestWithContext(rctx, http.MethodGet, url, nil)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	applyHeaders(req, headers)
	resp, err := client.Do(req)
	if err != nil {
		cancel()
		return nil, nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		cancel()
		return nil, nil, fmt.Errorf("expected full response, got %s", resp.Status)
	}
	return resp, cancel, nil
}

func validChunkLayout(chunks []*Chunk, total, fileSize int64) bool {
	next := int64(0)
	for _, c := range chunks {
		if c == nil || c.Start != next || c.End < c.Start || c.End >= total {
			return false
		}
		n := c.loaded()
		if n < 0 || n > c.size() || (n > 0 && c.Start+n > fileSize) {
			return false
		}
		next = c.End + 1
	}
	return next == total
}

func (e *Engine) persistRecord(r Record) error {
	if e.cfg.Persist != nil {
		return e.cfg.Persist(r)
	}
	e.cfg.OnPersist(r)
	return nil
}
