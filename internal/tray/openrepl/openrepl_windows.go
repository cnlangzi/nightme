//go:build windows

// Windows implementation of tray "open terminal" for all commands.
//
// Strategy: cmd /c start "title" cmd /k nightme.exe <args...>. The
// /k switch keeps the new cmd window open after nightme exits, so
// the user can read output (rather than the window vanishing
// mid-flash). The double "cmd" is required — the inner cmd is the
// one that actually runs nightme.exe; the outer cmd is the one
// that `start` invokes with /k, and `start` needs a command to run
// that is itself a console subsystem binary.
//
// We route through proc.NewVisible (not proc.New) because the
// user is asking for a new visible terminal window — proc.New
// applies CREATE_NO_WINDOW which would defeat the entire purpose.

package openrepl

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/cnlangzi/nightme/internal/proc"
)

// openCmd spawns a new console window that runs `nightme.exe <args>`.
// The window stays open after exit (/k) so output is visible.
func openCmd(args ...string) error {
	bin, err := exec.LookPath("nightme.exe")
	if err != nil {
		return fmt.Errorf("openrepl: nightme.exe not on PATH: %w", err)
	}
	cmdExe := proc.ComSpecOrDefault()
	// cmd.exe /c start "title" cmd /k <bin> [args...]
	//
	//   "NightMe [...]" — window title (positional arg to `start`;
	//                      quotes mandatory because the title
	//                      contains a space when args are appended)
	//   cmd /k           — invoke cmd.exe and keep its window open
	//                      after the inner command exits
	//   <bin> [args]     — the inner command + its subcommand args
	title := "NightMe"
	if len(args) > 0 {
		title = "NightMe " + strings.Join(args, " ")
	}
	fullArgs := append([]string{"/c", "start", title, "cmd", "/k", bin}, args...)
	cmd := proc.NewVisible(context.Background(), cmdExe, fullArgs...)
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("openrepl: start cmd /k: %w", err)
	}
	go func() { _ = cmd.Wait() }()
	return nil
}
