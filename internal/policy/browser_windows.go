//go:build windows

package policy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/windows/registry"
)

// browserDef describes how to locate and identify a Chromium-based browser.
type browserDef struct {
	name    string // friendly name
	process string // image name, e.g. chrome.exe
	relPath string // path under Program Files / Local AppData
}

var browserDefs = []browserDef{
	{name: "Chrome", process: "chrome.exe", relPath: `Google\Chrome\Application\chrome.exe`},
	{name: "Edge", process: "msedge.exe", relPath: `Microsoft\Edge\Application\msedge.exe`},
}

// InstalledBrowsers returns the friendly names of supported browsers that are
// installed (executable found), regardless of whether they are running.
func InstalledBrowsers() []string {
	var out []string
	for _, b := range browserDefs {
		if locateBrowser(b) != "" {
			out = append(out, b.name)
		}
	}
	return out
}

// OpenExtensionsPages opens the extensions management page in the requested
// browsers (by friendly name, e.g. "Chrome"). If names is empty, all installed
// browsers are used. Returns the names actually opened.
func OpenExtensionsPages(names []string) []string {
	want := func(name string) bool {
		if len(names) == 0 {
			return true
		}
		for _, n := range names {
			if strings.EqualFold(n, name) {
				return true
			}
		}
		return false
	}
	var opened []string
	for _, b := range browserDefs {
		if !want(b.name) {
			continue
		}
		exe := locateBrowser(b)
		if exe == "" {
			continue
		}
		url := "chrome://extensions/"
		if b.name == "Edge" {
			url = "edge://extensions/"
		}
		// --new-window makes the internal URL navigate reliably even when the
		// browser is already running.
		if err := exec.Command(exe, "--new-window", url).Start(); err == nil {
			opened = append(opened, b.name)
		}
	}
	return opened
}

// RunningBrowsers returns the friendly names of supported browsers currently
// running (Chrome / Edge).
func RunningBrowsers() []string {
	var out []string
	for _, b := range browserDefs {
		if processRunning(b.process) {
			out = append(out, b.name)
		}
	}
	return out
}

// RestartBrowsers gracefully closes and relaunches every supported browser that
// is currently running, so the extension policy takes effect. Returns the names
// restarted. Browsers that are not running are left untouched.
func RestartBrowsers() ([]string, error) {
	var restarted []string
	for _, b := range browserDefs {
		if !processRunning(b.process) {
			continue
		}
		exe := locateBrowser(b)
		if exe == "" {
			return restarted, fmt.Errorf("找不到 %s 的可执行文件", b.name)
		}
		// Ask the browser to close (sends WM_CLOSE, a clean shutdown).
		_ = exec.Command("taskkill", "/IM", b.process).Run()
		// Wait for it to exit (up to ~6s), then force if still alive.
		deadline := time.Now().Add(6 * time.Second)
		for processRunning(b.process) && time.Now().Before(deadline) {
			time.Sleep(200 * time.Millisecond)
		}
		if processRunning(b.process) {
			_ = exec.Command("taskkill", "/IM", b.process, "/T", "/F").Run()
			time.Sleep(500 * time.Millisecond)
		}
		// Relaunch detached.
		if err := exec.Command(exe).Start(); err != nil {
			return restarted, fmt.Errorf("重启 %s 失败: %w", b.name, err)
		}
		restarted = append(restarted, b.name)
	}
	return restarted, nil
}

// locateBrowser resolves the browser executable via the App Paths registry key,
// falling back to the standard install locations.
func locateBrowser(b browserDef) string {
	if p := appPath(b.process); p != "" {
		return p
	}
	roots := []string{
		os.Getenv("ProgramFiles"),
		os.Getenv("ProgramFiles(x86)"),
		filepath.Join(os.Getenv("LocalAppData")),
	}
	for _, root := range roots {
		if root == "" {
			continue
		}
		candidate := filepath.Join(root, b.relPath)
		if _, err := os.Stat(candidate); err == nil {
			return candidate
		}
	}
	return ""
}

// appPath reads HKLM\...\App Paths\<exe> default value.
func appPath(exe string) string {
	key := `SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\` + exe
	for _, root := range []registry.Key{registry.LOCAL_MACHINE, registry.CURRENT_USER} {
		k, err := registry.OpenKey(root, key, registry.QUERY_VALUE)
		if err != nil {
			continue
		}
		v, _, err := k.GetStringValue("")
		k.Close()
		if err == nil && v != "" {
			if _, statErr := os.Stat(v); statErr == nil {
				return v
			}
		}
	}
	return ""
}

func processRunning(image string) bool {
	out, err := exec.Command("tasklist", "/FI", "IMAGENAME eq "+image, "/NH").Output()
	if err != nil {
		return false
	}
	return strings.Contains(strings.ToLower(string(out)), strings.ToLower(image))
}
