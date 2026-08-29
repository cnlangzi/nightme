//go:build !windows

package proc

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// KillGrace is how long a SIGTERM gets to flush a child before
// SIGKILL. Used by WithGrace for nightme-side git / gh / glab /
// ocr / agent-bridge subprocesses. Tuned for `git`'s
// `.git/index.lock` release path: 1s is comfortably longer than
// `git`'s signal-induced cleanup while still feeling instant to a
// human-driven context cancel.
//
// Distinct from cmd/nightme/kill_unix.go:killGrace (30s), which
// serves agent-CLI "flush --resume id" semantics. Don't unify.
const KillGrace = 1 * time.Second

// gracedMu guards graced. Entries are added by WithGrace on
// first-call and removed by the watcher goroutine once ctx
// fires (or process reaps, whichever first). A late cmd can
// outlive its ctx (we never tight-GC the entry), so memory is
// bounded by # of distinct *exec.Cmd instances ever witnessed —
// acceptable for a long-running daemon.
var (
	gracedMu sync.Mutex
	graced   = make(map[*exec.Cmd]context.Context)
)

// WithGrace swaps exec.CommandContext's SIGKILL-on-cancel for
// SIGTERM → KillGrace → SIGKILL. Without it, a cancelled `git`
// child leaves .git/index.lock on disk — LLM then sees the
// stale lock on its own `git add` and (autonomously) tries to
// `rm` it. See docs/feat/F-CLAUDE-PRINT-002 §后续 2026-08-29.
//
// Contract:
//   - cmd MUST come from proc.New(ctx, ...) (i.e. the cmd was
//     built via exec.CommandContext + Setsid). Other callers'
//     cmds are silently skipped — registry lookup misses and
//     WithGrace is a no-op.
//   - Calling WithGrace twice on the same cmd is safe: the
//     watcher goroutine is restartable via the cmd-context
//     registry. Only one watcher will fire per ctx-fire.
//   - cmd.Cancel is overwritten to a no-op so stdlib's default
//     SIGKILL path doesn't race our SIGTERM. WithGrace owns
//     the child's death-on-cancel exclusively.
//   - The watcher goroutine self-terminates once ctx fires.
//     cmd is not held after.
//
// cmd.WaitDelay (if set elsewhere) still takes effect; this
// helper does not touch it.
func WithGrace(cmd *exec.Cmd, grace time.Duration) {
	if cmd == nil || grace <= 0 {
		return
	}
	gracedMu.Lock()
	ctx := graced[cmd]
	gracedMu.Unlock()
	if ctx == nil {
		return
	}

	// Suppress stdlib's default Cancel (SIGKILL on ctx-fire),
	// and tell it to surface the child's real exit status. The
	// default Cancel returns nil; stdlib interprets that as
	// "we interrupted, ctx is the cause" and wraps
	// Run/Wait/Output with ctx.Err() even when the child
	// actually exited 0. Returning os.ErrProcessDone instead
	// tells stdlib "the process is gone, don't attribute the
	// result to ctx" — Run returns the child's exit status as
	// if ctx had never been cancelled. See os/exec.Cancel doc
	// (Go stdlib).
	cmd.Cancel = func() error { return os.ErrProcessDone }

	go func() {
		<-ctx.Done()

		if cmd.Process == nil {
			return
		}
		pid := cmd.Process.Pid

		// SIGTERM the entire process group (Setsid enabled
		// pgid = pid). git reaps .git/index.lock on its way
		// out. Fall back to single-pid signal on ESRCH
		// (race between watchCtx and a natural exit).
		if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
			if errors.Is(err, syscall.ESRCH) {
				return
			}
			_ = cmd.Process.Signal(syscall.SIGTERM)
		}

		// Grace timer. Fires once. If the child was already
		// gone (cmd.Process nil'd by Wait), it's a no-op.
		time.AfterFunc(grace, func() {
			proc := cmd.Process
			if proc == nil {
				return
			}
			if err := syscall.Kill(-proc.Pid, syscall.SIGKILL); err != nil {
				if errors.Is(err, syscall.ESRCH) {
					return
				}
				_ = proc.Kill()
			}
		})

		// Self-cleanup: drop the registry entry so future
		// calls to WithGrace(cmd, ...) for the same *exec.Cmd
		// pointer (rare but possible during retries) start
		// fresh. cmd pointer is stable; not a re-add.
		gracedMu.Lock()
		delete(graced, cmd)
		gracedMu.Unlock()
	}()
}
