// Package takeover runs a localhost HTTP server that the browser extension uses
// to hand intercepted downloads to the application.
package takeover

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"
)

// DownloadRequest is the payload posted by the browser extension.
type DownloadRequest struct {
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
	onDownload func(DownloadRequest)
	version    string

	mu      sync.Mutex
	srv     *http.Server
	port    int
	running bool

	extMu     sync.RWMutex
	extCRX    []byte
	extUpdate []byte

	// Diagnostics: set when a browser fetches the update manifest / CRX, which
	// proves the policy install reached the download stage.
	manifestFetched bool
	crxFetched      bool
}

// FetchInfo reports whether a browser has fetched the update manifest and CRX.
func (s *Server) FetchInfo() (manifest, crx bool) {
	s.extMu.RLock()
	defer s.extMu.RUnlock()
	return s.manifestFetched, s.crxFetched
}

// New creates a takeover server. onDownload is invoked for each intercepted
// download (it should return quickly; heavy work belongs on another goroutine).
func New(version string, onDownload func(DownloadRequest)) *Server {
	return &Server{onDownload: onDownload, version: version}
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
	s.mu.Lock()
	if s.running {
		if s.port == port {
			s.mu.Unlock()
			return nil
		}
		s.mu.Unlock()
		_ = s.Stop()
		s.mu.Lock()
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/ping", s.handlePing)
	mux.HandleFunc("/download", s.handleDownload)
	mux.HandleFunc("/ext/updates.xml", s.handleUpdateXML)
	mux.HandleFunc("/ext/bdm.crx", s.handleCRX)

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		s.mu.Unlock()
		return fmt.Errorf("takeover listen: %w", err)
	}
	srv := &http.Server{Handler: withCORS(mux), ReadHeaderTimeout: 10 * time.Second}
	s.srv = srv
	s.port = port
	s.running = true
	s.mu.Unlock()

	go func() {
		_ = srv.Serve(ln)
		s.mu.Lock()
		s.running = false
		s.mu.Unlock()
	}()
	return nil
}

// Stop gracefully shuts the server down.
func (s *Server) Stop() error {
	s.mu.Lock()
	srv := s.srv
	s.srv = nil
	s.running = false
	s.mu.Unlock()
	if srv == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

// SetExtension publishes the signed CRX and its update manifest so browsers can
// fetch them for policy force-install.
func (s *Server) SetExtension(crx, updateXML []byte) {
	s.extMu.Lock()
	s.extCRX = crx
	s.extUpdate = updateXML
	s.extMu.Unlock()
}

func (s *Server) handleUpdateXML(w http.ResponseWriter, r *http.Request) {
	s.extMu.Lock()
	data := s.extUpdate
	if data != nil {
		s.manifestFetched = true
	}
	s.extMu.Unlock()
	if data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/xml")
	_, _ = w.Write(data)
}

func (s *Server) handleCRX(w http.ResponseWriter, r *http.Request) {
	s.extMu.Lock()
	data := s.extCRX
	if data != nil {
		s.crxFetched = true
	}
	s.extMu.Unlock()
	if data == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/x-chrome-extension")
	w.Header().Set("Content-Disposition", "attachment; filename=bdm.crx")
	_, _ = w.Write(data)
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]string{"app": "B Download Manager", "version": s.version})
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
	if req.URL == "" {
		http.Error(w, "missing url", http.StatusBadRequest)
		return
	}
	if s.onDownload != nil {
		s.onDownload(req)
	}
	writeJSON(w, map[string]bool{"ok": true})
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
