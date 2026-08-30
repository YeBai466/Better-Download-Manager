package downloader

// Adversarial scenario tests: each test simulates a hostile or quirky server /
// network and asserts the engine stays correct, recovers, and stays fast.

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yebai/better-download-manager/internal/proxy"
)

// newTestEngineCfg builds an engine with test-friendly timeouts and a plain
// keep-alive HTTP/1.1 client, forwarding updates like newTestEngine.
func newTestEngineCfg(updates chan TaskInfo, mut func(*Config)) *Engine {
	cfg := Config{
		MaxConcurrent: 4,
		ClientFactory: func(proxy.Settings) *http.Client { return &http.Client{Timeout: 60 * time.Second} },
		OnUpdate: func(info TaskInfo) {
			select {
			case updates <- info:
			default:
			}
		},
	}
	if mut != nil {
		mut(&cfg)
	}
	return NewEngine(cfg)
}

// serveRanged writes a (possibly truncated) range response for data.
func serveRanged(w http.ResponseWriter, r *http.Request, data []byte) {
	if s, e, ok := parseRange(r.Header.Get("Range"), len(data)); ok {
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", itoa(e-s+1))
		w.Header().Set("Content-Range", "bytes "+itoa(s)+"-"+itoa(e)+"/"+itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[s : e+1])
		return
	}
	w.Header().Set("Accept-Ranges", "bytes")
	w.Header().Set("Content-Length", itoa(len(data)))
	_, _ = w.Write(data)
}

// TestChunkConnectionReuse asserts that a multi-chunk download does not open a
// fresh TCP connection per chunk: response bodies must be drained to EOF so the
// transport returns connections to the keep-alive pool.
func TestChunkConnectionReuse(t *testing.T) {
	data := makeData(20 << 20) // 20 MiB -> many 1-2 MiB chunks, few workers
	var conns int64
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRanged(w, r, data)
	}))
	srv.Config.ConnState = func(c net.Conn, s http.ConnState) {
		if s == http.StateNew {
			atomic.AddInt64(&conns, 1)
		}
	}
	srv.Start()
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, nil)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 8, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, data)

	if got := atomic.LoadInt64(&conns); got > 8 {
		t.Fatalf("connection churn: %d TCP connections for one download (chunks are not reusing keep-alive connections)", got)
	}
}

// TestStreamStallAborts asserts that a server that sends headers plus a few
// bytes and then goes silent cannot hang a task forever: the stall watchdog
// must fire on the initial streamed response too, and the task must reach a
// terminal state (error) instead of "downloading at 0 B/s until doomsday".
func TestStreamStallAborts(t *testing.T) {
	data := makeData(4 << 20)
	block := make(chan struct{}) // never closed: the server stalls forever
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Non-ranged 200 with a known length: engine takes the single-stream path.
		w.Header().Set("Content-Length", itoa(len(data)))
		_, _ = w.Write(data[:64<<10])
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-block
	}))
	defer srv.Close()
	defer close(block)

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, func(c *Config) {
		c.StallTimeout = 500 * time.Millisecond
		c.Retries = 1
	})
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 4, AutoStart: true}); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(15 * time.Second)
	for {
		select {
		case info := <-updates:
			if info.Status == StatusError {
				return // watchdog fired and the task failed cleanly
			}
			if info.Status == StatusCompleted {
				t.Fatalf("cannot complete: server never sends the full body")
			}
		case <-deadline:
			t.Fatalf("task hung: stall watchdog never fired on the streamed response")
		}
	}
}

// TestRetryKeepsGoingWhileProgressing simulates a server that drops every
// connection after ~256 KiB. Each retry makes progress, so the engine must keep
// retrying to completion instead of burning the fixed retry budget.
func TestRetryKeepsGoingWhileProgressing(t *testing.T) {
	data := makeData(6 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, e, ok := parseRange(r.Header.Get("Range"), len(data))
		if !ok {
			s, e = 0, len(data)-1
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", itoa(e-s+1))
		if ok {
			w.Header().Set("Content-Range", "bytes "+itoa(s)+"-"+itoa(e)+"/"+itoa(len(data)))
			w.WriteHeader(http.StatusPartialContent)
		}
		// Send at most 256 KiB then abort the connection (short write vs the
		// declared Content-Length makes the client see an unexpected EOF).
		limit := 256 << 10
		if e-s+1 < limit {
			limit = e - s + 1
		}
		_, _ = w.Write(data[s : s+limit])
	}))
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, func(c *Config) { c.Retries = 2 })
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 4, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("progressing retries must not exhaust the budget: status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, data)
}

