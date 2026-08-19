//go:build windows

// Windows implementation of "Open REPL" from the tray menu.
//
// Strategy: cmd /c start "title" cmd /k nightme.exe. The /k
// switch keeps the new cmd window open after the nightme REPL
// exits, so the user can read any error output (rather than the
// window vanishing mid-flash). The double "cmd" is required —
// the inner cmd is the one that actually runs nightme.exe; the
// outer cmd is the one that `start` invokes with /k, and `start`
// needs a command to run that is itself a console subsystem
// binary.
//
// We deliberately do NOT use proc.New here: the user is asking
// for a new visible terminal window, and CREATE_NO_WINDOW
// (which proc.New applies on Windows) would defeat the entire
// purpose. The intent is the opposite of the daemon child.

package openrepl

import (
	"fmt"
	"os/exec"
	"syscall"

	"github.com/cnlangzi/nightme/internal/proc"
)

// open spawns a new console window that runs `nightme.exe` (REPL
// mode). The window stays open after exit (/k) so error output
// is visible.
func open() error {
	// Resolve nightme.exe from PATH. We don't hard-code a
	// path because the user may have installed via scoop,
	// chocolatey, or a manual copy to a non-standard
	// location; the tray click is supposed to do the right
	// thing regardless of install method.
	bin, err := exec.LookPath("nightme.exe")
	if err != nil {
		return fmt.Errorf("openrepl: nightme.exe not on PATH: %w", err)
	}
	// Resolve cmd.exe via %ComSpec% (set on every standard
	// Windows install) with an explicit fallback for the
	// rare case where the user cleared it. Reuses the same
	// helper as the daemon's spawn recipe so this codepath
	// can't drift from the rest of the codebase.
	cmdExe := proc.ComSpecOrDefault()
	// cmd.exe /c start "title" cmd /k <bin>
	//
	// Args layout (verbatim, including quoting rules):
	//   /c         — run and exit (but start is asynchronous)
	//   start      — built-in cmd command to launch
	//   "NightMe REPL" — the title of the new window (also
	//                    a positional arg to `start`; the
	//                    quotes are mandatory because the
	//                    title contains a space)
	//   cmd /k     — invoke cmd.exe and keep its window open
	//                after the inner command exits
	//   <bin>      — the inner command
	//
	// SysProcAttr stays nil so the new window is a normal
	// console (not a hidden one).
	cmd := exec.Command(cmdExe, "/c", "start", "NightMe REPL", "cmd", "/k", bin)
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("openrepl: start cmd /k: %w", err)
	}
	// The outer `cmd /c start` returns immediately because
	// `start` is asynchronous; we don't want a zombie. The
	// new console window is its own process; this Wait()
	// just reaps the helper.
	go func() { _ = cmd.Wait() }()
	return nil
}
