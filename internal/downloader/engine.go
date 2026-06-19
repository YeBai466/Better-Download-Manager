package downloader

import (
	"context"
	"errors"
	"net/http"
	"sync"
	"sync/atomic"
	"time"
)

// DefaultConnections is the number of parallel connections used per task when
// the caller does not specify one (matches IDM's common default).
const DefaultConnections = 8

// Config configures an Engine. The callbacks let the host (Wails service) react
// to task updates without the engine depending on the UI or storage layers.
type Config struct {
	MaxConcurrent int                 // max simultaneously downloading tasks
	ClientFactory func() *http.Client // builds the HTTP client (proxy-aware)
	OnUpdate      func(info TaskInfo) // throttled progress + status changes
	OnPersist     func(rec Record)    // durable state changes (status only)
	OnRemoved     func(id string)     // task removed
}

// Engine manages the task queue, scheduling and lifecycle of downloads.
type Engine struct {
	cfg Config

	mu          sync.Mutex
	tasks       map[string]*managed
	order       []string
	activeCount int
	closed      bool
}

type managed struct {
	task    *Task
	cancel  context.CancelFunc
	done    chan struct{}
	running bool
}

// NewEngine creates an engine with sensible defaults applied to cfg.
func NewEngine(cfg Config) *Engine {
	if cfg.MaxConcurrent < 1 {
		cfg.MaxConcurrent = 5
	}
	if cfg.ClientFactory == nil {
		cfg.ClientFactory = func() *http.Client { return &http.Client{} }
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
	return &Engine{cfg: cfg, tasks: map[string]*managed{}}
}

// ErrNotFound is returned when an operation references an unknown task id.
var ErrNotFound = errors.New("task not found")

// AddOptions describes a new download to add.
type AddOptions struct {
	ID          string
	URL         string
	Filename    string
	SavePath    string
	Category    string
	Connections int
	Headers     map[string]string
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
		Status:      StatusQueued,
		CreatedAt:   time.Now(),
	}

	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return TaskInfo{}, errors.New("engine closed")
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
	e.emit(m.task)
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
	var cancel context.CancelFunc
	if ok {
		cancel = m.cancel
	}
	e.mu.Unlock()
	if !ok {
		return ErrNotFound
	}
	switch m.task.getStatus() {
	case StatusDownloading, StatusConnecting:
		if cancel != nil {
			cancel()
		}
		m.task.recalcDownloaded() // reflect current segment progress immediately
		m.task.mu.Lock()
		m.task.Speed = 0
		m.task.mu.Unlock()
		m.task.setStatus(StatusPaused, "")
		e.emit(m.task)
	case StatusQueued:
		m.task.setStatus(StatusPaused, "")
		e.emit(m.task)
	}
	return nil
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
	e.mu.Lock()
	e.closed = true
	running := make([]*managed, 0)
	for _, m := range e.tasks {
		if m.running {
			running = append(running, m)
		}
	}
	e.mu.Unlock()
	for _, m := range running {
		if m.cancel != nil {
			m.cancel()
		}
	}
	for _, m := range running {
		<-m.done
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

	client := e.cfg.ClientFactory()
	t.setStatus(StatusConnecting, "")
	e.emit(t)

	if err := e.prepare(ctx, client, t); err != nil {
		e.fail(t, ctx, err)
		return
	}

	w, err := openPartFile(t.SavePath, t.TotalSize)
	if err != nil {
		e.fail(t, ctx, err)
		return
	}

	t.setStatus(StatusDownloading, "")
	e.emit(t)

	if err := e.transfer(ctx, client, t, w); err != nil {
		w.Close()
		_ = writeMeta(t)
		e.fail(t, ctx, err)
		return
	}

	if err := finalize(w, t.SavePath); err != nil {
		e.fail(t, ctx, err)
		return
	}
	removeMeta(t.SavePath)
	t.recalcDownloaded()
	t.setStatus(StatusCompleted, "")
	e.emit(t)
}

// prepare probes the URL (if needed) and (re)builds the segment plan, loading
// any existing resume metadata.
func (e *Engine) prepare(ctx context.Context, client *http.Client, t *Task) error {
	t.mu.RLock()
	hasSegments := len(t.Segments) > 0
	t.mu.RUnlock()
	if hasSegments {
		return nil
	}

	// Try to resume from sidecar metadata first.
	if m, err := readMeta(t.SavePath); err == nil && m.TotalSize > 0 {
		t.mu.Lock()
		t.TotalSize = m.TotalSize
		t.Resumable = m.Resumable
		t.MIME = m.MIME
		if t.Filename == "" {
			t.Filename = m.Filename
		}
		t.Segments = make([]*Segment, len(m.Segments))
		for i := range m.Segments {
			s := m.Segments[i]
			t.Segments[i] = &s
		}
		t.mu.Unlock()
		t.recalcDownloaded()
		return nil
	}

	res, err := probe(ctx, client, t.URL, t.headersCopy())
	if err != nil {
		return err
	}
	t.mu.Lock()
	t.TotalSize = res.TotalSize
	t.Resumable = res.Resumable
	t.MIME = res.MIME
	if t.Filename == "" {
		t.Filename = res.Filename
	}
	conns := t.Connections
	if !res.Resumable {
		conns = 1
	}
	t.Segments = buildSegments(res.TotalSize, conns)
	t.mu.Unlock()
	return nil
}

// transfer runs the segment workers concurrently and reports progress until all
// complete, the context is cancelled, or one fails.
func (e *Engine) transfer(ctx context.Context, client *http.Client, t *Task, w *fileWriter) error {
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

// reportProgress periodically computes speed and emits throttled updates.
func (e *Engine) reportProgress(t *Task, progress *int64, stop <-chan struct{}) {
	ticker := time.NewTicker(400 * time.Millisecond)
	defer ticker.Stop()
	var lastBytes int64
	lastTime := time.Now()
	const alpha = 0.4
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
			t.Speed = int64(alpha*instant + (1-alpha)*float64(t.Speed))
			t.mu.Unlock()
			t.recalcDownloaded()
			e.cfg.OnUpdate(t.Snapshot())
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
	e.emit(t)
}

// emit pushes both a UI update and a durable persist for a task.
func (e *Engine) emit(t *Task) {
	e.cfg.OnUpdate(t.Snapshot())
	e.cfg.OnPersist(t.Record())
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
