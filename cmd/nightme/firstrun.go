// Package main — first-run prompt for the AI agent binary.
//
// The previous build auto-detected a primary by probing built-ins
// in registration order, silently picking the first one whose
// PATH lookup succeeded. That silently degraded whenever none of
// the seven built-ins were on PATH: cfg.Primary was left empty,
// the daemon would later crash with "need primaryAgent to create"
// and the user had no in-band hint about what to do.
//
// This file replaces that with a deterministic flow:
//
//  1. Build the registry from cfg (cfg.Agents overrides applied).
//  2. Run Detect on every built-in.
//  3. If cfg.Primary resolves to a working agent, accept it and
//     return — no prompt.
//  4. Else if at least one built-in resolves, auto-pick the first
//     working one as cfg.Primary, save, return — no prompt.
//  5. Else prompt the user: pick a built-in, give it a path,
//     write cfg.Agents + cfg.Primary, re-detect. Loop until the
//     path resolves or the user aborts.
//
// Non-interactive callers (CI, container init) get a clean error
// instead of a hung read; set NIGHTME_NO_PROMPT=1 to force the
// non-interactive path even from a TTY (e.g. for batch smoke
// tests that want the error, not the prompt).
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentregistry"
	"github.com/cnlangzi/nightme/internal/config"
)

// errNoAgentConfigured is returned when EnsureAgentAvailable
// cannot find or configure any built-in agent. The daemon / test
// command should surface this verbatim — it carries the
// remediation hint.
var errNoAgentConfigured = errors.New(
	"no AI coding agent available; install one of the built-ins (claude / codex / dsh / opencode / cursor / pi / copilot) or set cfg.Agents to override its path, then retry",
)

// EnsureAgentAvailable makes sure cfg.Primary resolves to a
// working built-in agent before the caller boots a runtime that
// needs one. Updates cfg.Primary / cfg.Agents in place and saves
// to disk when it makes a change.
//
// in is wrapped in a *bufio.Reader (or expected to be one) so the
// interactive prompt can consume multiple lines per session — a
// fresh io.Reader wrapper on each readLine loses bytes between
// calls.
func EnsureAgentAvailable(cfg *config.Config, in *bufio.Reader, out io.Writer) error {
	probe, detected := probeBuiltins(cfg)

	if cfg.Primary != "" {
		if err, ok := probe[cfg.Primary]; ok && err == nil {
			return nil
		}
		// cfg.Primary points at an agent that no longer resolves.
		// Drop it; the auto-pick below picks a working one
		// (or the prompt collects a new path).
		cfg.Primary = ""
	}

	if len(detected) > 0 {
		cfg.Primary = detected[0]
		if err := config.SaveDefault(cfg); err != nil {
			return fmt.Errorf("save primary %q: %w", detected[0], err)
		}
		fmt.Fprintf(out, "✓ primary set to %q (auto-detected)\n", detected[0])
		return nil
	}

	if !canPrompt(in) {
		return errNoAgentConfigured
	}
	return firstrunPrompt(cfg, probe, in, out)
}

// probeBuiltins runs Detect on every built-in after applying
// cfg.Agents overrides. Returns the per-name error map and the
// ordered list of names that resolved successfully.
func probeBuiltins(cfg *config.Config) (map[string]error, []string) {
	reg := agentregistry.Build(cfg, "")
	probe := make(map[string]error, len(reg.List()))
	var detected []string
	for _, s := range reg.List() {
		name := s.Info().Name
		err := s.Detect()
		probe[name] = err
		if err == nil {
			detected = append(detected, name)
		}
	}
	return probe, detected
}

// canPrompt reports whether the underlying stdin is a TTY and the
// user has not set NIGHTME_NO_PROMPT to force non-interactive
// behaviour. Tests wrap a bytes.Buffer or *strings.Reader with
// bufio.NewReader; those are treated as interactive so the prompt
// runs (the test drives the input).
func canPrompt(in *bufio.Reader) bool {
	if os.Getenv("NIGHTME_NO_PROMPT") != "" {
		return false
	}
	if in == nil {
		return false
	}
	// bufio.NewReader hides the underlying source; for the
	// non-TTY bail-out we only check os.Stdin directly. Tests
	// typically want the prompt, so non-Stdin readers default to
	// interactive.
	if f, ok := unwrapFile(in); ok {
		info, err := f.Stat()
		if err != nil {
			return false
		}
		return (info.Mode() & os.ModeCharDevice) != 0
	}
	return true
}

