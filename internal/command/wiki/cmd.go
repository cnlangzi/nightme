// Package wiki implements the /wiki slash command.
//
// /wiki is a SINGLE command with no subcommands. It runs in
// two phases:
//
//  1. Plan: scan the source tree, compute the incremental
//     update list, write wiki.yml.pending. Pure mechanical
//     (git diff) — no LLM cost.
//  2. Apply: consume wiki.yml.pending, write per-module wiki
//     pages in bottom-up order (deepest path first, then
//     aggregate pages). Stub mode writes the placeholder
//     template; LLM mode (with `-a <agent>`) dispatches a real
//     agent — reserved for the future agent-call wiring.
//
// Phases are persistent: pending survives crashes, so
// resuming /wiki picks up where the previous run stopped.
package wiki

import (
	"context"
	"os/exec"
	"strings"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory implements command.SlashCommandFactory for /wiki.
//
// v0 holds a Git runner only. The LLM dispatcher (used when
// `-a <agent>` is provided) is currently a stub — when the
// real agent call path lands, the stub is swapped for one
// that wraps agent.Builtins.RunOnce.
type Factory struct {
	git GitRunner
}

// NewFactory constructs a Factory. The git runner defaults to
// ExecGitRunner (calls the git binary); tests pass a fake.
func NewFactory() *Factory {
	return &Factory{git: ExecGitRunner{}}
}

// SetGitRunner overrides the git runner (used by tests).
func (f *Factory) SetGitRunner(g GitRunner) { f.git = g }

// init self-registers the wiki command.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory()
	})
}

// Spec implements command.SlashCommandFactory. No Subcommands.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "wiki",
		Aliases: []string{"llm-wiki"},
		Summary: "Sync the repository wiki with the source tree (plan + apply, LLM stub by default).",
		Usage: "/wiki [-a <agent>]   scan source tree, plan updates, apply (stub or agent)",
	}
}

// Handle implements command.SlashCommandFactory. /wiki takes
// no subcommand — Args[1:] are user flags.
func Run(f *Factory) command.SlashCommandFactory { return f }

func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices, mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	return f.runWiki(ctx, cs, input)
}

// ExecGitRunner shells out to the git binary. Production
// default — no gtw dependency.
type ExecGitRunner struct{}

func (ExecGitRunner) Head(cwd string) (string, error) {
	cmd := exec.Command("git", "-C", cwd, "rev-parse", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func (ExecGitRunner) ChangedFiles(cwd, from, pathFilter string) ([]string, error) {
	if from == "" {
		return nil, nil
	}
	args := []string{"-C", cwd, "diff", "--name-only", from + "..HEAD"}
	if pathFilter != "" {
		args = append(args, "--", pathFilter)
	}
	cmd := exec.Command("git", args...)
	out, err := cmd.Output()
	if err != nil {
		// git diff returns non-zero when `from` doesn't
		// resolve (force-push, history rewrite). Treat as
		// "no specific file list" — caller falls back to
		// full-module regenerate.
		return nil, nil
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

func (ExecGitRunner) IsClean(cwd string) (bool, error) {
	cmd := exec.Command("git", "-C", cwd, "status", "--porcelain")
	out, err := cmd.Output()
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(string(out)) == "", nil
}