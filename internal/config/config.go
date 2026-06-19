// Package config defines the application settings model and defaults.
package config

import (
	"os"
	"path/filepath"

	"github.com/yebai/b-download-manager/internal/proxy"
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
	DownloadDir     string            `json:"downloadDir"`
	Categorize      bool              `json:"categorize"`      // route files into category subfolders
	CategoryDirs    map[string]string `json:"categoryDirs"`    // per-category overrides (abs paths)
	MaxConcurrent   int               `json:"maxConcurrent"`   // simultaneous downloads
	Connections     int               `json:"connections"`     // connections per download
	SpeedLimit      int64             `json:"speedLimit"`      // bytes/sec, 0 = unlimited
	Proxy           proxy.Settings    `json:"proxy"`
	TakeoverEnabled  bool             `json:"takeoverEnabled"`  // run the local browser-integration server
	TakeoverPort     int              `json:"takeoverPort"`
	TakeoverAction   TakeoverAction   `json:"takeoverAction"`
	ExtPromptIgnored bool             `json:"extPromptIgnored"` // user chose "ignore forever" for the install prompt
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
		DownloadDir:     DefaultDownloadDir(),
		Categorize:      true,
		CategoryDirs:    map[string]string{},
		MaxConcurrent:   5,
		Connections:     8,
		SpeedLimit:      0,
		Proxy:           proxy.DefaultSettings(),
		TakeoverEnabled:  true,
		TakeoverPort:     9614,
		TakeoverAction:   ActionShowDialog,
		ExtPromptIgnored: false,
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
}
