package service

import (
	"crypto/sha256"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/yebai/better-download-manager/internal/config"
	"github.com/yebai/better-download-manager/internal/downloader"
	"github.com/yebai/better-download-manager/internal/proxy"
)

// TestConcurrentSameFilenameGetsDistinctPaths: adding the same URL/filename
// twice (double-click, takeover + manual) must never let two tasks write the
// same .part file — that interleaves ranges from two engines into one file.
func TestConcurrentSameFilenameGetsDistinctPaths(t *testing.T) {
	data := make([]byte, 2<<20)
	rand.New(rand.NewSource(3)).Read(data)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Accept-Ranges", "bytes")
		http.ServeContent(w, r, "data.bin", fixedModTime, newReadSeeker(data))
	}))
	defer srv.Close()

	dir := t.TempDir()
	svc, err := New(filepath.Join(dir, "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.ServiceShutdown()

	cfg := config.Default()
	cfg.DownloadDir = dir
	cfg.Categorize = false
	cfg.Proxy = proxy.Settings{Mode: proxy.ModeNone}
	cfg.TakeoverEnabled = false
	if _, err := svc.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}

	const n = 4
	infos := make([]downloader.TaskInfo, n)
	errs := make([]error, n)
	done := make(chan int, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			infos[i], errs[i] = svc.AddURL(AddRequest{URL: srv.URL, Filename: "same.bin", Connections: 4, AutoStart: true})
			done <- i
		}(i)
	}
	for i := 0; i < n; i++ {
		<-done
	}
	paths := map[string]bool{}
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		if paths[infos[i].SavePath] {
			t.Fatalf("two concurrent tasks share SavePath %q — they would corrupt each other's .part file", infos[i].SavePath)
		}
		paths[infos[i].SavePath] = true
	}
	for i := 0; i < n; i++ {
		final := waitDone(t, svc, infos[i].ID)
		if final.Status != downloader.StatusCompleted {
			t.Fatalf("task %d: status=%s err=%s", i, final.Status, final.Error)
		}
		got, err := os.ReadFile(final.SavePath)
		if err != nil {
			t.Fatal(err)
		}
		if sha256.Sum256(got) != sha256.Sum256(data) {
			t.Fatalf("task %d: corrupt content at %s", i, final.SavePath)
		}
	}
}
