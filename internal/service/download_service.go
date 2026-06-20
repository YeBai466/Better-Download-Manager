// Package service implements the Wails-bound application services that expose
// the download engine, settings and browser integration to the frontend.
package service

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/application"

	cat "github.com/yebai/better-download-manager/internal/category"
	"github.com/yebai/better-download-manager/internal/config"
	"github.com/yebai/better-download-manager/internal/downloader"
	"github.com/yebai/better-download-manager/internal/httpclient"
	"github.com/yebai/better-download-manager/internal/policy"
	"github.com/yebai/better-download-manager/internal/proxy"
	"github.com/yebai/better-download-manager/internal/store"
	"github.com/yebai/better-download-manager/internal/takeover"
	"github.com/yebai/better-download-manager/internal/updates"
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

// Add-download windows are created on demand with unique names ("add-N") so any
// number of downloads can have their own independent window (IDM-style).

// Version is the application version reported to the browser extension.
const Version = "1.1.2"

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

	mu          sync.RWMutex
	settings    config.Settings
	pendingAdds map[string]AddPrefill // per-window prefill, keyed by the add window's unique name
	addSeq      int64                 // monotonic counter for unique add-window names

	clientMu sync.Mutex
	clients  map[proxy.Settings]*http.Client // cached, proxy-aware clients keyed by task proxy settings
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
	s := &DownloadService{store: st, settings: settings, extFiles: extFiles, keyDir: filepath.Dir(dbPath), pendingAdds: make(map[string]AddPrefill)}

	s.engine = downloader.NewEngine(downloader.Config{
		MaxConcurrent:  settings.MaxConcurrent,
		MaxConnections: settings.MaxConcurrent * settings.Connections,
		SpeedLimit:     settings.SpeedLimit,
		ClientFactory:  s.newClientForProxy,
		OnUpdate:       s.onTaskUpdate,
		OnPersist:      s.onTaskPersist,
		OnRemoved:      s.onTaskRemoved,
	})
	s.takeover = takeover.New(Version, s.onTakeover)
	return s, nil
}

// --- Wails service lifecycle ---

