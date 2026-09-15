//go:build windows

package stt

import (
	"fmt"
	"os/exec"
	"strings"
)

// killByName sends WM_CLOSE + force-kill to every process
// whose ImageName or CommandLine matches `name`. The
// PowerShell pipeline keeps us out of WMI / PSAPI and
// stays self-contained on the GitHub-hosted windows
// runner (no extra binaries required).
//
// `taskkill /IM nightme-stt.exe /F /T` works when the
// image name is nightme-stt.exe (Windows binary name).
// We accept the nightme-stt token to match both exe and
// any cmdline that contains the substring.
func killByName(name string) {
	// taskkill wants a basename. If the caller passed
	// "nightme-stt", we map to "nightme-stt.exe".
	img := name
	if !strings.HasSuffix(strings.ToLower(img), ".exe") {
		img += ".exe"
	}
	ps := fmt.Sprintf(`Get-Process -ErrorAction SilentlyContinue | Where-Object { $_.ProcessName -eq '%s' -or ($_.CommandLine -like '*%s*') } | Stop-Process -Force`, img, name)
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", ps)
	_ = cmd.Run()
}
