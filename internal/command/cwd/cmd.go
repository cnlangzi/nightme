// Package cwd implements the `/cwd <path>` slash command.
//
// /cwd sets the workspace for the current chat. The path goes
// through `~` expansion, $HOME-relative resolution, and a
// directory-existence check before being committed via
// cs.SetActiveCwd.
//
// Factory holds *chatsession.Manager directly.
package cwd

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory is the command.SlashCommandFactory for /cwd.
type Factory struct {
	mgr            *chatsession.Manager
	defaultPrimary string
}

// NewFactory constructs a Factory backed by mgr.
func NewFactory(mgr *chatsession.Manager, defaultPrimary string) *Factory {
	return &Factory{mgr: mgr, defaultPrimary: defaultPrimary}
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "cwd",
		Summary: "Set workspace for this chat: /cwd <absolute-path>",
		Usage:   "/cwd <absolute-path>",
	}
}

// Handle implements command.SlashCommandFactory.
//
// Semantics:
//
//	/cwd (no arg)         → reply "Usage: /cwd <path>"
//	/cwd /nonexistent     → reply "Path does not exist: ..."
//	/cwd ~                → $HOME (absolute)
//	/cwd ~/foo            → $HOME/foo
//	/cwd foo              → $HOME/foo  (relative path = $HOME-relative)
//
// Existence check: we reject non-existent paths at /cwd time so
// the agent doesn't fail later with a confusing spawn error.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if len(input.Args) < 2 {
		return command.Reply(ctx, rt, "Usage: /cwd <path>"), nil
	}

	raw := strings.TrimSpace(input.Args[1])
	if raw == "" {
		return command.Reply(ctx, rt, "Usage: /cwd <path>"), nil
	}

	// 1. ~ expansion
	expanded, err := expandTilde(raw)
	if err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("Cannot expand ~: %v", err)), nil
	}

	// 2. Relative paths are $HOME-relative (not daemon-cwd-relative).
	if !filepath.IsAbs(expanded) {
		home, herr := os.UserHomeDir()
		if herr != nil {
			return command.Reply(ctx, rt, fmt.Sprintf("Cannot resolve relative path: HOME unset: %v", herr)), nil
		}
		expanded = filepath.Join(home, expanded)
	}

	abs, err := filepath.Abs(expanded)
	if err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("Invalid path: %v", err)), nil
	}

	// 3. Existence + directory check.
	info, err := os.Stat(abs)
	if err != nil {
		if os.IsNotExist(err) {
			return command.Reply(ctx, rt, fmt.Sprintf("Path does not exist: %s (resolved from %q)", abs, raw)), nil
		}
		return command.Reply(ctx, rt, fmt.Sprintf("Cannot stat %s: %v", abs, err)), nil
	}
	if !info.IsDir() {
		return command.Reply(ctx, rt, fmt.Sprintf("Not a directory: %s", abs)), nil
	}
if err := cs.SetActiveCwd(abs); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("SetActiveCwd failed: %v", err)), nil
	}

	activeAgent := cs.ActiveAgent()
	if activeAgent == "" {
		activeAgent = f.defaultPrimary
	}
	return command.Reply(ctx, rt, fmt.Sprintf(
		"Workspace set to %s.\nSession is ready (active agent: %s). Send any message to chat with it, or /use <agent> to switch. /use is optional — plain text is forwarded to the active agent automatically.",
		abs, activeAgent)), nil
}

// expandTilde expands a leading "~" or "~/" to the user's home
// directory. "~" alone becomes $HOME; "~/foo" becomes $HOME/foo.
// Returns the input unchanged if it doesn't start with "~".
func expandTilde(path string) (string, error) {
	if path == "" {
		return path, nil
	}
	if path == "~" {
		return os.UserHomeDir()
	}
	if strings.HasPrefix(path, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(home, path[2:]), nil
	}
	return path, nil
}