// ServiceStartup restores persisted tasks and starts the takeover server.
func (s *DownloadService) ServiceStartup(ctx context.Context, _ application.ServiceOptions) error {
	// Wipe any installer left in the update tmp folder by a previous in-app
	// update (completed or cancelled) so it doesn't linger in the install dir.
	s.CleanupUpdateTmp()

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

// newClient returns a shared, proxy-aware HTTP client. The client (and its
// connection pool) is cached and reused across tasks so segment workers can
// reuse keep-alive connections instead of opening fresh TCP+TLS connections
// every time — which is a large part of the "slow to start" delay. The cache is
// invalidated by invalidateClient when proxy settings change.
func (s *DownloadService) newClient() *http.Client {
	s.mu.RLock()
	p := s.settings.Proxy
	s.mu.RUnlock()
	return s.newClientForProxy(p)
}

func (s *DownloadService) newClientForProxy(p proxy.Settings) *http.Client {
	if p.Mode == "" {
		p.Mode = proxy.ModeSystem
	}
	s.clientMu.Lock()
	defer s.clientMu.Unlock()
	if s.clients == nil {
		s.clients = map[proxy.Settings]*http.Client{}
	}
	if c := s.clients[p]; c != nil {
		return c
	}
	c, err := httpclient.New(p)
	if err != nil {
		return &http.Client{}
	}
	s.clients[p] = c
	return c
}

// invalidateClient drops the cached client so the next download rebuilds it with
// fresh proxy settings. Idle connections on the old client are closed.
func (s *DownloadService) invalidateClient() {
	s.clientMu.Lock()
	old := s.clients
	s.clients = nil
	s.clientMu.Unlock()
	for _, c := range old {
		if c != nil {
			c.CloseIdleConnections()
		}
	}
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
	URL           string            `json:"url"`
	Filename      string            `json:"filename"`
	Category      string            `json:"category"`
	SaveDir       string            `json:"saveDir"`
	Connections   int               `json:"connections"`
	Headers       map[string]string `json:"headers"`
	Proxy         proxy.Settings    `json:"proxy"`
	RememberProxy bool              `json:"rememberProxy"`
	AutoStart     bool              `json:"autoStart"`
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
	taskProxy := s.normalizeTaskProxy(req.Proxy)
	if req.RememberProxy {
		if err := s.saveProxySettings(taskProxy); err != nil {
			return downloader.TaskInfo{}, err
		}
	}

	return s.engine.Add(downloader.AddOptions{
		ID:          newID(),
		URL:         req.URL,
		Filename:    filename,
		SavePath:    savePath,
		Category:    category,
		Connections: conns,
		Headers:     req.Headers,
		Proxy:       taskProxy,
		AutoStart:   req.AutoStart,
	})
}

// ShowAddWindow opens (or focuses) the separate add-download window, prefilled
// with the given data. Called from the main UI's "Add URL" button.
func (s *DownloadService) ShowAddWindow(p AddPrefill) {
	s.openAddWindow(p)
}

// ConsumePendingAdd returns and clears the prefill for a specific add window
// (called by that window when it loads, identified by its unique name).
func (s *DownloadService) ConsumePendingAdd(window string) AddPrefill {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.pendingAdds[window]
	delete(s.pendingAdds, window)
	return p
}

// openAddWindow stores the prefill and shows a fresh, independent add-download
// window. Every call creates its own uniquely-named window so any number of
// downloads can run concurrently (IDM-style) — a completed window never blocks
// a new one. Prefill is keyed by the window name so concurrent opens don't race.
func (s *DownloadService) openAddWindow(p AddPrefill) {
	s.mu.Lock()
	name := fmt.Sprintf("add-%d", atomic.AddInt64(&s.addSeq, 1))
	s.pendingAdds[name] = p
	s.mu.Unlock()

	app := application.Get()
	if app == nil {
		return
	}
	w := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             name,
		Title:            s.tr("添加下载", "Add Download"),
		Width:            560,
		Height:           500,
		MinWidth:         460,
		MinHeight:        420,
		AlwaysOnTop:      true,
		BackgroundColour: application.NewRGB(245, 246, 248),
		URL:              "/?view=add&w=" + name,
	})
	w.Show()
	w.Focus()
	go resetTopmost(w)
}

