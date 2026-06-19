package downloader

import (
	"crypto/sha256"
	"encoding/json"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/yebai/b-download-manager/internal/proxy"
)

// makeData returns deterministic pseudo-random bytes of length n.
func makeData(n int) []byte {
	b := make([]byte, n)
	r := rand.New(rand.NewSource(42))
	r.Read(b)
	return b
}

// rangeServer serves data with optional Range support and an optional per-chunk
// delay (to make pausing observable).
func rangeServer(t *testing.T, data []byte, supportRange bool, delay time.Duration) *httptest.Server {
	t.Helper()
	h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := 0, len(data)-1
		ranged := false
		if supportRange {
			w.Header().Set("Accept-Ranges", "bytes")
			if s, e, ok := parseRange(r.Header.Get("Range"), len(data)); ok {
				start, end, ranged = s, e, true
			}
		}
		body := data[start : end+1]
		w.Header().Set("Content-Length", itoa(len(body)))
		if ranged {
			w.Header().Set("Content-Range", "bytes "+itoa(start)+"-"+itoa(end)+"/"+itoa(len(data)))
			w.WriteHeader(http.StatusPartialContent)
		}
		flusher, _ := w.(http.Flusher)
		const chunk = 16 * 1024
		for off := 0; off < len(body); off += chunk {
			ce := off + chunk
			if ce > len(body) {
				ce = len(body)
			}
			if _, err := w.Write(body[off:ce]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if delay > 0 {
				time.Sleep(delay)
			}
		}
	})
	return httptest.NewServer(h)
}

// parseRange parses a single "bytes=start-end" header. end may be empty.
func parseRange(h string, size int) (start, end int, ok bool) {
	if !strings.HasPrefix(h, "bytes=") {
		return 0, 0, false
	}
	spec := strings.TrimPrefix(h, "bytes=")
	parts := strings.SplitN(spec, "-", 2)
	if len(parts) != 2 {
		return 0, 0, false
	}
	s, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	e := size - 1
	if parts[1] != "" {
		e, err = strconv.Atoi(parts[1])
		if err != nil {
			return 0, 0, false
		}
	}
	if e >= size {
		e = size - 1
	}
	return s, e, true
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// waitForStatus drains updates until the task reaches one of want or times out.
func waitForStatus(t *testing.T, updates <-chan TaskInfo, want ...Status) TaskInfo {
	t.Helper()
	timeout := time.After(15 * time.Second)
	for {
		select {
		case info := <-updates:
			for _, s := range want {
				if info.Status == s {
					return info
				}
			}
		case <-timeout:
			t.Fatalf("timed out waiting for status %v", want)
		}
	}
}

func newTestEngine(updates chan TaskInfo) *Engine {
	return NewEngine(Config{
		MaxConcurrent: 4,
		ClientFactory: func(proxy.Settings) *http.Client { return &http.Client{Timeout: 30 * time.Second} },
		OnUpdate: func(info TaskInfo) {
			select {
			case updates <- info:
			default:
			}
		},
	})
}

func assertFileEquals(t *testing.T, path string, want []byte) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("size mismatch: got %d want %d", len(got), len(want))
	}
	if sha256.Sum256(got) != sha256.Sum256(want) {
		t.Fatalf("content mismatch")
	}
}

func TestSegmentedDownload(t *testing.T) {
	data := makeData(1 << 20) // 1 MiB
	srv := rangeServer(t, data, true, 0)
	defer srv.Close()

	updates := make(chan TaskInfo, 256)
	e := newTestEngine(updates)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	_, err := e.Add(AddOptions{
		ID: "t1", URL: srv.URL, SavePath: dst, Connections: 8, AutoStart: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("status=%s err=%s", info.Status, info.Error)
	}
	if len(info.Segments) < 2 {
		t.Fatalf("expected multiple segments, got %d", len(info.Segments))
	}
	assertFileEquals(t, dst, data)
}

func TestSingleStreamNoRange(t *testing.T) {
	data := makeData(300 * 1024)
	srv := rangeServer(t, data, false, 0)
	defer srv.Close()

	updates := make(chan TaskInfo, 256)
	e := newTestEngine(updates)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	_, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 8, AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}

	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("status=%s err=%s", info.Status, info.Error)
	}
	if len(info.Segments) != 1 {
		t.Fatalf("expected single segment for non-range server, got %d", len(info.Segments))
	}
	assertFileEquals(t, dst, data)
}

func TestResumeFromMeta(t *testing.T) {
	data := makeData(512 * 1024)
	srv := rangeServer(t, data, true, 0)
	defer srv.Close()

	dir := t.TempDir()
	dst := filepath.Join(dir, "out.bin")
	half := int64(len(data) / 2)

	// Simulate a previous interrupted download: a .part file with the first
	// half written and a sidecar meta recording the progress.
	pf, err := os.OpenFile(partPath(dst), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pf.WriteAt(data[:half], 0); err != nil {
		t.Fatal(err)
	}
	_ = pf.Truncate(int64(len(data)))
	pf.Close()

	m := metaFile{
		URL: srv.URL, TotalSize: int64(len(data)), Resumable: true,
		Segments: []Segment{{Index: 0, Start: 0, End: int64(len(data)) - 1, Downloaded: half}},
	}
	mb, _ := json.Marshal(&m)
	if err := os.WriteFile(metaPath(dst), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	updates := make(chan TaskInfo, 256)
	e := newTestEngine(updates)
	defer e.Shutdown()

	_, err = e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 8, AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}

	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, data)

	// Meta should be cleaned up after completion.
	if _, err := os.Stat(metaPath(dst)); !os.IsNotExist(err) {
		t.Fatalf("expected meta removed, got err=%v", err)
	}
}

