// Package service implements the Wails-bound application services that expose
// the download engine, settings and browser integration to the frontend.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	cat "github.com/yebai/b-download-manager/internal/category"
	"github.com/yebai/b-download-manager/internal/config"
	"github.com/yebai/b-download-manager/internal/downloader"
	"github.com/yebai/b-download-manager/internal/httpclient"
	"github.com/yebai/b-download-manager/internal/policy"
	"github.com/yebai/b-download-manager/internal/store"
	"github.com/yebai/b-download-manager/internal/takeover"
	"github.com/yebai/b-download-manager/internal/updates"
)

// Frontend event names.
const (
	EventTaskUpdate      = "task:update"
	EventTaskRemoved     = "task:removed"
	EventTakeoverRequest = "takeover:request"
)

// MainWindowName is the name assigned to the primary window so the service can
// focus it when a browser download arrives.
const MainWindowName = "main"

// AddWindowName is the name of the separate "add download" window (IDM-style).
const AddWindowName = "add"

// Version is the application version reported to the browser extension.
const Version = "1.0.0"

// StoreExtensionID is the published Chrome Web Store extension ID. Because the
// extension is store-hosted, policy force-install works on consumer machines
// too (unlike self-hosted off-store extensions).
const StoreExtensionID = "hgkakilajhnbpmhmpcnblioiiaomdkjp"

// DownloadService is the primary Wails service. It owns the engine, store and
// takeover server and exposes methods to the frontend.
type DownloadService struct {
	store    *store.Store
	engine   *downloader.Engine
	takeover *takeover.Server

	extFiles fs.FS  // bundled chromium extension files (for manual "Load unpacked")
	keyDir   string // app data dir (also where the extension is extracted)

	mu         sync.RWMutex
	settings   config.Settings
	pendingAdd AddPrefill
}

// AddPrefill is the data shown in the separate add-download window.
type AddPrefill struct {
	URL      string            `json:"url"`
	Filename string            `json:"filename"`
	Headers  map[string]string `json:"headers"`
}

// New constructs the service backed by a database at dbPath. extFiles is the
// bundled browser-extension filesystem (rooted at the extension's manifest.json)
// used for silent policy install.
func New(dbPath string, extFiles fs.FS) (*DownloadService, error) {
	st, err := store.Open(dbPath)
	if err != nil {
		return nil, err
	}
	settings, err := st.LoadSettings()
	if err != nil {
		st.Close()
		return nil, err
	}
	s := &DownloadService{store: st, settings: settings, extFiles: extFiles, keyDir: filepath.Dir(dbPath)}

	s.engine = downloader.NewEngine(downloader.Config{
		MaxConcurrent: settings.MaxConcurrent,
		ClientFactory: s.newClient,
		OnUpdate:      s.onTaskUpdate,
		OnPersist:     s.onTaskPersist,
		OnRemoved:     s.onTaskRemoved,
	})
	s.takeover = takeover.New(Version, s.onTakeover)
	return s, nil
}

// --- Wails service lifecycle ---

// ServiceStartup restores persisted tasks and starts the takeover server.
func (s *DownloadService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	recs, err := s.store.LoadTasks()
	if err != nil {
		return err
	}
	for _, r := range recs {
		s.engine.Restore(downloader.TaskFromRecord(r))
	}
	s.mu.RLock()
	cfg := s.settings
	s.mu.RUnlock()
	if cfg.TakeoverEnabled {
		if err := s.takeover.Start(cfg.TakeoverPort); err != nil {
			// Non-fatal: another instance may hold the port.
			fmt.Println("takeover server:", err)
		}
	}
	return nil
}

// ServiceShutdown stops downloads and releases resources.
func (s *DownloadService) ServiceShutdown() error {
	s.engine.Shutdown()
	_ = s.takeover.Stop()
	return s.store.Close()
}

// --- Engine callbacks ---

func (s *DownloadService) newClient() *http.Client {
	s.mu.RLock()
	p := s.settings.Proxy
	s.mu.RUnlock()
	c, err := httpclient.New(p)
	if err != nil {
		return &http.Client{}
	}
	return c
}

func (s *DownloadService) onTaskUpdate(info downloader.TaskInfo) {
	if app := application.Get(); app != nil {
		app.Event.Emit(EventTaskUpdate, info)
	}
}

func (s *DownloadService) onTaskPersist(rec downloader.Record) {
	if err := s.store.SaveTask(rec); err != nil {
		fmt.Println("save task:", err)
	}
}

func (s *DownloadService) onTaskRemoved(id string) {
	_ = s.store.DeleteTask(id)
	if app := application.Get(); app != nil {
		app.Event.Emit(EventTaskRemoved, id)
	}
}

// --- Frontend-exposed methods ---

