package downloader

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
