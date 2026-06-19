//go:build windows

// Package policy force-installs the browser extension via Chrome/Edge
// enterprise policy registry keys under HKLM. Writing requires administrator
// rights, so Install/Uninstall relaunch an elevated PowerShell (one UAC
// prompt); Status only reads and needs no elevation.
package policy

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows/registry"
)

// forcelistKeys are the per-browser policy paths holding the force-install list.
var forcelistKeys = map[string]string{
	"chrome": `SOFTWARE\Policies\Google\Chrome\ExtensionInstallForcelist`,
	"edge":   `SOFTWARE\Policies\Microsoft\Edge\ExtensionInstallForcelist`,
}

// Status reports, per browser, whether our extension id is force-installed.
type Status struct {
	Chrome bool `json:"chrome"`
	Edge   bool `json:"edge"`
}

// GetStatus reads the policy registry (no elevation required).
func GetStatus(id string) Status {
	return Status{
		Chrome: hasForcedEntry(forcelistKeys["chrome"], id),
		Edge:   hasForcedEntry(forcelistKeys["edge"], id),
	}
}

func hasForcedEntry(keyPath, id string) bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, keyPath, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	names, err := k.ReadValueNames(0)
	if err != nil {
		return false
	}
	for _, n := range names {
		if v, _, err := k.GetStringValue(n); err == nil {
			if strings.HasPrefix(v, id+";") {
				return true
			}
		}
	}
	return false
}

// Install writes the force-install entry for both browsers with one elevation.
// updateURL is the Omaha update manifest URL the local server serves.
func Install(id, updateURL string) error {
	val := fmt.Sprintf("%s;%s", id, updateURL)
	var b strings.Builder
	b.WriteString("$ErrorActionPreference='Stop'\n")
	for _, key := range forcelistKeys {
		fmt.Fprintf(&b, "New-Item -Path 'HKLM:\\%s' -Force | Out-Null\n", key)
		fmt.Fprintf(&b, "New-ItemProperty -Path 'HKLM:\\%s' -Name '1' -Value '%s' -PropertyType String -Force | Out-Null\n", key, val)
	}
	return runElevated(b.String())
}

// Uninstall removes the force-install entries for both browsers.
func Uninstall(id string) error {
	var b strings.Builder
	for _, key := range forcelistKeys {
		fmt.Fprintf(&b, "Remove-ItemProperty -Path 'HKLM:\\%s' -Name '1' -ErrorAction SilentlyContinue\n", key)
	}
	return runElevated(b.String())
}

// runElevated writes script to a temp .ps1 and runs it via an elevated
// PowerShell, waiting for completion.
func runElevated(script string) error {
	tmp, err := os.CreateTemp("", "bdm-policy-*.ps1")
	if err != nil {
		return err
	}
	path := tmp.Name()
	defer os.Remove(path)
	if _, err := tmp.WriteString(script); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	psCmd := fmt.Sprintf(
		"Start-Process -FilePath powershell -Verb RunAs -Wait -WindowStyle Hidden -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File','%s'",
		filepath.Clean(path),
	)
	cmd := exec.Command("powershell", "-NoProfile", "-Command", psCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("elevated policy update failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
