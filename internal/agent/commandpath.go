// Command-path resolution shared by every built-in starter.
//
// All built-in starters (claudecode / codex / opencode / cursor /
// pi / copilot / dsh / acp / pty) implement Detect() as
//
//	ResolveCommand(s.command)
//
// so cfg.Agents overrides can point an agent at a non-PATH
// location. Absolute paths go through os.Stat; relative names
// resolve via PATH. cfg.Agents writes absolute paths in
// practice (the firstrun prompt asks for one), but a relative
// name remains valid as long as the parent shell has it on
// PATH.
package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// ResolveCommand verifies command resolves to an invokable
// binary. Absolute paths are stat-checked and must point at a
// regular file; relative names go through exec.LookPath on
// $PATH.
func ResolveCommand(command string) error {
	if command == "" {
		return errors.New("agent: empty command")
	}
	if filepath.IsAbs(command) {
		info, err := os.Stat(command)
		if err != nil {
			return err
		}
		if info.IsDir() {
			return errors.New("agent: configured path is a directory")
		}
		return nil
	}
	if _, err := exec.LookPath(command); err != nil {
		return err
	}
	return nil
}