// TestInitialRequestRetriesTransientFailure asserts a transient 503 on the very
// first request does not instantly fail the task.
func TestInitialRequestRetriesTransientFailure(t *testing.T) {
	data := makeData(1 << 20)
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt64(&hits, 1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		serveRanged(w, r, data)
	}))
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, func(c *Config) { c.Retries = 3 })
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 4, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("initial transient 503 must be retried: status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, data)
}

// TestNonRangedStreamRetriesFromScratch: a range-less server drops the
// connection halfway on the first attempt; the engine must restart the stream
// from zero instead of failing the task.
func TestNonRangedStreamRetriesFromScratch(t *testing.T) {
	data := makeData(2 << 20)
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", itoa(len(data)))
		if atomic.AddInt64(&hits, 1) == 1 {
			_, _ = w.Write(data[:len(data)/2]) // short write, then handler returns
			return
		}
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, func(c *Config) { c.Retries = 2 })
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 4, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("non-ranged stream must retry from scratch: status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, data)
}

// TestFileReplacedMidDownloadRestarts: the remote file is replaced (new ETag,
// same size) while chunks are in flight. The engine must detect the identity
// change and transparently restart, finishing with the NEW content — not a
// corrupt mix, and not a dead task.
func TestFileReplacedMidDownloadRestarts(t *testing.T) {
	oldData := makeData(24 << 20)
	newData := make([]byte, len(oldData))
	for i := range newData {
		newData[i] = oldData[i] ^ 0xFF
	}
	var swapped atomic.Bool
	var served int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, tag := oldData, `"v1"`
		if swapped.Load() {
			data, tag = newData, `"v2"`
		}
		// RFC 7233 If-Range: serve the range only when the validator matches.
		if ir := r.Header.Get("If-Range"); ir != "" && ir != tag {
			w.Header().Set("ETag", tag)
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", itoa(len(data)))
			_, _ = w.Write(data)
			return
		}
		w.Header().Set("ETag", tag)
		serveRanged(w, r, data)
		if atomic.AddInt64(&served, 1) == 2 {
			swapped.Store(true) // replace the file after a couple of chunks
		}
	}))
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, nil)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 8, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("identity change must restart, not kill the task: status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, newData)
}

// TestWeakETagDoesNotBreakChunks: RFC 7233 forbids weak validators in If-Range;
// a compliant server answers 200 (full body) when it gets one. The engine must
// not send weak ETags in If-Range, so chunk requests keep getting 206.
func TestWeakETagDoesNotBreakChunks(t *testing.T) {
	data := makeData(24 << 20)
	const weak = `W/"v1"`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", weak)
		if ir := r.Header.Get("If-Range"); ir != "" {
			// Weak validator (or any mismatch) => whole representation.
			if strings.HasPrefix(ir, "W/") || ir != weak {
				w.Header().Set("Accept-Ranges", "bytes")
				w.Header().Set("Content-Length", itoa(len(data)))
				_, _ = w.Write(data)
				return
			}
		}
		serveRanged(w, r, data)
	}))
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, nil)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 8, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("weak ETag handling: status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, data)
}

// TestForwardedAcceptEncodingIsOverridden: takeover forwards browser headers
// including Accept-Encoding. The engine must force identity so ranged offsets
// address the raw representation and the saved file is not gzip garbage.
func TestForwardedAcceptEncodingIsOverridden(t *testing.T) {
	data := []byte(strings.Repeat("plain text that compresses well\n", 4096))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			w.Header().Set("Content-Encoding", "gzip")
			gz := gzip.NewWriter(w)
			_, _ = gz.Write(data)
			_ = gz.Close()
			return
		}
		w.Header().Set("Content-Length", itoa(len(data)))
		_, _ = w.Write(data)
	}))
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, nil)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.txt")
	_, err := e.Add(AddOptions{
		ID: "t1", URL: srv.URL, SavePath: dst, Connections: 4, AutoStart: true,
		Headers: map[string]string{"Accept-Encoding": "gzip, deflate, br"},
	})
	if err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, data)
}

