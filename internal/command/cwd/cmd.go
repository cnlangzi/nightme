// Package cwd implements the `/cwd <path>` slash command.
//
// /cwd sets the workspace for the current chat. The path goes
// through `~` expansion, $HOME-relative resolution, and a
// directory-existence check before being committed via
// cs.SetSelectedCwd.
//
// Factory holds no chatsession.Manager — cs comes from the
// dispatcher parameter at Handle time.
package cwd

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

// Factory is the command.SlashCommandFactory for /cwd.
type Factory struct{}

// NewFactory constructs a Factory. command/* factories do not
// receive a *chatsession.Manager — cs comes from the dispatcher
// parameter at Handle time.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory()
	})
}

func NewFactory() *Factory {
	return &Factory{}
}

// Spec implements command.SlashCommandFactory.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "cwd",
		Summary: "Set workspace for this chat: /cwd <absolute-path>",
		Usage:   "/cwd <absolute-path>",
	}
}

// cwdSpec declares /cwd's argv grammar for the shared lexer
// (issue #291): no flags, exactly one positional path.
//
// Commander.extractCommand splits the message body on
// whitespace via strings.Fields, so "/cwd /foo bar" arrives as
// three Args. /cwd takes exactly one path, so anything beyond
// Args[1] is either a typo or the user forgot to quote a path
// containing spaces — better to surface the mistake than
// silently use only the first token.
//
// The option/arg split matters even with zero flags today: a
// path that legitimately starts with "-" is reachable via the
// conventional terminator (`/cwd -- -weird-dir`), and if /cwd
// ever grows a real flag it goes in Flags here rather than
// re-introducing the "silently swallowed token" bug class.
var cwdSpec = command.CmdSpec{
	Name:    "/cwd",
	Usage:   "/cwd <absolute-path>",
	MinArgs: 1,
	MaxArgs: 1,
}

// Handle implements command.SlashCommandFactory.
//
// Semantics:
//
//	/cwd (no arg)         → reply "Usage: /cwd <path>"
//	/cwd /a /b            → reply "too many arguments"
//	/cwd --typo           → reply "unknown flag"
//	/cwd /nonexistent     → reply "Path does not exist: ..."
//	/cwd ~                → $HOME (absolute)
//	/cwd ~/foo            → $HOME/foo
//	/cwd foo              → $HOME/foo  (relative path = $HOME-relative)
//
// Windows-specific paths (handled by path_windows.go):
//
//	/cwd C:\foo           → C:\foo        (drive-rooted, absolute)
//	/cwd C:/foo           → C:/foo        (forward-slash variant)
//	/cwd \foo             → \foo          (root-relative on current drive)
//	/cwd /foo             → <drv>:\foo    (root-relative on current drive)
//	/cwd \\server\share   → UNC path      (absolute)
//	/cwd C:foo            → rejected      (drive-relative ambiguity)
//
// Existence check: we reject non-existent paths at /cwd time so
// the agent doesn't fail later with a confusing spawn error.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	parsed, err := command.ParseCmdArgs(input.Args[1:], cwdSpec)
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	raw := strings.TrimSpace(parsed.Arg(0))
	if raw == "" {
		return command.Reply(ctx, rt, "Usage: /cwd <path>"), nil
	}
	// Multi-line inputs are almost always a paste accident.
	// Reject explicitly rather than letting downstream code
	// see a path with embedded \n (os.Stat would treat it as
	// a single-line path, but the user clearly didn't mean
	// that).
	if strings.ContainsAny(raw, "\n\r") {
		return command.Reply(ctx, rt, "Path cannot span multiple lines; paste as a single line."), nil
	}

	// IME normalisation: full-width ASCII → half-width, CJK
	// punctuation → English. This guards against the common
	// case of a user typing "/cwd ／foo" with a Chinese IME
	// active — the leading full-width slash would otherwise
	// cause resolvePath to misclassify the path.
	//
	// Preserve the original input in rawOriginal so error
	// messages echo what the user actually typed (full-width
	// slash and all), not the normalised form they can't
	// easily correlate with. The mapping never produces an
	// empty string from a non-empty input, so no second
	// empty-check is needed here.
	rawOriginal := raw
	raw = normalizePathInput(raw)

	// 1. ~ expansion
	expanded, err := expandTilde(raw)
	if err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("Cannot expand ~: %v", err)), nil
	}

	// 2. Resolve to an absolute path. The platform-specific
	// resolvePath (path_unix.go / path_windows.go) decides
	// whether the input is absolute or $HOME-relative. On
	// Windows, drive-relative forms like "C:foo" are rejected
	// explicitly because Go's filepath.IsAbs classifies them
	// as relative and would silently join them with $HOME.
	abs, err := resolvePath(expanded)
	if err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("Cannot resolve path %q: %v", rawOriginal, err)), nil
	}

	// 3. Existence + directory check, also platform-specific.
	if err := verifyDirectory(abs, rawOriginal); err != nil {
		return command.Reply(ctx, rt, err.Error()), nil
	}

	if err := cs.SetSelectedCwd(abs); err != nil {
		return command.Reply(ctx, rt, fmt.Sprintf("SetSelectedCwd failed: %v", err)), nil
	}

	selectedAgent := cs.SelectedAgent()
	if selectedAgent == "" {
		selectedAgent = rt.Config.Primary
	}
	return command.Reply(ctx, rt, fmt.Sprintf(
		"Workspace set to %s.\nSession is ready (active agent: %s). Send any message to chat with it, or /use <agent> to switch. /use is optional — plain text is forwarded to the active agent automatically.",
		abs, selectedAgent)), nil
}

// errHomeUnset formats a "HOME is unset" error. Shared by
// path_unix.go and path_windows.go via the resolvePath path.
func errHomeUnset(err error) error {
	return fmt.Errorf("HOME unset: %w", err)
}

// expandTilde expands a leading "~" or "~/" to the user's home
// directory. "~" alone becomes $HOME; "~/foo" becomes $HOME/foo.
// Returns the input unchanged if it doesn't start with "~".
//
// F-PATHUTIL-001 §13.3.3: route the home-relative join through
// pathutil.Join so the platform-specific separator handling
// (backslashes on Windows, forward slashes on Unix) is
// consistent with every other path operation in this package.
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
		return pathutil.Join(home, path[2:]), nil
	}
	return path, nil
}
