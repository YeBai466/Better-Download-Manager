package service

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/yebai/better-download-manager/internal/config"
	"github.com/yebai/better-download-manager/internal/proxy"
	"github.com/yebai/better-download-manager/internal/takeover"
)

func TestHandoffIsDurableAndIdempotent(t *testing.T) {
	svc, err := New(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.ServiceShutdown()
	cfg := config.Default()
	cfg.TakeoverEnabled = false
	cfg.Proxy = proxy.Settings{Mode: proxy.ModeNone}
	if _, err := svc.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	req := takeover.DownloadRequest{RequestID: "same-request", URL: "https://example.invalid/file", Filename: "file.bin"}
	if err := svc.acceptHandoff(req); err != nil {
		t.Fatal(err)
	}
	if err := svc.acceptHandoff(req); err != nil {
		t.Fatal(err)
	}
	h, found, err := svc.store.Handoff(req.RequestID)
	if err != nil || !found || h.State != "dialog" {
		t.Fatalf("handoff=%+v found=%t err=%v", h, found, err)
	}
	if len(svc.pendingAdds) != 1 {
		t.Fatalf("opened %d dialogs", len(svc.pendingAdds))
	}
	info, err := svc.AddURL(AddRequest{HandoffID: req.RequestID, URL: req.URL, Filename: req.Filename, SaveDir: t.TempDir(), AutoStart: false})
	if err != nil {
		t.Fatal(err)
	}
	again, err := svc.AddURL(AddRequest{HandoffID: req.RequestID, URL: req.URL, Filename: req.Filename, AutoStart: false})
	if err != nil || info.ID != again.ID || len(svc.ListTasks()) != 1 {
		t.Fatalf("duplicate task: %+v %v", again, err)
	}
	r, err := svc.handoffStatus(req.RequestID)
	if err != nil || !r.Accepted {
		t.Fatalf("receipt=%+v err=%v", r, err)
	}
}

func TestUnrelatedSettingsPreserveProxyClient(t *testing.T) {
	svc, err := New(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.ServiceShutdown()
	cfg := config.Default()
	cfg.TakeoverEnabled = false
	cfg.Proxy = proxy.Settings{Mode: proxy.ModeNone}
	if _, err := svc.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	before := svc.newClient()
	cfg.Language = "en"
	if _, err := svc.SaveSettings(cfg); err != nil {
		t.Fatal(err)
	}
	if before != svc.newClient() {
		t.Fatal("language save discarded connection pool")
	}
}

func TestFilenameFromAllEntryPointsIsSanitized(t *testing.T) {
	svc, err := New(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.ServiceShutdown()
	dir := t.TempDir()
	for _, name := range []string{"../outside.bin", `..\outside.bin`, "CON.txt", "name:stream"} {
		info, err := svc.AddURL(AddRequest{URL: "https://example.invalid/file", Filename: name, SaveDir: dir})
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(info.SavePath) != dir {
			t.Fatalf("escaped destination: %s", info.SavePath)
		}
		if info.Filename == name {
			t.Fatalf("unsafe filename unchanged: %s", name)
		}
	}
}

func TestGetTaskSeesFastCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("ok")) }))
	defer srv.Close()
	svc, err := New(filepath.Join(t.TempDir(), "test.db"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.ServiceShutdown()
	info, err := svc.AddURL(AddRequest{URL: srv.URL, Filename: "fast.bin", SaveDir: t.TempDir(), Proxy: proxy.Settings{Mode: proxy.ModeNone}, AutoStart: true})
	if err != nil {
		t.Fatal(err)
	}
	final := waitDone(t, svc, info.ID)
	snapshot, err := svc.GetTask(info.ID)
	if err != nil || snapshot.Status != final.Status || snapshot.Revision == 0 {
		t.Fatalf("snapshot=%+v err=%v", snapshot, err)
	}
}