// TestUpgradeTo206WhenServerIgnoresOpenRange: some servers legally answer 200
// to "Range: bytes=0-" (it means the whole file) but do honor real subranges.
// The engine should detect range support (Accept-Ranges + 0-0 probe) and keep
// the download resumable instead of degrading to a non-resumable single stream.
func TestUpgradeTo206WhenServerIgnoresOpenRange(t *testing.T) {
	data := makeData(24 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rng := r.Header.Get("Range")
		if rng == "" || rng == "bytes=0-" {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", itoa(len(data)))
			_, _ = w.Write(data)
			return
		}
		serveRanged(w, r, data)
	}))
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, nil)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 8, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("status=%s err=%s", info.Status, info.Error)
	}
	if !info.Resumable {
		t.Fatalf("server honours subranges (0-0 probe gives 206); task must be marked resumable")
	}
	assertFileEquals(t, dst, data)
}

// TestBogusContentRangeStartRejected: a broken server answering bytes=0- with a
// 206 whose Content-Range starts at a non-zero offset must not have its bytes
// silently written at offset 0.
func TestBogusContentRangeStartRejected(t *testing.T) {
	data := makeData(1 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Off-by-100 Content-Range with a body length that still matches the
		// advertised total, so a naive client "completes" with shifted bytes.
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", itoa(len(data)))
		w.Header().Set("Content-Range", "bytes 100-"+itoa(len(data)+99)+"/"+itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[100:])
		_, _ = w.Write(make([]byte, 100))
	}))
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, func(c *Config) { c.Retries = 1 })
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 4, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status == StatusCompleted {
		t.Fatalf("mismatched Content-Range start must not produce a silently corrupt file")
	}
}

// TestRedirectResolvedOncePerTransfer: chunk requests must go to the resolved
// final URL, not re-follow the redirect chain once per chunk.
func TestRedirectResolvedOncePerTransfer(t *testing.T) {
	data := makeData(24 << 20)
	dataSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRanged(w, r, data)
	}))
	defer dataSrv.Close()

	var redirects int64
	redirSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&redirects, 1)
		http.Redirect(w, r, dataSrv.URL, http.StatusFound)
	}))
	defer redirSrv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, nil)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: redirSrv.URL, SavePath: dst, Connections: 8, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, data)
	if got := atomic.LoadInt64(&redirects); got > 3 {
		t.Fatalf("redirect endpoint hit %d times; chunk requests are re-following the redirect chain", got)
	}
}

// TestExpiredSignedURLRefreshesViaOrigin: the redirect target (a "signed" URL)
// expires mid-download; chunk requests start failing with 403. The engine must
// go back through the origin URL to obtain a fresh signed URL and finish.
func TestExpiredSignedURLRefreshesViaOrigin(t *testing.T) {
	data := makeData(24 << 20)
	var gen atomic.Int64
	gen.Store(1)
	var served int64

	mux := http.NewServeMux()
	dataSrv := httptest.NewServer(mux)
	defer dataSrv.Close()

	mux.HandleFunc("/signed", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("tok") != fmt.Sprint(gen.Load()) {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		serveRanged(w, r, data)
		if atomic.AddInt64(&served, 1) == 3 {
			gen.Add(1) // expire the current token after a few chunks
		}
	})
	mux.HandleFunc("/origin", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, dataSrv.URL+"/signed?tok="+fmt.Sprint(gen.Load()), http.StatusFound)
	})

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, nil)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: dataSrv.URL + "/origin", SavePath: dst, Connections: 8, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("expired signed URL must be refreshed via origin: status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, data)
}

