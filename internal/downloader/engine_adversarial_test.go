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