// unwrapFile peels off the bufio.Reader wrapper to recover the
// underlying *os.File, if any. Other sources (bytes.Buffer,
// strings.Reader) are treated as non-TTY and get the default
// "interactive" path.
func unwrapFile(br *bufio.Reader) (*os.File, bool) {
	type unwrapper interface{ Unwrap() *os.File }
	// bufio.Reader does not implement Unwrap, so this branch
	// always fails — the helper exists so future wrappers (e.g.
	// a custom linereader) can opt in.
	if u, ok := any(br).(unwrapper); ok {
		return u.Unwrap(), true
	}
	return nil, false
}

func firstrunPrompt(cfg *config.Config, probe map[string]error, in *bufio.Reader, out io.Writer) error {
	builtins := agent.Builtins.List()

	fmt.Fprintln(out)
	fmt.Fprintln(out, "nightme: no AI coding agent found in PATH or cfg.Agents.")
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Supported built-ins:")
	for i, s := range builtins {
		name := s.Info().Name
		status := "not found"
		if err := probe[name]; err == nil {
			status = "found"
		}
		fmt.Fprintf(out, "  [%d] %-9s %s\n", i+1, name, status)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Enter a number to configure that agent's path, or q to abort:")
	fmt.Fprint(out, "> ")

	choice := strings.TrimSpace(readLine(in))
	if choice == "" || strings.EqualFold(choice, "q") {
		return errNoAgentConfigured
	}
	n, err := strconv.Atoi(choice)
	if err != nil || n < 1 || n > len(builtins) {
		return fmt.Errorf("invalid choice %q", choice)
	}

	picked := builtins[n-1]
	name := picked.Info().Name

	for {
		pathLine, err := promptAgentPath(name, in, out)
		if err != nil {
			if errors.Is(err, errPromptAborted) {
				return errNoAgentConfigured
			}
			return err
		}
		applyAgentOverride(cfg, name, pathLine)
		cfg.Primary = name
		if err := config.SaveDefault(cfg); err != nil {
			return fmt.Errorf("save config: %w", err)
		}
		fmt.Fprintf(out, "✓ wrote primary=%s, cfg.Agents[%s]=%s\n", name, name, pathLine)
		return nil
	}
}

// applyAgentOverride upserts the (name, command) pair into
// cfg.Agents. Replaces an existing entry with the same name so
// re-running the prompt updates the path rather than appending.
func applyAgentOverride(cfg *config.Config, name, command string) {
	for i := range cfg.Agents {
		if cfg.Agents[i].Name == name {
			cfg.Agents[i].Command = command
			return
		}
	}
	cfg.Agents = append(cfg.Agents, config.AgentEntry{Name: name, Command: command})
}

// removeAgentOverride deletes the cfg.Agents entry with the given
// name, if any. No-op when the name is not overridden.
func removeAgentOverride(cfg *config.Config, name string) {
	out := cfg.Agents[:0]
	for _, e := range cfg.Agents {
		if e.Name != name {
			out = append(out, e)
		}
	}
	cfg.Agents = out
}

// promptAgentPath interactively collects an absolute path for
// the given built-in. Loops on invalid input (empty / non-absolute
// / missing / directory) until the user supplies a valid path or
// aborts with q. Shared by firstrunPrompt and configAgentsMenu.
func promptAgentPath(name string, in *bufio.Reader, out io.Writer) (string, error) {
	for {
		fmt.Fprintf(out, "Absolute path to %s binary (q to abort):\n> ", name)
		pathLine := strings.TrimSpace(readLine(in))
		if pathLine == "" {
			fmt.Fprintln(out, "Path cannot be empty.")
			continue
		}
		if strings.EqualFold(pathLine, "q") {
			return "", errPromptAborted
		}
		if !filepath.IsAbs(pathLine) {
			fmt.Fprintf(out, "Path must be absolute: %s\n", pathLine)
			continue
		}
		if err := validateAbsolutePath(pathLine); err != nil {
			fmt.Fprintln(out, err.Error())
			continue
		}
		if err := agent.ResolveCommand(pathLine); err != nil {
			fmt.Fprintf(out, "Detect failed: %v\n", err)
			continue
		}
		return pathLine, nil
	}
}

// validateAbsolutePath confirms path points at a regular file.
func validateAbsolutePath(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	if info.IsDir() {
		return fmt.Errorf("%s is a directory, not a binary", path)
	}
	return nil
}

var errPromptAborted = errors.New("aborted by user")