// TestServerCapsRangeLength: some CDNs answer a range request with a SHORTER
// range than asked (Content-Range end < requested end). The engine must accept
// the partial range and keep requesting the remainder instead of failing.
func TestServerCapsRangeLength(t *testing.T) {
	data := makeData(12 << 20)
	const capLen = 512 << 10 // server never returns more than 512 KiB per request
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, e, ok := parseRange(r.Header.Get("Range"), len(data))
		if !ok {
			w.Header().Set("Accept-Ranges", "bytes")
			w.Header().Set("Content-Length", itoa(len(data)))
			_, _ = w.Write(data)
			return
		}
		if e-s+1 > capLen {
			e = s + capLen - 1
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", itoa(e-s+1))
		w.Header().Set("Content-Range", "bytes "+itoa(s)+"-"+itoa(e)+"/"+itoa(len(data)))
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write(data[s : e+1])
	}))
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, nil)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 4, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status != StatusCompleted {
		t.Fatalf("capped range responses must be tolerated: status=%s err=%s", info.Status, info.Error)
	}
	assertFileEquals(t, dst, data)
}

// TestResumeValidationNetworkFailureKeepsPartial: the server is unreachable
// when a paused task is resumed. That must NOT be read as "the file changed"
// — the partial data has to survive so the user can retry later.
func TestResumeValidationNetworkFailureKeepsPartial(t *testing.T) {
	data := makeData(4 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serveRanged(w, r, data)
	}))
	url := srv.URL
	srv.Close() // nothing is listening any more

	dir := t.TempDir()
	dst := filepath.Join(dir, "out.bin")
	half := int64(len(data) / 2)
	pf, err := os.OpenFile(partPath(dst), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := pf.WriteAt(data[:half], 0); err != nil {
		t.Fatal(err)
	}
	pf.Close()

	m := metaFile{
		Version: 2, URL: url, TotalSize: int64(len(data)), Resumable: true, ETag: `"v1"`,
		Chunks:   []Chunk{{Index: 0, Start: 0, End: int64(len(data)) - 1, Downloaded: half}},
		Segments: []Segment{{Index: 0, Start: 0, End: int64(len(data)) - 1, Downloaded: half}},
	}
	mb, _ := json.Marshal(&m)
	if err := os.WriteFile(metaPath(dst), mb, 0o644); err != nil {
		t.Fatal(err)
	}

	updates := make(chan TaskInfo, 256)
	e := newTestEngineCfg(updates, func(c *Config) { c.Retries = 1 })
	defer e.Shutdown()
	if _, err := e.Add(AddOptions{ID: "t1", URL: url, SavePath: dst, Connections: 4, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, updates, StatusError)

	info, err := os.Stat(partPath(dst))
	if err != nil {
		t.Fatalf("partial file was destroyed by a failed resume check: %v", err)
	}
	if info.Size() != half {
		t.Fatalf("partial file truncated: got %d want %d", info.Size(), half)
	}
	if _, err := os.Stat(metaPath(dst)); err != nil {
		t.Fatalf("resume metadata was destroyed by a failed resume check: %v", err)
	}
}

// TestSilentTruncationIsRejected: a server that ends the body early on the
// LAST chunk (declared length, short delivery, clean close each time) must
// never yield a "completed" task with a short file.
func TestSilentTruncationIsRejected(t *testing.T) {
	data := makeData(12 << 20)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s, e, ok := parseRange(r.Header.Get("Range"), len(data))
		if !ok {
			s, e = 0, len(data)-1
		}
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", itoa(e-s+1))
		if ok {
			w.Header().Set("Content-Range", "bytes "+itoa(s)+"-"+itoa(e)+"/"+itoa(len(data)))
			w.WriteHeader(http.StatusPartialContent)
		}
		end := e + 1
		if e == len(data)-1 {
			end -= 16 // never deliver the last 16 bytes of the file
		}
		if end > s {
			_, _ = w.Write(data[s:end])
		}
	}))
	defer srv.Close()

	updates := make(chan TaskInfo, 512)
	e := newTestEngineCfg(updates, func(c *Config) { c.Retries = 1 })
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "out.bin")
	if _, err := e.Add(AddOptions{ID: "t1", URL: srv.URL, SavePath: dst, Connections: 4, AutoStart: true}); err != nil {
		t.Fatal(err)
	}
	info := waitForStatus(t, updates, StatusCompleted, StatusError)
	if info.Status == StatusCompleted {
		st, serr := os.Stat(dst)
		t.Fatalf("task reported completed on a truncated download (file=%v err=%v)", st.Size(), serr)
	}
	if _, err := os.Stat(dst); err == nil {
		t.Fatalf("a truncated download must not be published as the final file")
	}
}