// AddRequest describes a download to add from the UI or the add dialog.
type AddRequest struct {
	URL         string            `json:"url"`
	Filename    string            `json:"filename"`
	Category    string            `json:"category"`
	SaveDir     string            `json:"saveDir"`
	Connections int               `json:"connections"`
	Headers     map[string]string `json:"headers"`
	AutoStart   bool              `json:"autoStart"`
}

// AddURL registers a new download and returns its task info.
func (s *DownloadService) AddURL(req AddRequest) (downloader.TaskInfo, error) {
	if strings.TrimSpace(req.URL) == "" {
		return downloader.TaskInfo{}, fmt.Errorf("url is required")
	}
	filename := req.Filename
	if filename == "" {
		filename = filenameFromURL(req.URL)
	}
	category := req.Category
	if category == "" {
		category = cat.Resolve(filename, "")
	}
	saveDir := req.SaveDir
	if saveDir == "" {
		saveDir = s.resolveDir(category)
	}
	savePath := uniquePath(filepath.Join(saveDir, filename))

	conns := req.Connections
	if conns < 1 {
		s.mu.RLock()
		conns = s.settings.Connections
		s.mu.RUnlock()
	}

	return s.engine.Add(downloader.AddOptions{
		ID:          newID(),
		URL:         req.URL,
		Filename:    filename,
		SavePath:    savePath,
		Category:    category,
		Connections: conns,
		Headers:     req.Headers,
		AutoStart:   req.AutoStart,
	})
}

// ShowAddWindow opens (or focuses) the separate add-download window, prefilled
// with the given data. Called from the main UI's "Add URL" button.
func (s *DownloadService) ShowAddWindow(p AddPrefill) {
	s.openAddWindow(p)
}

// ConsumePendingAdd returns and clears the prefill for the add window (called by
// the add window when it loads).
func (s *DownloadService) ConsumePendingAdd() AddPrefill {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pendingAdd
	s.pendingAdd = AddPrefill{}
	return p
}

// openAddWindow stores the prefill and shows the dedicated add window.
func (s *DownloadService) openAddWindow(p AddPrefill) {
	s.mu.Lock()
	s.pendingAdd = p
	s.mu.Unlock()

	app := application.Get()
	if app == nil {
		return
	}
	if w, ok := app.Window.Get(AddWindowName); ok {
		app.Event.Emit("add:reload", nil) // tell it to re-read the prefill
		w.Show()
		w.Focus()
		return
	}
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             AddWindowName,
		Title:            "添加下载",
		Width:            560,
		Height:           500,
		MinWidth:         460,
		MinHeight:        420,
		BackgroundColour: application.NewRGB(245, 246, 248),
		URL:              "/?view=add",
	})
}

// ListTasks returns all tasks in insertion order.
func (s *DownloadService) ListTasks() []downloader.TaskInfo { return s.engine.List() }

// StartTask queues a paused/errored task.
func (s *DownloadService) StartTask(id string) error { return s.engine.Start(id) }

// PauseTask stops an active task.
func (s *DownloadService) PauseTask(id string) error { return s.engine.Pause(id) }

// RemoveTask deletes a task, optionally removing the partial file.
func (s *DownloadService) RemoveTask(id string, deleteFile bool) error {
	return s.engine.Remove(id, deleteFile)
}

// StartAll queues every non-completed task.
func (s *DownloadService) StartAll() {
	for _, t := range s.engine.List() {
		if t.Status != downloader.StatusCompleted && t.Status != downloader.StatusDownloading {
			_ = s.engine.Start(t.ID)
		}
	}
}

// PauseAll stops every active task.
func (s *DownloadService) PauseAll() {
	for _, t := range s.engine.List() {
		if t.Status == downloader.StatusDownloading || t.Status == downloader.StatusConnecting {
			_ = s.engine.Pause(t.ID)
		}
	}
}

// Preview holds metadata shown in the add dialog before a download starts.
type Preview struct {
	Filename  string `json:"filename"`
	TotalSize int64  `json:"totalSize"`
	Resumable bool   `json:"resumable"`
	MIME      string `json:"mime"`
	Category  string `json:"category"`
}

// ProbeURL fetches metadata for a URL so the add dialog can show file info.
func (s *DownloadService) ProbeURL(url string) (Preview, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := downloader.Probe(ctx, s.newClient(), url, nil)
	if err != nil {
		return Preview{}, err
	}
	name := res.Filename
	if name == "" {
		name = filenameFromURL(url)
	}
	return Preview{
		Filename:  name,
		TotalSize: res.TotalSize,
		Resumable: res.Resumable,
		MIME:      res.MIME,
		Category:  cat.Resolve(name, res.MIME),
	}, nil
}

