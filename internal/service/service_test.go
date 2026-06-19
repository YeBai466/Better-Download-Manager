package service

import (
	"crypto/sha256"
	"io"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yebai/b-download-manager/internal/config"
	"github.com/yebai/b-download-manager/internal/downloader"
	"github.com/yebai/b-download-manager/internal/proxy"
)

func TestServiceAddAndDownload(t *testing.T) {
	data := make([]byte, 512*1024)
	rand.New(rand.NewSource(7)).Read(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		http.ServeContent(w, r, "data.bin", time.Now(), newReadSeeker(data))
	}))
	defer srv.Close()

	dir := t.TempDir()
	svc, err := New(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.ServiceShutdown()

	// Direct connection, no browser takeover server, files into dir.
	cfg := config.Default()
	cfg.DownloadDir = dir
	cfg.Categorize = false
	cfg.Proxy = proxy.Settings{Mode: proxy.ModeNone}
	cfg.TakeoverEnabled = false
	if _, err := svc.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}

	info, err := svc.AddURL(AddRequest{URL: srv.URL, Filename: "data.bin", Connections: 6, AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}

	final := waitDone(t, svc, info.ID)
	if final.Status != downloader.StatusCompleted {
		t.Fatalf("status=%s err=%s", final.Status, final.Error)
	}

	got, err := os.ReadFile(final.SavePath)
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256(data) {
		t.Fatalf("downloaded content mismatch")
	}

	// Task should be persisted and reloadable.
	recs, err := svc.store.LoadTasks()
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 || recs[0].Status != downloader.StatusCompleted {
		t.Fatalf("unexpected persisted records: %+v", recs)
	}
}

func waitDone(t *testing.T, svc *DownloadService, id string) downloader.TaskInfo {
	t.Helper()
	deadline := time.After(20 * time.Second)
	for {
		select {
		case <-deadline:
			t.Fatal("download did not finish in time")
		case <-time.After(100 * time.Millisecond):
			info, err := svc.engine.Get(id)
			if err != nil {
				t.Fatal(err)
			}
			if info.Status == downloader.StatusCompleted || info.Status == downloader.StatusError {
				return info
			}
		}
	}
}

// newReadSeeker wraps a byte slice for http.ServeContent.
func newReadSeeker(b []byte) *byteReadSeeker { return &byteReadSeeker{b: b} }

type byteReadSeeker struct {
	b   []byte
	pos int64
}

func (r *byteReadSeeker) Read(p []byte) (int, error) {
	if r.pos >= int64(len(r.b)) {
		return 0, io.EOF
	}
	n := copy(p, r.b[r.pos:])
	r.pos += int64(n)
	return n, nil
}

func (r *byteReadSeeker) Seek(offset int64, whence int) (int64, error) {
	switch whence {
	case 0:
		r.pos = offset
	case 1:
		r.pos += offset
	case 2:
		r.pos = int64(len(r.b)) + offset
	}
	return r.pos, nil
}
