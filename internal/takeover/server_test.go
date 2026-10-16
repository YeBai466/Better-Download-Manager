package takeover

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// freePort asks the OS for an available loopback port.
func freePort(t *testing.T) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().(*net.TCPAddr).Port
}

func TestHandlerFailureIsNotAcknowledged(t *testing.T) {
	s := NewWithHandler("1", func(DownloadRequest) error { return errors.New("disk full") }, nil)
	r := httptest.NewRequest("POST", "/download", bytes.NewBufferString(`{"url":"https://example.com/file","requestId":"one"}`))
	w := httptest.NewRecorder()
	s.handleDownload(w, r)
	if w.Code != 503 {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
}

func TestFailedPortChangeKeepsOldListener(t *testing.T) {
	s := New("1", nil)
	old := freePort(t)
	if err := s.Start(old); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
	occupied, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer occupied.Close()
	if err := s.Start(occupied.Addr().(*net.TCPAddr).Port); err == nil {
		t.Fatal("expected port conflict")
	}
	if !s.Running() || s.Port() != old {
		t.Fatal("old server was discarded")
	}
	waitReady(t, fmt.Sprintf("http://127.0.0.1:%d/ping", old))
}

func TestServerPingAndDownload(t *testing.T) {
	got := make(chan DownloadRequest, 1)
	s := New("9.9.9", func(r DownloadRequest) { got <- r })

	port := freePort(t)
	if err := s.Start(port); err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	waitReady(t, base+"/ping")

	// /ping
	resp, err := http.Get(base + "/ping")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("ping status %d", resp.StatusCode)
	}
	var ping map[string]string
	_ = json.Unmarshal(body, &ping)
	if ping["version"] != "9.9.9" {
		t.Fatalf("unexpected ping body: %s", body)
	}

	// /download
	req := DownloadRequest{URL: "https://example.com/f.zip", Filename: "f.zip", Cookie: "a=1"}
	rb, _ := json.Marshal(req)
	resp, err = http.Post(base+"/download", "application/json", bytes.NewReader(rb))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("download status %d", resp.StatusCode)
	}

	select {
	case r := <-got:
		if r.URL != req.URL || r.Filename != req.Filename || r.Cookie != "a=1" {
			t.Fatalf("callback got %+v", r)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("onDownload was not called")
	}
}

func TestServerRestartOnDifferentPort(t *testing.T) {
	s := New("1.0", func(DownloadRequest) {})
	p1 := freePort(t)
	if err := s.Start(p1); err != nil {
		t.Fatal(err)
	}
	p2 := freePort(t)
	if err := s.Start(p2); err != nil { // should stop p1 and bind p2
		t.Fatal(err)
	}
	defer s.Stop()
	if s.Port() != p2 {
		t.Fatalf("expected port %d, got %d", p2, s.Port())
	}
	waitReady(t, fmt.Sprintf("http://127.0.0.1:%d/ping", p2))
}

func waitReady(t *testing.T, url string) {
	t.Helper()
	for i := 0; i < 50; i++ {
		if resp, err := http.Get(url); err == nil {
			resp.Body.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("server not ready at %s", url)
}
