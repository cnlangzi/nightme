//go:build windows

// Package main — process termination primitives for `nightme kill`
// on Windows.
package main

import (
	"errors"
	"fmt"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/cnlangzi/nightme/internal/pathutil"
)

// errProcessGone marks a PID that no longer exists. Windows has no
// ESRCH, so we synthesize the same signal for the shared kill path.
var errProcessGone = errors.New("process not found")

// killProcess terminates pid via the Win32 API.
//
// Windows has no POSIX signals, so the SIGTERM-then-SIGKILL grace
// window has no equivalent: TerminateProcess is the only portable
// way to stop an arbitrary child, and the `--force` flag is
// therefore a no-op here. This mirrors internal/agentsession's
// reapOrphan, which makes the same trade-off.
//
// ERROR_INVALID_PARAMETER from OpenProcess means the PID does not
// exist — reported as errProcessGone so the caller prints "already
// exited" instead of a failure. Other OpenProcess errors (notably
// ERROR_ACCESS_DENIED, i.e. a live process we may not touch) are
// surfaced as real errors.
func killProcess(pid int, _ bool) error {
	if pid <= 0 {
		return errProcessGone
	}
	h, err := windows.OpenProcess(windows.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) {
			return errProcessGone
		}
		return fmt.Errorf("open pid %d: %w", pid, err)
	}
	defer windows.CloseHandle(h)

	if err := windows.TerminateProcess(h, 1); err != nil {
		return fmt.Errorf("terminate pid %d: %w", pid, err)
	}
	return nil
}

// isProcessGone reports whether err means "the process no longer
// exists", which kill treats as success.
func isProcessGone(err error) bool {
	return errors.Is(err, errProcessGone)
}

// verifyPIDOwner reports whether pid still belongs to wantCommand,
// returning the executable name it actually found.
//
// It is the Windows counterpart of the Unix PID-identity check and
// follows the same fail-OPEN policy — ok=true whenever we cannot get
// a definite contradiction:
//
//   - wantCommand == "" (unknown agent): nothing to compare against.
//   - the image name could not be read (snapshot failed, PID gone):
//     killProcess handles the gone case as "already exited", and a
//     failed probe must not disable the command.
//   - the PID belongs to the host interpreter that runs the agent
//     shim: see hostInterpreterFor.
//
// Only a positive mismatch ("this PID is codex.exe, you expected
// claude") blocks the kill.
func verifyPIDOwner(pid int, wantCommand string) (string, bool) {
	if wantCommand == "" {
		return "", true
	}
	actual := processImageName(pid)
	if actual == "" {
		return "", true
	}
	if host := hostInterpreterFor(wantCommand); host != "" && strings.EqualFold(actual, host) {
		return actual, true
	}
	return actual, sameCommandFold(actual, wantCommand)
}

// hostInterpreterFor returns the executable that nightme's spawn path
// starts INSTEAD of the agent binary for a command with the given
// name, or "" when the agent binary itself is the spawned process.
//
// internal/proc's launchOnWindowsWith routes by the resolved file's
// extension: a .cmd/.bat runs under cmd.exe, a .ps1 under
// powershell.exe and a .js under node.exe. For those, the PID recorded
// in agent_sessions.json belongs to the host, not to the agent — the
// image name cannot say WHICH agent the session runs, so ownership is
// undecidable and the check must fail open. Refusing there would make
// `nightme kill` skip every npm-installed agent on Windows, i.e.
// useless.
//
// The host is derived from the configured command, not from a fixed
// list: exempting every interpreter unconditionally would fail open a
// recycled PID onto the user's unrelated node.exe or powershell.exe
// processes — common on a dev box, and fatal on Windows, where a
// freed PID is handed straight back out.
//
// The extension is read off the configured command. A bare command
// name ("claude") carries none, yet npm installs the agent as
// claude.cmd, so the bare-name case is treated as a cmd.exe host.
// Resolving the name here would be unreliable — the daemon resolved
// it against ITS PATH at spawn time, and that PATH need not be ours.
//
// The known cost of the exemption: a recycled PID that lands on the
// host of a shim agent is not caught.
func hostInterpreterFor(want string) string {
	switch commandExt(want) {
	case "cmd", "bat":
		return "cmd.exe"
	case "ps1":
		return "powershell.exe"
	case "js":
		return "node.exe"
	case "":
		// A bare command name resolves through PATHEXT, which
		// lists .CMD before .JS and does not list .PS1 at all —
		// so the only host it can be is cmd.exe.
		return "cmd.exe"
	}
	return ""
}

// commandExt returns the lower-cased extension of a configured agent
// command ("cmd", "ps1", "js", "exe", …), or "" when it has none.
func commandExt(want string) string {
	base := strings.ToLower(pathutil.Base(want))
	if i := strings.LastIndex(base, "."); i > 0 {
		return base[i+1:]
	}
	return ""
}

// processImageName returns the executable file name of pid (e.g.
// "codex.exe"), or "" when it cannot be determined.
//
// The name comes from a Toolhelp32 process snapshot — a pure Win32
// call, no `tasklist` subprocess, no console window to flash. The
// snapshot lists every process on the system, including ones we
// have no right to OpenProcess.
func processImageName(pid int) string {
	if pid <= 0 {
		return ""
	}
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(snap)

	var entry windows.ProcessEntry32
	// Process32First rejects the entry unless Size carries the
	// struct size.
	entry.Size = uint32(unsafe.Sizeof(entry))
	if err := windows.Process32First(snap, &entry); err != nil {
		return ""
	}
	for {
		if int(entry.ProcessID) == pid {
			return windows.UTF16ToString(entry.ExeFile[:])
		}
		if err := windows.Process32Next(snap, &entry); err != nil {
			return ""
		}
	}
}
