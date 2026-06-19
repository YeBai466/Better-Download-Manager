package main

import (
	"embed"
	"io/fs"
	"log"
	"os"
	"path/filepath"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"

	"github.com/yebai/b-download-manager/internal/service"
)

//go:embed all:frontend/dist
var assets embed.FS

//go:embed all:extensions/chromium
var extAssets embed.FS

//go:embed build/appicon.png
var trayIcon []byte

func init() {
	// Register frontend events so the binding generator produces typed APIs.
	application.RegisterEvent[any](service.EventTaskUpdate)
	application.RegisterEvent[string](service.EventTaskRemoved)
	application.RegisterEvent[any](service.EventTakeoverRequest)
}

func main() {
	extFiles, err := fs.Sub(extAssets, "extensions/chromium")
	if err != nil {
		log.Fatal("load extension assets:", err)
	}
	svc, err := service.New(databasePath(), extFiles)
	if err != nil {
		log.Fatal("init service:", err)
	}

	app := application.New(application.Options{
		Name:        "B Download Manager",
		Description: "仿 IDM 多线程下载器",
		Services: []application.Service{
			application.NewService(svc),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		// Only one instance may run: it owns the takeover port and tray. A second
		// launch focuses the existing window and exits instead of conflicting.
		SingleInstance: &application.SingleInstanceOptions{
			UniqueID: "com.yebai.bdownloadmanager",
			OnSecondInstanceLaunch: func(application.SecondInstanceData) {
				if a := application.Get(); a != nil {
					if w, ok := a.Window.Get(service.MainWindowName); ok {
						w.Show()
						w.Focus()
					}
				}
			},
		},
	})

	window := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Name:             service.MainWindowName,
		Title:            "B Download Manager",
		Width:            1080,
		Height:           680,
		MinWidth:         860,
		MinHeight:        520,
		Hidden:           startMinimized(),
		BackgroundColour: application.NewRGB(245, 246, 248),
		URL:              "/",
	})

	// Minimise to the system tray instead of quitting when the window closes.
	window.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		e.Cancel()
		window.Hide()
	})

	setupTray(app, window)

	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}

func setupTray(app *application.App, window application.Window) {
	tray := app.SystemTray.New()
	if len(trayIcon) > 0 {
		tray.SetIcon(trayIcon)
	}
	tray.SetTooltip("B Download Manager")

	menu := application.NewMenu()
	menu.Add("显示主窗口").OnClick(func(*application.Context) {
		window.Show()
		window.Focus()
	})
	menu.AddSeparator()
	menu.Add("退出").OnClick(func(*application.Context) {
		app.Quit()
	})
	tray.SetMenu(menu)
	tray.OnClick(func() {
		window.Show()
		window.Focus()
	})
}

// startMinimized reports whether the app was launched with --minimized (used by
// the login autostart entry so it starts hidden in the tray).
func startMinimized() bool {
	for _, a := range os.Args[1:] {
		if a == "--minimized" || a == "-minimized" {
			return true
		}
	}
	return false
}

// databasePath returns the path to the SQLite database in the user's config dir.
func databasePath() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}
	appDir := filepath.Join(dir, "BDownloadManager")
	_ = os.MkdirAll(appDir, 0o755)
	return filepath.Join(appDir, "bdm.db")
}
