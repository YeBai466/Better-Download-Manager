package downloader

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestUnknownContentRangeRequiresFullResponse(t *testing.T) {
	data := makeData(8192)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Range") != "" {
			w.Header().Set("Content-Range", "bytes 0-1023/*")
			w.WriteHeader(http.StatusPartialContent)
			_, _ = w.Write(data[:1024])
			return
		}
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	updates := make(chan TaskInfo, 128)
	e := newTestEngine(updates)
	defer e.Shutdown()
	dst := filepath.Join(t.TempDir(), "unknown.bin")
	_, err := e.Add(AddOptions{ID: "unknown", URL: srv.URL, SavePath: dst, AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatal(info.Error)
	}
	assertFileEquals(t, dst, data)
}

func TestEmptyDownloadReportsCompleteProgress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Header().Set("Content-Length", "0") }))
	defer srv.Close()
	updates := make(chan TaskInfo, 128)
	e := newTestEngine(updates)
	defer e.Shutdown()
	dst := filepath.Join(t.TempDir(), "empty.bin")
	_, err := e.Add(AddOptions{ID: "empty", URL: srv.URL, SavePath: dst, AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted || info.Progress != 1 || info.TotalSize != 0 || info.Speed != 0 {
		t.Fatalf("incorrect empty-file state: %+v", info)
	}
	assertFileEquals(t, dst, nil)
}

func TestPauseLatencyP95(t *testing.T) {
	started := make(chan struct{}, 32)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576")
		_, _ = w.Write(make([]byte, 64<<10))
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer srv.Close()
	e := NewEngine(Config{MaxConcurrent: 1})
	defer e.Shutdown()
	dir := t.TempDir()
	samples := make([]time.Duration, 20)
	for i := range samples {
		id := fmt.Sprint(i)
		if _, err := e.Add(AddOptions{ID: id, URL: srv.URL, SavePath: filepath.Join(dir, id), AutoStart: true}); err != nil {
			t.Fatal(err)
		}
		select {
		case <-started:
		case <-time.After(5 * time.Second):
			t.Fatal("request did not start")
		}
		start := time.Now()
		if err := e.Pause(id); err != nil {
			t.Fatal(err)
		}
		samples[i] = time.Since(start)
		if err := e.Remove(id, true); err != nil {
			t.Fatal(err)
		}
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	p95 := samples[18]
	t.Logf("20 active HTTP transfers: pause P95=%s", p95)
	if p95 >= 250*time.Millisecond {
		t.Fatalf("pause P95 exceeds target: %s", p95)
	}
}

func TestRecoverRenameBeforeDatabaseCommit(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "committed.bin")
	data := makeData(4096)
	w, err := openPartFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteAt(data, 0)
	task := &Task{ID: "committed", URL: "https://example.invalid/file", SavePath: dst, TotalSize: int64(len(data)), Resumable: true, Status: StatusPaused,
		Chunks: []*Chunk{{Index: 0, Start: 0, End: int64(len(data)) - 1, Downloaded: int64(len(data))}}}
	if err := writeMeta(task); err != nil {
		t.Fatal(err)
	}
	if err := finalize(w, dst); err != nil {
		t.Fatal(err)
	}
	updates := make(chan TaskInfo, 128)
	e := newTestEngine(updates)
	defer e.Shutdown()
	e.Restore(TaskFromRecord(task.Record()))
	if err := e.Start(task.ID); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatal(info.Error)
	}
	assertFileEquals(t, dst, data)
}

