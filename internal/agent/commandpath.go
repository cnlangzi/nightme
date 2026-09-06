// Command-path resolution shared by every built-in starter.
//
// All built-in starters (claudecode / codex / opencode / cursor /
// pi / copilot / dsh / acp / pty) implement Detect() as
//
//	ResolveCommand(s.command)
//
// so cfg.Agents can override the executable path without
// re-implementing the same logic in nine places. Absolute paths
// go through os.Stat; relative names resolve via PATH. cfg.Agents
// writes absolute paths in practice (the firstrun prompt asks for
// one), but a relative name remains valid as long as the parent
// shell has it on PATH.
package agent

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
)

// CommandSetter is implemented by every Starter whose executable
// path can be overridden at runtime. SetBuiltinCommand type-asserts
// against this interface; starters that don't implement it fall
// back to the hardcoded command.
type CommandSetter interface {
	SetCommand(string)
}

// SetBuiltinCommand overrides the executable path used by Detect
// and Info on the starter registered under name. Returns false
// when the registry has no entry for name, or the entry does not
// implement CommandSetter.
//
// Pass "" to clear a previous override and revert to the
// hardcoded command baked in by NewStarter.
//
// The mutation hits the *Starter stored in Builtins — every
// subsequent Detect() and Info() reflects the new path. This is
// intentional: registration-time builtins are singletons and the
// daemon's first Build() is the only one that matters in
// production. Tests that mutate must save/restore.
func SetBuiltinCommand(reg *Registry, name, command string) bool {
	s, err := reg.Get(name)
	if err != nil {
		return false
	}
	c, ok := s.(CommandSetter)
	if !ok {
		return false
	}
	c.SetCommand(command)
	return true
}

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