// GetSettings returns the current settings.
func (s *DownloadService) GetSettings() config.Settings {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings
}

// SaveSettings persists new settings and applies side effects (takeover server).
func (s *DownloadService) SaveSettings(cfg config.Settings) (config.Settings, error) {
	cfg.Normalize()
	if err := s.store.SaveSettings(cfg); err != nil {
		return config.Settings{}, err
	}
	s.mu.Lock()
	s.settings = cfg
	s.mu.Unlock()

	if cfg.TakeoverEnabled {
		_ = s.takeover.Start(cfg.TakeoverPort)
	} else {
		_ = s.takeover.Stop()
	}
	s.applyAutostart(cfg.AutoStart)
	return cfg, nil
}

// applyAutostart registers or removes the login autostart entry to match cfg.
func (s *DownloadService) applyAutostart(enabled bool) {
	app := application.Get()
	if app == nil || app.Autostart == nil {
		return
	}
	cur, _ := app.Autostart.IsEnabled()
	if enabled {
		// Re-register every save so the --minimized argument tracks the current
		// StartMinimized setting.
		_ = app.Autostart.Disable()
		opts := application.AutostartOptions{}
		s.mu.RLock()
		if s.settings.StartMinimized {
			opts.Arguments = []string{"--minimized"}
		}
		s.mu.RUnlock()
		if err := app.Autostart.EnableWithOptions(opts); err != nil {
			fmt.Println("autostart enable:", err)
		}
	} else if cur {
		if err := app.Autostart.Disable(); err != nil {
			fmt.Println("autostart disable:", err)
		}
	}
}

// CheckForUpdates queries GitHub Releases and returns version + notes. The bool
// arg is ignored; it exists so the frontend can call it explicitly.
func (s *DownloadService) CheckForUpdates() (updates.Result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	return updates.Check(ctx, Version)
}

// OpenURL opens a URL in the user's default browser (used for the release page
// / installer download link).
func (s *DownloadService) OpenURL(url string) error {
	if app := application.Get(); app != nil {
		return app.Browser.OpenURL(url)
	}
	return openInShell(url)
}

// Categories returns the available category names for the UI sidebar.
func (s *DownloadService) Categories() []string { return cat.All() }

// ResolveSaveDir returns the real, concrete directory a download of the given
// category would be saved to (so the add window can show an actual path instead
// of "auto"). The directory is not created until the download starts.
func (s *DownloadService) ResolveSaveDir(category string) string {
	if category == "" {
		category = cat.General
	}
	return s.resolveDir(category)
}

// ExtStatus reports the browser-extension force-install state. Chrome/Edge mean
// the policy registry entry exists for the store extension ID.
type ExtStatus struct {
	ID     string `json:"id"`     // the Chrome Web Store extension ID
	Chrome bool   `json:"chrome"` // policy configured for Chrome
	Edge   bool   `json:"edge"`   // policy configured for Edge
}

// BrowserExtensionStatus returns the current install status (no elevation).
func (s *DownloadService) BrowserExtensionStatus() ExtStatus {
	st := policy.GetStatus(StoreExtensionID)
	return ExtStatus{ID: StoreExtensionID, Chrome: st.Chrome, Edge: st.Edge}
}

// InstallBrowserExtension force-installs the published Web Store extension into
// the selected browsers (names like "Chrome"/"Edge"; empty = all installed) via
// enterprise policy. Prompts for administrator elevation (UAC). Because the
// extension is store-hosted, this works on consumer machines too.
func (s *DownloadService) InstallBrowserExtension(browsers []string) error {
	if err := policy.Install(StoreExtensionID, policy.StoreUpdateURL, browsers); err != nil {
		return err
	}
	return nil
}

// UninstallBrowserExtension removes the force-install policy for the selected
// browsers (empty = all). Prompts for UAC.
func (s *DownloadService) UninstallBrowserExtension(browsers []string) error {
	return policy.Uninstall(StoreExtensionID, browsers)
}
type ManualInstallInfo struct {
	Dir string `json:"dir"` // folder containing the unpacked extension
}

// InstalledBrowsers returns which supported browsers (Chrome / Edge) are
// installed, so the UI can offer them as choices.
func (s *DownloadService) InstalledBrowsers() []string { return policy.InstalledBrowsers() }

// PrepareManualInstall extracts the bundled extension to a real folder and opens
// that folder in Explorer, so the user can "Load unpacked". It does NOT try to
// open chrome://extensions: browsers deliberately block navigating to internal
// pages from the command line (especially when already running), so that path
// is unreliable. The UI instead copies the extensions URL to the clipboard for
// the user to paste. This is the reliable install path on unmanaged (consumer)
// machines, where policy force-install of off-store extensions is blocked.
func (s *DownloadService) PrepareManualInstall() (ManualInstallInfo, error) {
	if s.extFiles == nil {
		return ManualInstallInfo{}, fmt.Errorf("没有内置扩展文件")
	}
	dir := filepath.Join(s.keyDir, "extension")
	if err := extractFS(s.extFiles, dir); err != nil {
		return ManualInstallInfo{}, err
	}
	_ = openInShell(dir)
	return ManualInstallInfo{Dir: dir}, nil
}