func TestDeleteSerializesWithInflightPersistence(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	var mu sync.Mutex
	persisted := false
	e := NewEngine(Config{Persist: func(r Record) error {
		if r.Status == StatusConnecting {
			close(entered)
			<-release
		}
		mu.Lock()
		persisted = true
		mu.Unlock()
		return nil
	}, RemoveRecord: func(string) error { mu.Lock(); persisted = false; mu.Unlock(); return nil }})
	_, err := e.Add(AddOptions{ID: "remove", URL: "https://example.invalid/file", SavePath: filepath.Join(t.TempDir(), "file"), AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	removed := make(chan error, 1)
	go func() { removed <- e.Remove("remove", true) }()
	close(release)
	if err := <-removed; err != nil {
		t.Fatal(err)
	}
	e.Shutdown()
	mu.Lock()
	defer mu.Unlock()
	if persisted {
		t.Fatal("deleted task resurrected in persistence")
	}
}

func TestFailedAdmissionDoesNotRegisterTask(t *testing.T) {
	e := NewEngine(Config{Persist: func(Record) error { return errors.New("disk full") }})
	defer e.Shutdown()
	if _, err := e.Add(AddOptions{ID: "failed", SavePath: filepath.Join(t.TempDir(), "file")}); err == nil {
		t.Fatal("admission acknowledged without durable record")
	}
	if len(e.List()) != 0 {
		t.Fatal("failed task was registered")
	}
}

func TestPauseDoesNotWaitForPersistence(t *testing.T) {
	entered, release := make(chan struct{}), make(chan struct{})
	e := NewEngine(Config{Persist: func(r Record) error {
		if r.Status == StatusConnecting {
			close(entered)
			<-release
		}
		return nil
	}})
	defer e.Shutdown()
	_, err := e.Add(AddOptions{ID: "pause", URL: "https://example.invalid/file", SavePath: filepath.Join(t.TempDir(), "file"), AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}
	<-entered
	start := time.Now()
	if err := e.Pause("pause"); err != nil {
		close(release)
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	close(release)
	if elapsed >= 250*time.Millisecond {
		t.Fatalf("pause blocked on storage: %s", elapsed)
	}
}

func TestSplitRangesAreNotImmediatelyRecombined(t *testing.T) {
	chunk := &Chunk{Index: 0, Start: 0, End: 8<<20 - 1, Downloaded: 1 << 20}
	task := &Task{Chunks: []*Chunk{chunk}}
	q := newChunkQueue(task.Chunks, 1, false)
	q.claimed[0] = true
	q.setActive(0, []*Chunk{chunk})
	q.requeueAndSplit(0, task, true)
	if !validChunkLayout(task.Chunks, 8<<20, 1<<20) {
		t.Fatal("split introduced a gap or overlap")
	}
	batch := q.takeBatch(0, 32<<20)
	if len(batch) != 1 || !batch[0].split {
		t.Fatalf("split tail was merged again: %+v", batch)
	}
}

func TestInitialStreamsRespectGlobalBudget(t *testing.T) {
	var active, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		w.Header().Set("Content-Length", "1")
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		time.Sleep(40 * time.Millisecond)
		_, _ = w.Write([]byte{1})
	}))
	defer srv.Close()
	updates := make(chan TaskInfo, 256)
	e := newTestEngineCfg(updates, func(c *Config) { c.MaxConcurrent = 3; c.MaxConnections = 1 })
	defer e.Shutdown()
	dir := t.TempDir()
	for i := 0; i < 3; i++ {
		id := fmt.Sprint(i)
		_, err := e.Add(AddOptions{ID: id, URL: srv.URL, SavePath: filepath.Join(dir, id), AutoStart: true})
		if err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 3; i++ {
		info := waitForStatus(t, updates, StatusCompleted, StatusError)
		if info.Status != StatusCompleted {
			t.Fatal(info.Error)
		}
	}
	if peak.Load() != 1 {
		t.Fatalf("peak requests=%d, limit=1", peak.Load())
	}
}

func TestConnectionBudgetResizeRetainsLeases(t *testing.T) {
	l := newConnectionLimiter(2)
	_ = l.Acquire(context.Background())
	_ = l.Acquire(context.Background())
	l.SetLimit(1)
	l.Release()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := l.Acquire(ctx); err == nil {
		t.Fatal("new request admitted while at lowered cap")
	}
	l.Release()
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatal(err)
	}
	l.Release()
}

func TestResumeRejectsProgressPastFileEnd(t *testing.T) {
	chunks := []*Chunk{{Start: 0, End: 99}, {Start: 100, End: 199, Downloaded: 20}}
	if validChunkLayout(chunks, 200, 100) {
		t.Fatal("accepted downloaded bytes beyond file EOF")
	}
	if !validChunkLayout(chunks, 200, 120) {
		t.Fatal("valid sparse layout rejected")
	}
	chunks[1].Start = 99
	if validChunkLayout(chunks, 200, 120) {
		t.Fatal("overlapping ranges accepted")
	}
}

func TestNonRangeRetryUsesNewLength(t *testing.T) {
	data := makeData(8192)
	var requests atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			w.Header().Set("Content-Length", "4096")
			_, _ = w.Write(data[:128])
			return
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(data)))
		_, _ = w.Write(data)
	}))
	defer srv.Close()
	updates := make(chan TaskInfo, 128)
	e := newTestEngine(updates)
	defer e.Shutdown()
	dst := filepath.Join(t.TempDir(), "retry.bin")
	_, err := e.Add(AddOptions{ID: "retry", URL: srv.URL, SavePath: dst, AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatal(info.Error)
	}
	assertFileEquals(t, dst, data)
}

