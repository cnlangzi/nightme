//go:build windows

package stt

import (
	"fmt"
	"os/exec"
	"strings"
)

// isWindowsExe is true on Windows; see kill_unix.go for the
// other half of this build-tag pair.
var isWindowsExe = true

// killByName terminates any running process whose command line
// contains path. Used by the install / upgrade path to evict a
// stale worker before overwriting its binary on disk; the
// execHandle spawned by ProductionSpawner is the canonical
// shutdown path during normal operation.
//
// On Windows we shell out to PowerShell's Stop-Process via a
// CIM cmdlet — taskkill /F works too but does not match by
// command line substring (it matches by image basename). A
// path substring match avoids killing unrelated processes that
// happen to share the image name.
func killByName(path string) error {
	if path == "" {
		return fmt.Errorf("stt: killByName: empty path")
	}
	// PowerShell: Get-CimInstance Win32_Process | Where-Object
	// { $_.CommandLine -like '*<path>*' } | ForEach-Object {
	// Stop-Process -Id $_.ProcessId -Force }
	//
	// Stop-Process on a non-existent PID throws; we swallow
	// the "cannot find process" path with -ErrorAction
	// SilentlyContinue so first-time installs run cleanly.
	ps := fmt.Sprintf(
		`Get-CimInstance Win32_Process -Filter "Name='nightme-stt.exe'" `+
			`| Where-Object { $_.CommandLine -like '*%s*' } `+
			`| ForEach-Object { Stop-Process -Id $_.ProcessId -Force -ErrorAction SilentlyContinue }`,
		strings.ReplaceAll(path, "'", "''"),
	)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("stt: powershell kill %s: %w (%s)", path, err, strings.TrimSpace(string(out)))
	}
	return nil
}