// TestURLRefreshIsSingleFlight: when N workers hit the same expired signed URL
// at once, exactly ONE must re-resolve the origin. Otherwise a single expiry
// event fires N origin requests and instantly burns the refresh budget,
// failing the download.
func TestURLRefreshIsSingleFlight(t *testing.T) {
	var originHits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&originHits, 1)
		time.Sleep(30 * time.Millisecond) // widen the window for the racers
		w.Header().Set("Accept-Ranges", "bytes")
		w.Header().Set("Content-Length", "1")
		w.Header().Set("Content-Range", "bytes 0-0/1024")
		w.WriteHeader(http.StatusPartialContent)
		_, _ = w.Write([]byte{0})
	}))
	defer srv.Close()

	h := newURLHolder(srv.URL+"/signed?tok=old", srv.URL, nil)
	_, gen := h.get()
	id := responseIdentity{TotalSize: 1024}

	const workers = 8
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			errs <- h.refresh(context.Background(), srv.Client(), gen, id)
		}()
	}
	close(start)
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Fatalf("worker %d: %v", i, err)
		}
	}
	if got := atomic.LoadInt64(&originHits); got != 1 {
		t.Fatalf("not single-flight: %d origin requests for one expiry event", got)
	}
	if h.refreshes != 1 {
		t.Fatalf("refresh budget consumed %d times for one expiry event", h.refreshes)
	}
}

// TestConnectionLimiterNoLeakOnCancel: Acquire must hold no slot when it
// reports an error. Leaking on a cancelled context (pause/remove) would
// permanently shrink the global connection budget until the app restarts.
func TestConnectionLimiterNoLeakOnCancel(t *testing.T) {
	for trial := 0; trial < 500; trial++ {
		l := newConnectionLimiter(1)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := l.Acquire(ctx); err != nil && len(l.ch) != 0 {
			t.Fatalf("trial %d: Acquire failed but kept a slot (%d held)", trial, len(l.ch))
		}
	}
	// A cancelled Acquire must leave the limiter fully usable.
	l := newConnectionLimiter(1)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = l.Acquire(ctx)
	if err := l.Acquire(context.Background()); err != nil {
		t.Fatalf("limiter starved after a cancelled Acquire: %v", err)
	}
}

// TestTransferPlanTapersTail: workers exit as soon as the chunk queue is empty,
// so a uniformly-chunked plan ends with one connection grinding through a full
// chunk while the rest idle. The plan must taper toward the end of the file to
// bound that endgame tail.
func TestTransferPlanTapersTail(t *testing.T) {
	const total = int64(1 << 30) // 1 GiB -> 16 MiB chunks
	plan := buildTransferPlan(total, 8)
	if plan.workers != 8 {
		t.Fatalf("workers=%d want 8", plan.workers)
	}
	if len(plan.chunks) < 2 {
		t.Fatalf("expected many chunks, got %d", len(plan.chunks))
	}

	first := plan.chunks[0].size()
	last := plan.chunks[len(plan.chunks)-1].size()
	if last > first/2 {
		t.Fatalf("tail not tapered: first chunk %d bytes, last %d", first, last)
	}

	// Contiguity and coverage must survive the taper.
	var covered int64
	for i, c := range plan.chunks {
		if c.Start != covered {
			t.Fatalf("chunk %d starts at %d, expected %d (gap or overlap)", i, c.Start, covered)
		}
		if c.End < c.Start {
			t.Fatalf("chunk %d is empty: %d-%d", i, c.Start, c.End)
		}
		covered = c.End + 1
	}
	if covered != total {
		t.Fatalf("plan covers %d of %d bytes", covered, total)
	}

	// The worst-case single-connection tail is one chunk; keep it small
	// relative to the file.
	if last > total/128 {
		t.Fatalf("tail chunk %d bytes is too large for a %d byte file", last, total)
	}

	// A single-worker plan has no idle connection to starve: keep it uniform.
	solo := buildTransferPlan(4<<20, 1)
	for i, c := range solo.chunks[:len(solo.chunks)-1] {
		if c.size() != solo.chunks[0].size() {
			t.Fatalf("single-worker plan should stay uniform, chunk %d differs", i)
		}
	}
}

