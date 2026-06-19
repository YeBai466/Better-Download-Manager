//go:build !windows

// Package policy stub for non-Windows builds (the app targets Windows only).
package policy

import "errors"

type Status struct {
	Chrome bool `json:"chrome"`
	Edge   bool `json:"edge"`
}

func GetStatus(id string) Status         { return Status{} }
func Install(id, updateURL string) error { return errors.New("policy install is only supported on Windows") }
func Uninstall(id string) error          { return errors.New("policy uninstall is only supported on Windows") }
func RunningBrowsers() []string             { return nil }
func InstalledBrowsers() []string           { return nil }
func OpenExtensionsPages(names []string) []string { return nil }
func RestartBrowsers() ([]string, error) {
	return nil, errors.New("restarting browsers is only supported on Windows")
}