// extractFS writes every file in src to dir on disk.
func extractFS(src fs.FS, dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return fs.WalkDir(src, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		target := filepath.Join(dir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		data, err := fs.ReadFile(src, path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0o644)
	})
}

// RestartBrowsers gracefully closes and relaunches running Chrome/Edge so the
// extension policy takes effect. Returns a human-readable result message.
func (s *DownloadService) RestartBrowsers() (string, error) {
	names, err := policy.RestartBrowsers()
	if err != nil {
		return "", err
	}
	if len(names) == 0 {
		return "未检测到正在运行的 Chrome 或 Edge，直接启动浏览器即可。", nil
	}
	return "已重启：" + strings.Join(names, "、") + "。请稍候浏览器自动加载扩展。", nil
}

// RunningBrowsers returns which supported browsers are currently running.
func (s *DownloadService) RunningBrowsers() []string { return policy.RunningBrowsers() }

// ChooseFolder opens a native directory picker and returns the selected path.
func (s *DownloadService) ChooseFolder() (string, error) {
	app := application.Get()
	if app == nil {
		return "", fmt.Errorf("application not ready")
	}
	return app.Dialog.OpenFile().
		CanChooseDirectories(true).
		CanChooseFiles(false).
		SetTitle("选择保存目录").
		PromptForSingleSelection()
}

// OpenFile opens a completed download in the default application.
func (s *DownloadService) OpenFile(id string) error {
	info, err := s.engine.Get(id)
	if err != nil {
		return err
	}
	return openInShell(info.SavePath)
}

// OpenFolder reveals a download in Windows Explorer.
func (s *DownloadService) OpenFolder(id string) error {
	info, err := s.engine.Get(id)
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(info.SavePath); statErr == nil {
		return exec.Command("explorer", "/select,", filepath.Clean(info.SavePath)).Start()
	}
	return openInShell(filepath.Dir(info.SavePath))
}

// --- Takeover handling ---

func (s *DownloadService) onTakeover(req takeover.DownloadRequest) {
	headers := map[string]string{}
	for k, v := range req.Headers {
		headers[k] = v
	}
	if req.Referrer != "" {
		headers["Referer"] = req.Referrer
	}
	if req.UserAgent != "" {
		headers["User-Agent"] = req.UserAgent
	}
	if req.Cookie != "" {
		headers["Cookie"] = req.Cookie
	}

	s.mu.RLock()
	action := s.settings.TakeoverAction
	s.mu.RUnlock()

	if action == config.ActionAutoStart {
		_, _ = s.AddURL(AddRequest{
			URL: req.URL, Filename: req.Filename, Headers: headers, AutoStart: true,
		})
		return
	}

	// Dialog mode: open the dedicated add window prefilled with the request.
	s.openAddWindow(AddPrefill{URL: req.URL, Filename: req.Filename, Headers: headers})
}

func (s *DownloadService) focusWindow() {
	app := application.Get()
	if app == nil {
		return
	}
	if w, ok := app.Window.Get(MainWindowName); ok {
		w.Show()
		w.Focus()
	}
}

// --- helpers ---

func (s *DownloadService) resolveDir(category string) string {
	s.mu.RLock()
	cfg := s.settings
	s.mu.RUnlock()
	if !cfg.Categorize {
		return cfg.DownloadDir
	}
	if override, ok := cfg.CategoryDirs[category]; ok && override != "" {
		return override
	}
	return filepath.Join(cfg.DownloadDir, cat.DefaultSubfolder(category))
}

func openInShell(path string) error {
	// "" is the title argument for cmd's start command.
	return exec.Command("cmd", "/c", "start", "", filepath.Clean(path)).Start()
}

func newID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func filenameFromURL(rawURL string) string {
	u := rawURL
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i]
	}
	u = strings.TrimRight(u, "/")
	base := u
	if i := strings.LastIndex(u, "/"); i >= 0 {
		base = u[i+1:]
	}
	if base == "" {
		return "download"
	}
	return base
}

// uniquePath appends " (n)" before the extension if the path already exists.
func uniquePath(path string) string {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return path
	}
	ext := filepath.Ext(path)
	base := strings.TrimSuffix(path, ext)
	for i := 1; ; i++ {
		candidate := fmt.Sprintf("%s (%d)%s", base, i, ext)
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}