// TestWorkersStartSpreadAcrossFile: the dynamic queue used to hand every worker
// a chunk off the front of the file, so all N connections crowded into the first
// UI lane and the per-thread bars lit up one after another (looking like the
// threads started serially, even though they were all running). Each worker must
// start in its OWN region.
func TestWorkersStartSpreadAcrossFile(t *testing.T) {
	for _, total := range []int64{30 << 20, 100 << 20, 1 << 30, 4 << 30} {
		plan := buildTransferPlan(total, 8)
		lanes := displaySegmentsFromChunks(plan.chunks, plan.workers)
		if len(lanes) != plan.workers {
			t.Fatalf("total=%d: %d UI lanes for %d workers", total, len(lanes), plan.workers)
		}

		// Fast-start layout: worker 0 is the streamer and takes the reserved
		// lowest chunk; the rest claim ranged chunks concurrently.
		q := newChunkQueue(plan.chunks, plan.workers, true)
		claims := make([]*Chunk, plan.workers)
		claims[0] = q.nextContiguous(0)
		for i := 1; i < plan.workers; i++ {
			claims[i] = q.nextChunk(i)
		}
		for i, c := range claims {
			if c == nil {
				t.Fatalf("total=%d: worker %d got no chunk at start", total, i)
			}
			if c.Start < lanes[i].Start || c.End > lanes[i].End {
				t.Fatalf("total=%d: worker %d started on chunk %d-%d, outside its lane %d-%d",
					total, i, c.Start, c.End, lanes[i].Start, lanes[i].End)
			}
		}

		// Resume layout (no streamer): same requirement.
		q = newChunkQueue(plan.chunks, plan.workers, false)
		for i := 0; i < plan.workers; i++ {
			c := q.nextChunk(i)
			if c == nil {
				t.Fatalf("total=%d: resume worker %d got no chunk at start", total, i)
			}
			if c.Start < lanes[i].Start || c.End > lanes[i].End {
				t.Fatalf("total=%d: resume worker %d started on chunk %d-%d, outside its lane %d-%d",
					total, i, c.Start, c.End, lanes[i].Start, lanes[i].End)
			}
		}
	}
}

// TestQueueDrainsExactlyOnceWithStealing: spreading workers must not cost the
// work stealing that keeps a fast worker busy (and bounds the endgame tail).
// Every chunk must still be handed out exactly once, to somebody.
func TestQueueDrainsExactlyOnceWithStealing(t *testing.T) {
	plan := buildTransferPlan(1<<30, 8)
	q := newChunkQueue(plan.chunks, plan.workers, false)

	seen := make(map[int]int, len(plan.chunks))
	// Worker 0 races ahead: it drains its own region, then must steal.
	stolen := 0
	regions := chunkRegions(len(plan.chunks), plan.workers)
	for {
		c := q.nextChunk(0)
		if c == nil {
			break
		}
		seen[c.Index]++
		if c.Index < regions[0][0] || c.Index >= regions[0][1] {
			stolen++
		}
	}
	if stolen == 0 {
		t.Fatal("worker 0 never stole work after draining its own region")
	}
	// Everyone else finds the queue already empty.
	for i := 1; i < plan.workers; i++ {
		if c := q.nextChunk(i); c != nil {
			t.Fatalf("worker %d got chunk %d from a drained queue", i, c.Index)
		}
	}
	if len(seen) != len(plan.chunks) {
		t.Fatalf("handed out %d of %d chunks", len(seen), len(plan.chunks))
	}
	for idx, n := range seen {
		if n != 1 {
			t.Fatalf("chunk %d handed out %d times", idx, n)
		}
	}
}