func TestPauseResume(t *testing.T) {
	data := makeData(1 << 20)
	srv := rangeServer(t, data, true, 15*time.Millisecond) // slow enough to pause mid-flight
	defer srv.Close()

	updates := make(chan TaskInfo, 256)
	e := newTestEngine(updates)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	_, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 4, AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}

	// Wait until some bytes have transferred, then pause.
	waitForStatus(t, updates, StatusDownloading)
	time.Sleep(150 * time.Millisecond)
	if err := e.Pause("t1"); err != nil {
		t.Fatal(err)
	}
	paused, _ := e.Get("t1")
	if paused.Status != StatusPaused {
		t.Fatalf("expected paused, got %s", paused.Status)
	}
	if paused.Downloaded <= 0 || paused.Downloaded >= int64(len(data)) {
		t.Fatalf("expected partial progress, got %d/%d", paused.Downloaded, len(data))
	}

	// Resume to completion.
	if err := e.Start("t1"); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, data)
}

func TestRemoveRunningDeletesFilesAndDoesNotReappear(t *testing.T) {
	data := makeData(2 << 20)
	srv := rangeServer(t, data, true, 20*time.Millisecond)
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	removed := make(chan string, 1)
	e := NewEngine(Config{
		MaxConcurrent: 1,
		ClientFactory: func(proxy.Settings) *http.Client { return &http.Client{Timeout: 30 * time.Second} },
		OnUpdate: func(info TaskInfo) {
			select {
			case updates <- info:
			default:
			}
		},
		OnRemoved: func(id string) { removed <- id },
	})
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 4, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, updates, StatusDownloading)
	if err := e.Remove("t1", true); err != nil {
		t.Fatal(err)
	}
	select {
	case id := <-removed:
		if id != "t1" {
			t.Fatalf("removed id=%s", id)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("remove callback not called")
	}
	time.Sleep(500 * time.Millisecond)
	if got := e.List(); len(got) != 0 {
		t.Fatalf("expected empty list, got %+v", got)
	}
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("expected final file removed, err=%v", err)
	}
	if _, err := os.Stat(partPath(dst)); !os.IsNotExist(err) {
		t.Fatalf("expected partial file removed, err=%v", err)
	}
	if _, err := os.Stat(metaPath(dst)); !os.IsNotExist(err) {
		t.Fatalf("expected meta file removed, err=%v", err)
	}
}

func TestRemoveCompletedDeletesFinalFile(t *testing.T) {
	data := makeData(128 * 1024)
	srv := rangeServer(t, data, true, 0)
	defer srv.Close()

	updates := make(chan TaskInfo, 256)
	e := newTestEngine(updates)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 2, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("status=%s err=%s", info.Status, info.Error)
	}
	if err := e.Remove("t1", true); err != nil {
		t.Fatal(err)
	}
	time.Sleep(100 * time.Millisecond)
	if _, err := os.Stat(dst); !os.IsNotExist(err) {
		t.Fatalf("expected final file removed, err=%v", err)
	}
}

func TestPauseQueuedTask(t *testing.T) {
	updates := make(chan TaskInfo, 16)
	e := NewEngine(Config{
		MaxConcurrent: 1,
		ClientFactory: func(proxy.Settings) *http.Client { return &http.Client{Timeout: 30 * time.Second} },
		OnUpdate: func(info TaskInfo) {
			select {
			case updates <- info:
			default:
			}
		},
	})
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: "http://127.0.0.1/never", SavePath: dst, AutoStart: false}); err != nil {
		t.Fatal(err)
	}
	if err := e.Start("t1"); err != nil {
		t.Fatal(err)
	}
	if err := e.Pause("t1"); err != nil {
		t.Fatal(err)
	}
	info, err := e.Get("t1")
	if err != nil {
		t.Fatal(err)
	}
	if info.Status != StatusPaused {
		t.Fatalf("expected paused, got %s", info.Status)
	}
}

func TestTaskProxyPersistsAcrossRecord(t *testing.T) {
	want := proxy.Settings{Mode: proxy.ModeCustom, URL: "socks5://127.0.0.1:1080", Username: "u", Password: "p"}
	task := TaskFromRecord((&Task{
		ID: "t1", URL: "https://example.com/file", SavePath: "file.bin", Status: StatusPaused,
		Proxy: want, CreatedAt: time.Now(),
	}).Record())
	if task.Proxy != want {
		t.Fatalf("proxy mismatch: got %+v want %+v", task.Proxy, want)
	}
}

func TestOpenPartFileDoesNotPreallocate(t *testing.T) {
	dst := filepath.Join(t.TempDir(), "large.bin")
	w, err := openPartFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()

	info, err := os.Stat(partPath(dst))
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected new part file size 0, got %d", info.Size())
	}
}
