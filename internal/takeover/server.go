// Package takeover runs a localhost HTTP server that the browser extension uses
// to hand intercepted downloads to the application.
package takeover

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// DownloadRequest is the payload posted by the browser extension.
type DownloadRequest struct {
	RequestID string            `json:"requestId,omitempty"`
	URL       string            `json:"url"`
	Filename  string            `json:"filename"`
	Referrer  string            `json:"referrer"`
	MIME      string            `json:"mime"`
	FileSize  int64             `json:"fileSize"`
	UserAgent string            `json:"userAgent"`
	Cookie    string            `json:"cookie"`
	Headers   map[string]string `json:"headers"`
}

// Server is the local browser-integration endpoint. It binds only to loopback.
type Server struct {
	onDownload func(DownloadRequest) error
	version    string

	opMu    sync.Mutex
	lookup  func(string) (Receipt, error)
	mu      sync.Mutex
	srv     *http.Server
	port    int
	running bool
}

// New creates a takeover server. onDownload is invoked for each intercepted
// download (it should return quickly; heavy work belongs on another goroutine).
func New(version string, onDownload func(DownloadRequest)) *Server {
	return NewWithHandler(version, func(r DownloadRequest) error {
		if onDownload != nil {
			onDownload(r)
		}
		return nil
	}, nil)
}

// Running reports whether the server is currently listening.
func (s *Server) Running() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running
}

// Port returns the port the server is (or was last) bound to.
func (s *Server) Port() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.port
}

// Start binds the server to 127.0.0.1:port. It is a no-op if already running on
// the same port; if running on a different port it is restarted.
func (s *Server) Start(port int) error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid port")
	}
	s.mu.Lock()
	if s.running && s.port == port {
		s.mu.Unlock()
		return nil
	}
	s.mu.Unlock()
	// Bind before retiring the old listener: a failed hot change keeps it alive.
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		return fmt.Errorf("takeover listen: %w", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/download", s.handleDownload)
	mux.HandleFunc("/download/status", s.handleStatus)
	srv := &http.Server{Handler: withCORS(mux), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second}
	s.mu.Lock()
	old := s.srv
	s.srv = srv
	s.port = port
	s.running = true
	s.mu.Unlock()
	go func() {
		_ = srv.Serve(ln)
		s.mu.Lock()
		if s.srv == srv {
			s.running = false
		}
		s.mu.Unlock()
	}()
	return stopServer(old)
}
func stopServer(srv *http.Server) error {
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	err := srv.Shutdown(ctx)
	if err != nil {
		_ = srv.Close()
	}
	return err
}
func (s *Server) Stop() error {
	s.opMu.Lock()
	defer s.opMu.Unlock()
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.running = false
	s.mu.Unlock()
	return stopServer(srv)
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"app": "Better Download Manager", "version": s.version, "protocol": "2"})
}

func (s *Server) handleDownload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req DownloadRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	u, err := url.Parse(req.URL)
	if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") || len(req.RequestID) > 128 {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	if s.onDownload != nil {
		if err := s.onDownload(req); err != nil {
			http.Error(w, err.Error(), http.StatusServiceUnavailable)
			return
		}
	}
	writeJSON(w, Receipt{OK: true, Accepted: true, RequestID: req.RequestID, State: "accepted"})
}

// withCORS allows browser-extension origins to call the loopback API and answers
// CORS preflight requests.
func withCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

type Receipt struct {
	OK        bool   `json:"ok"`
	Accepted  bool   `json:"accepted"`
	RequestID string `json:"requestId"`
	State     string `json:"state"`
}

func NewWithHandler(version string, handler func(DownloadRequest) error, lookup func(string) (Receipt, error)) *Server {
	return &Server{version: version, onDownload: handler, lookup: lookup}
}
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	id := strings.TrimSpace(r.URL.Query().Get("requestId"))
	if id == "" || len(id) > 128 || s.lookup == nil {
		http.Error(w, "unknown request", 404)
		return
	}
	receipt, err := s.lookup(id)
	if err != nil {
		http.Error(w, err.Error(), 503)
		return
	}
	writeJSON(w, receipt)
}