func resetTopmost(w application.Window) {
	time.Sleep(1200 * time.Millisecond)
	w.SetAlwaysOnTop(false)
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
		if t.Status == downloader.StatusDownloading || t.Status == downloader.StatusConnecting || t.Status == downloader.StatusQueued {
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

type ProbeRequest struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	Proxy   proxy.Settings    `json:"proxy"`
}

// ProbeURL fetches metadata for a URL so the add dialog can show file info.
func (s *DownloadService) ProbeURL(req ProbeRequest) (Preview, error) {
	url := strings.TrimSpace(req.URL)
	if url == "" {
		return Preview{}, fmt.Errorf("url is required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	res, err := downloader.Probe(ctx, s.newClientForProxy(s.normalizeTaskProxy(req.Proxy)), url, req.Headers)
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

// tr returns the English string when the saved UI language is "en", otherwise
// the Chinese default. Used for native chrome (window titles, dialogs).
func (s *DownloadService) tr(zh, en string) string {
	s.mu.RLock()
	lang := s.settings.Language
	s.mu.RUnlock()
	if lang == "en" {
		return en
	}
	return zh
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
	s.engine.UpdateRuntime(downloader.RuntimeConfig{
		MaxConcurrent:  cfg.MaxConcurrent,
		MaxConnections: cfg.MaxConcurrent * cfg.Connections,
		SpeedLimit:     cfg.SpeedLimit,
	})
	s.invalidateClient() // proxy may have changed; rebuild the pooled client

	if cfg.TakeoverEnabled {
		_ = s.takeover.Start(cfg.TakeoverPort)
	} else {
		_ = s.takeover.Stop()
	}
	s.applyAutostart(cfg.AutoStart)
	return cfg, nil
}

func (s *DownloadService) normalizeTaskProxy(p proxy.Settings) proxy.Settings {
	if p.Mode == "" {
		s.mu.RLock()
		p = s.settings.Proxy
		s.mu.RUnlock()
	}
	if p.Mode == "" {
		p.Mode = proxy.ModeSystem
	}
	return p
}

func (s *DownloadService) saveProxySettings(p proxy.Settings) error {
	s.mu.RLock()
	cfg := s.settings
	s.mu.RUnlock()
	cfg.Proxy = p
	cfg.Normalize()
	if err := s.store.SaveSettings(cfg); err != nil {
		return err
	}
	s.mu.Lock()
	s.settings = cfg
	s.mu.Unlock()
	s.engine.UpdateRuntime(downloader.RuntimeConfig{
		MaxConcurrent:  cfg.MaxConcurrent,
		MaxConnections: cfg.MaxConcurrent * cfg.Connections,
		SpeedLimit:     cfg.SpeedLimit,
	})
	s.invalidateClient()
	return nil
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

// DownloadUpdate downloads the new release's installer directly into a "tmp"
// folder under the app's own install directory (NOT as a regular download task),
// then launches it through the shell so Windows shows the UAC prompt the
// admin-level NSIS installer requires. The installer closes the running app
// itself (taskkill) and upgrades in place; on the next launch the updated app
// wipes the leftover tmp folder via CleanupUpdateTmp. Returns an error (so the
// UI can fall back to the release page) when no installer URL is available or
// the download/launch fails. Blocks until the installer has been launched.
func (s *DownloadService) DownloadUpdate(downloadURL string) error {
	url := strings.TrimSpace(downloadURL)
	if url == "" {
		return fmt.Errorf("没有可用的安装包下载地址")
	}

	tmpDir, err := s.updateTmpDir()
	if err != nil {
		return err
	}
	// Start from a clean slate so a previous, interrupted attempt can't leave a
	// stale or partial installer behind.
	_ = os.RemoveAll(tmpDir)
	if err := os.MkdirAll(tmpDir, 0o755); err != nil {
		return fmt.Errorf("创建临时目录失败: %w", err)
	}
	dest := filepath.Join(tmpDir, installerName(url))

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	if err := s.downloadFile(ctx, url, dest); err != nil {
		_ = os.RemoveAll(tmpDir)
		return fmt.Errorf("下载安装包失败: %w", err)
	}

	if err := openInShell(dest); err != nil {
		return fmt.Errorf("启动安装程序失败: %w", err)
	}
	return nil
}

// updateTmpDir returns "<installDir>/tmp" — the folder update installers are
// downloaded into. The install directory is the directory of the running
// executable.
func (s *DownloadService) updateTmpDir() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("无法定位程序目录: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), "tmp"), nil
}

// CleanupUpdateTmp removes the update "tmp" folder (and any installer left in
// it). It runs on startup so a completed or cancelled in-app update leaves
// nothing behind in the install directory.
func (s *DownloadService) CleanupUpdateTmp() {
	if dir, err := s.updateTmpDir(); err == nil {
		_ = os.RemoveAll(dir)
	}
}

// installerName derives a safe ".exe" file name from the download URL, falling
// back to a generic name when the URL has no usable .exe basename.
func installerName(rawURL string) string {
	name := rawURL
	if i := strings.IndexAny(name, "?#"); i >= 0 {
		name = name[:i]
	}
	name = path.Base(name)
	if name == "" || name == "." || name == "/" || !strings.HasSuffix(strings.ToLower(name), ".exe") {
		return "update-installer.exe"
	}
	return filepath.Base(name)
}

// downloadFile streams url to dest using the configured proxy-aware client.
func (s *DownloadService) downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "BetterDownloadManager-Updater")

	s.mu.RLock()
	px := s.settings.Proxy
	s.mu.RUnlock()

	resp, err := s.newClientForProxy(px).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("服务器返回 %s", resp.Status)
	}

	f, err := os.Create(dest)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		_ = os.Remove(dest)
		return err
	}
	return f.Close()
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

func (s *DownloadService) BrowserExtensionConfigured() bool {
	st := policy.GetStatus(StoreExtensionID)
	return st.Chrome || st.Edge
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
		SetTitle(s.tr("选择保存目录", "Choose Save Folder")).
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