func TestFinalizePreservesExistingDestination(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "collision.bin")
	w, err := openPartFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = w.WriteAt([]byte("new"), 0)
	if err := os.WriteFile(dst, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := finalize(w, dst); err == nil {
		t.Fatal("expected destination conflict")
	}
	assertFileEquals(t, dst, []byte("original"))
	assertFileEquals(t, partPath(dst), []byte("new"))
}

func TestLowLimitIsNotNetworkStall(t *testing.T) {
	data := makeData(4096)
	srv := rangeServer(t, data, false, 0)
	defer srv.Close()
	updates := make(chan TaskInfo, 128)
	e := newTestEngineCfg(updates, func(c *Config) {
		c.SpeedLimit = 4096
		c.StallTimeout = 100 * time.Millisecond
	})
	defer e.Shutdown()
	dst := filepath.Join(t.TempDir(), "limited.bin")
	_, _ = e.Add(AddOptions{ID: "limited", URL: srv.URL, SavePath: dst, AutoStart: true})
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatal(info.Error)
	}
	assertFileEquals(t, dst, data)
}

func TestGlobalSpeedLimitAccuracy(t *testing.T) {
	data := makeData(512 << 10)
	srv := rangeServer(t, data, false, 0)
	defer srv.Close()
	updates := make(chan TaskInfo, 256)
	e := newTestEngineCfg(updates, func(c *Config) { c.MaxConcurrent = 2; c.SpeedLimit = 256 << 10 })
	defer e.Shutdown()
	dir := t.TempDir()
	start := time.Now()
	for i := 0; i < 2; i++ {
		id := fmt.Sprint(i)
		if _, err := e.Add(AddOptions{ID: id, URL: srv.URL, SavePath: filepath.Join(dir, id), AutoStart: true}); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 2; i++ {
		if info := waitForStatus(t, updates, StatusCompleted, StatusError); info.Status != StatusCompleted {
			t.Fatal(info.Error)
		}
	}
	rate := float64(2*len(data)) / time.Since(start).Seconds()
	ratio := rate / float64(256<<10)
	t.Logf("global rate=%.2f KiB/s, target=256 KiB/s, ratio=%.4f", rate/1024, ratio)
	if ratio < 0.90 || ratio > 1.10 {
		t.Fatalf("global speed limit outside 10%% tolerance: %.4f", ratio)
	}
}

func TestSpeedLimitChangeWakesWaiter(t *testing.T) {
	l := newSpeedLimiter(1)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- l.Wait(ctx, 4096) }()
	time.Sleep(30 * time.Millisecond)
	l.SetRate(0)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("rate change did not wake waiter")
	}
}

func TestSlowTailIsReassignedAfterWriterStops(t *testing.T) {
	data := makeData(16 << 20)
	var split atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end, ok := parseRange(r.Header.Get("Range"), len(data))
		if !ok {
			start, end = 0, len(data)-1
		}
		w.Header().Set("Content-Length", fmt.Sprint(end-start+1))
		w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end, len(data)))
		w.Header().Set("ETag", `"stable"`)
		w.WriteHeader(http.StatusPartialContent)
		if start > 8<<20 {
			split.Store(true)
		}
		for pos := start; pos <= end; {
			n := min(64<<10, end-pos+1)
			if _, err := w.Write(data[pos : pos+n]); err != nil {
				return
			}
			w.(http.Flusher).Flush()
			pos += n
			if start == 8<<20 {
				select {
				case <-r.Context().Done():
					return
				case <-time.After(40 * time.Millisecond):
				}
			}
		}
	}))
	defer srv.Close()
	updates := make(chan TaskInfo, 256)
	e := newTestEngineCfg(updates, func(c *Config) { c.AdaptiveConnections = true; c.MaxConnections = 2 })
	defer e.Shutdown()
	dst := filepath.Join(t.TempDir(), "tail.bin")
	_, err := e.Add(AddOptions{ID: "tail", URL: srv.URL, SavePath: dst, Connections: 2, AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatal(info.Error)
	}
	if !split.Load() {
		t.Fatal("slow tail was not split")
	}
	assertFileEquals(t, dst, data)
}
