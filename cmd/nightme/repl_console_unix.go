//go:build !windows

// Package main — console gating for the interactive REPL on POSIX.
//
// POSIX terminals interpret VT/ANSI CSI sequences natively via the
// terminal driver, so any real tty is a usable host for
// reeflective/readline. readlineUsable reduces to isatty.
package main

import (
	"os"

	"github.com/mattn/go-isatty"
)

// readlineUsable on POSIX: any real terminal speaks VT natively,
// so isatty is a sufficient gate. Non-tty (pipe/file) → false →
// scanner path, same as Windows non-console.
func readlineUsable() bool {
	return isatty.IsTerminal(os.Stdin.Fd())
}
