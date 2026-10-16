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
	"Chrome": `SOFTWARE\Policies\Google\Chrome\ExtensionInstallForcelist`,
	"Edge":   `SOFTWARE\Policies\Microsoft\Edge\ExtensionInstallForcelist`,
}

// StoreUpdateURL is the Chrome Web Store update manifest. Extensions published
// on the store use this URL; force-installing a store extension via policy works
// even on unmanaged (consumer) machines, unlike self-hosted off-store ones.
const StoreUpdateURL = "https://clients2.google.com/service/update2/crx"

// Status reports, per browser, whether our extension id is force-installed.
type Status struct {
	Chrome bool `json:"chrome"`
	Edge   bool `json:"edge"`
}

// GetStatus reads the policy registry (no elevation required).
func GetStatus(id string) Status {
	return Status{
		Chrome: hasForcedEntry(forcelistKeys["Chrome"], id),
		Edge:   hasForcedEntry(forcelistKeys["Edge"], id),
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

// Install writes the force-install entry for the given browsers (by friendly
// name, e.g. "Chrome"/"Edge"; empty = all supported) with one UAC elevation.
// updateURL is the Omaha update manifest URL; for a Web Store extension use
// StoreUpdateURL. The forcelist value is "<id>;<updateURL>".
func Install(id, updateURL string, browsers []string) error {
	if updateURL == "" {
		updateURL = StoreUpdateURL
	}
	val := fmt.Sprintf("%s;%s", id, updateURL)
	var b strings.Builder
	b.WriteString("$ErrorActionPreference='Stop'\n")
	for name, key := range forcelistKeys {
		if !wantBrowser(browsers, name) {
			continue
		}
		fmt.Fprintf(&b, "New-Item -Path 'HKLM:\\%s' -Force | Out-Null\n", key)
		// Use the extension id as the value name so repeated installs are
		// idempotent and per-extension (instead of a fixed "1" slot).
		fmt.Fprintf(&b, "New-ItemProperty -Path 'HKLM:\\%s' -Name '%s' -Value '%s' -PropertyType String -Force | Out-Null\n", key, id, val)
	}
	return runElevated(b.String())
}

// Uninstall removes the force-install entry for the given browsers (empty = all).
func Uninstall(id string, browsers []string) error {
	return runElevated(uninstallScript(id, browsers))
}
func uninstallScript(id string, browsers []string) string {
	var b strings.Builder
	for name, key := range forcelistKeys {
		if !wantBrowser(browsers, name) {
			continue
		}
		fmt.Fprintf(&b, "$key=Get-Item -LiteralPath 'HKLM:\\%s' -ErrorAction SilentlyContinue\nif ($key) { foreach ($name in $key.GetValueNames()) { if ([string]$key.GetValue($name) -like '%s;*') { Remove-ItemProperty -LiteralPath 'HKLM:\\%s' -Name $name -ErrorAction Stop } } }\n", key, id, key)
	}
	return b.String()
}

func wantBrowser(browsers []string, name string) bool {
	if len(browsers) == 0 {
		return true
	}
	for _, b := range browsers {
		if strings.EqualFold(b, name) {
			return true
		}
	}
	return false
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
	if _, err := tmp.WriteString("$ErrorActionPreference='Stop'\ntry {\n" + script + "\n} catch { Write-Error $_; exit 1 }\nexit 0\n"); err != nil {
		tmp.Close()
		return err
	}
	tmp.Close()

	psCmd := fmt.Sprintf(
		"$p=Start-Process -FilePath powershell -Verb RunAs -Wait -PassThru -WindowStyle Hidden -ArgumentList '-NoProfile','-ExecutionPolicy','Bypass','-File','%s'; exit $p.ExitCode",
		strings.ReplaceAll(filepath.Clean(path), "'", "''"),
	)
	cmd := exec.Command("powershell", "-NoProfile", "-Command", psCmd)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("elevated policy update failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
