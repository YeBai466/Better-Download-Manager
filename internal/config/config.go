// Package config defines the application settings model and defaults.
package config

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/yebai/better-download-manager/internal/proxy"
)

// TakeoverAction decides what happens when the browser extension hands a URL to
// the app.
type TakeoverAction string

const (
	ActionShowDialog TakeoverAction = "dialog" // show the IDM-style add dialog (default)
	ActionAutoStart  TakeoverAction = "auto"   // start the download immediately
)

// Settings is the full, serializable application configuration.
type Settings struct {
	AdaptiveConnections bool              `json:"adaptiveConnections"`
	DownloadDir         string            `json:"downloadDir"`
	Categorize          bool              `json:"categorize"`    // route files into category subfolders
	CategoryDirs        map[string]string `json:"categoryDirs"`  // per-category overrides (abs paths)
	MaxConcurrent       int               `json:"maxConcurrent"` // simultaneous downloads
	Connections         int               `json:"connections"`   // connections per download
	SpeedLimit          int64             `json:"speedLimit"`    // bytes/sec, 0 = unlimited
	Proxy               proxy.Settings    `json:"proxy"`
	TakeoverEnabled     bool              `json:"takeoverEnabled"` // run the local browser-integration server
	TakeoverPort        int               `json:"takeoverPort"`
	TakeoverAction      TakeoverAction    `json:"takeoverAction"`
	ExtPromptIgnored    bool              `json:"extPromptIgnored"` // user chose "ignore forever" for the install prompt
	AutoStart           bool              `json:"autoStart"`        // launch at Windows login
	StartMinimized      bool              `json:"startMinimized"`   // when autostarted, start hidden in tray
	AutoCheckUpdate     bool              `json:"autoCheckUpdate"`  // check GitHub releases on startup
	Language            string            `json:"language"`         // UI language: "zh" or "en"
}

// DefaultDownloadDir returns the user's Downloads folder, falling back to the
// home directory or the working directory.
func DefaultDownloadDir() string {
	if home, err := os.UserHomeDir(); err == nil {
		dl := filepath.Join(home, "Downloads")
		return dl
	}
	if wd, err := os.Getwd(); err == nil {
		return wd
	}
	return "."
}

// Default returns the out-of-the-box settings.
func Default() Settings {
	return Settings{
		AdaptiveConnections: true,
		DownloadDir:         DefaultDownloadDir(),
		Categorize:          true,
		CategoryDirs:        map[string]string{},
		MaxConcurrent:       5,
		Connections:         8,
		SpeedLimit:          0,
		Proxy:               proxy.DefaultSettings(),
		TakeoverEnabled:     true,
		TakeoverPort:        9614,
		TakeoverAction:      ActionShowDialog,
		ExtPromptIgnored:    false,
		AutoStart:           false,
		StartMinimized:      true,
		AutoCheckUpdate:     true,
		Language:            "zh",
	}
}

// Normalize fills in any zero-valued fields with defaults so older or partial
// settings remain valid.
func (s *Settings) Normalize() {
	d := Default()
	if s.DownloadDir == "" {
		s.DownloadDir = d.DownloadDir
	}
	if s.CategoryDirs == nil {
		s.CategoryDirs = map[string]string{}
	}
	if s.MaxConcurrent > 20 {
		s.MaxConcurrent = 20
	}
	if s.Connections > 32 {
		s.Connections = 32
	}
	if s.TakeoverPort > 65535 {
		s.TakeoverPort = d.TakeoverPort
	}
	if s.SpeedLimit < 0 {
		s.SpeedLimit = 0
	}
	if s.MaxConcurrent < 1 {
		s.MaxConcurrent = d.MaxConcurrent
	}
	if s.Connections < 1 {
		s.Connections = d.Connections
	}
	if s.TakeoverPort < 1 {
		s.TakeoverPort = d.TakeoverPort
	}
	if s.TakeoverAction == "" {
		s.TakeoverAction = d.TakeoverAction
	}
	if s.Proxy.Mode == "" {
		s.Proxy.Mode = proxy.ModeSystem
	}
	if s.Language != "en" {
		s.Language = "zh"
	}
}

// Validate runs before normalization at public write boundaries.
func (s Settings) Validate() error {
	if s.MaxConcurrent < 1 || s.MaxConcurrent > 20 {
		return fmt.Errorf("concurrent downloads must be 1–20")
	}
	if s.Connections < 1 || s.Connections > 32 {
		return fmt.Errorf("connections must be 1–32")
	}
	if s.SpeedLimit < 0 {
		return fmt.Errorf("speed limit must be nonnegative")
	}
	if s.TakeoverPort < 1 || s.TakeoverPort > 65535 {
		return fmt.Errorf("port must be 1–65535")
	}
	if s.TakeoverAction != ActionShowDialog && s.TakeoverAction != ActionAutoStart {
		return fmt.Errorf("invalid takeover action")
	}
	return s.Proxy.Validate()
}
func (s Settings) Clone() Settings {
	copy := s
	copy.CategoryDirs = make(map[string]string, len(s.CategoryDirs))
	for k, v := range s.CategoryDirs {
		copy.CategoryDirs[k] = v
	}
	return copy
}