// TestStreamerChunkStaysReserved: while the fast-start stream is alive, ranged
// workers must not claim the chunk it is streaming into — unless it is the only
// work left, in which case idling would be worse.
func TestStreamerChunkStaysReserved(t *testing.T) {
	plan := buildTransferPlan(1<<30, 8)
	q := newChunkQueue(plan.chunks, plan.workers, true)
	for i := 1; i < plan.workers; i++ {
		if c := q.nextChunk(i); c == nil || c.Index == 0 {
			t.Fatalf("worker %d claimed the streamer's reserved chunk", i)
		}
	}
	if c := q.nextContiguous(0); c == nil || c.Index != 0 {
		t.Fatal("streamer lost its reserved chunk")
	}

	// Last chunk standing: a waiting worker must take it rather than idle.
	solo := []*Chunk{{Index: 0, Start: 0, End: 1023}}
	q = newChunkQueue(solo, 4, true)
	if c := q.nextChunk(1); c == nil || c.Index != 0 {
		t.Fatal("worker idled instead of stealing the only remaining chunk")
	}
}

// TestConnectionsFanOutAcrossFile is the end-to-end counterpart of
// TestWorkersStartSpreadAcrossFile: it checks the wire, not just the queue.
// The initial burst of ranged requests must reach across the whole file rather
// than clustering at its front.
func TestConnectionsFanOutAcrossFile(t *testing.T) {
	const total = 64 << 20 // 64 MiB -> 8 workers, 8 MiB chunks
	data := makeData(total)

	var mu sync.Mutex
	var starts []int
	gotBurst := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start, end := 0, len(data)-1
		ranged := false
		w.Header().Set("Accept-Ranges", "bytes")
		if s, e, ok := parseRange(r.Header.Get("Range"), len(data)); ok {
			start, end, ranged = s, e, true
		}
		if ranged && end > start { // ignore the 1-byte probes
			mu.Lock()
			starts = append(starts, start)
			if len(starts) == 8 {
				close(gotBurst)
			}
			mu.Unlock()
		}
		body := data[start : end+1]
		w.Header().Set("Content-Length", itoa(len(body)))
		if ranged {
			w.Header().Set("Content-Range", "bytes "+itoa(start)+"-"+itoa(end)+"/"+itoa(len(data)))
			w.WriteHeader(http.StatusPartialContent)
		}
		// Trickle, so every worker issues its first request before any of them
		// finishes a chunk and asks for a second one.
		flusher, _ := w.(http.Flusher)
		for off := 0; off < len(body); off += 4096 {
			ce := off + 4096
			if ce > len(body) {
				ce = len(body)
			}
			if _, err := w.Write(body[off:ce]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(time.Millisecond)
		}
	}))
	defer srv.Close()

	updates := make(chan TaskInfo, 256)
	e := newTestEngine(updates)
	defer e.Shutdown()

	dst := filepath.Join(t.TempDir(), "fanout.bin")
	if _, err := e.Add(AddOptions{
		ID: "t1", URL: srv.URL, SavePath: dst, Connections: 8, AutoStart: true,
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gotBurst:
	case <-time.After(30 * time.Second):
		mu.Lock()
		n := len(starts)
		mu.Unlock()
		t.Fatalf("only %d ranged requests in 30s, expected 8 connections to fan out", n)
	}
	_ = e.Pause("t1")

	mu.Lock()
	burst := append([]int(nil), starts[:8]...)
	mu.Unlock()

	// Every opening request must land in a DIFFERENT UI lane. When the workers
	// all claim off the front of the file instead, the burst piles into the
	// first few lanes and the rest of the thread bars stay dark until the
	// workers get there.
	plan := buildTransferPlan(total, 8)
	lanes := displaySegmentsFromChunks(plan.chunks, plan.workers)
	hit := map[int]int{}
	for _, s := range burst {
		for li, l := range lanes {
			if int64(s) >= l.Start && int64(s) <= l.End {
				hit[li]++
				break
			}
		}
	}
	if len(hit) != len(lanes) {
		t.Fatalf("opening requests covered %d of %d lanes (offsets %v, per-lane hits %v)",
			len(hit), len(lanes), burst, hit)
	}
}
