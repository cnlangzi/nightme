// Package wiki implements the /wiki slash command.
//
// /wiki is a SINGLE command with no subcommands. It is
// idempotent:
//
//   - First run (no wiki.yml, no wiki/): scaffolds the wiki
//     structure + per-module skeleton files.
//   - Subsequent runs: reconciles wiki.yml with the current
//     source tree (add/remove modules), regenerates content
//     for new modules, and preserves LLM-written content for
//     modules whose last_sha is non-null.
//
// Subcommands `init` and `update` were merged into `/wiki`
// because the distinction blurred once init became
// idempotent — running init to reconcile was already
// "init + regenerate stubs", and splitting it into two
// commands added a half-state the user had to remember to
// avoid. The single-command shape matches how users think:
// "I changed code → I run `/wiki`".
//
// /wiki currently takes one optional flag:
//
//	-a / --agent <agent>   override the LLM agent (reserved for
//	                      the future LLM-driven content path;
//	                      currently accepted and ignored)
//
// No LLM is invoked at this revision. Per-module docs are
// filled with the stub template (real file layout + sources
// footer + "[TBD]" placeholders for the rest). The agent
// plumbing is in place so wiring the LLM path is a single
// function swap (replace moduleDocStub's call site with an
// agent dispatch).
package wiki

import (
	"context"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
)

// Factory implements command.SlashCommandFactory for /wiki.
//
// v0 holds no runtime deps — /wiki does not read source code
// for LLM content (stub mode) and does not run git operations.
// The future LLM path will need git + agent deps; those land
// as Factory fields and are plumbed via command.Deps.LLMExt
// when the need arrives.
type Factory struct{}

// NewFactory returns an empty Factory. No deps to wire yet.
func NewFactory() *Factory {
	return &Factory{}
}

// init self-registers the wiki command. Phase 2.3: each
// command package's init() calls RegisterBuilder; the runtime
// orchestrator calls SetDeps once at startup to finalize
// every registered builder.
//
// The wiki package needs no extension deps yet, so the
// builder closure ignores d.LLMExt (the field does not
// exist at this revision; once /wiki needs deps, the
// builder pulls them out here and the orchestrator adds
// LLMExt to the SetDeps call in internal/runtime/runtime.go).
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory()
	})
}

// Spec implements command.SlashCommandFactory. No
// Subcommands — /wiki is the only verb.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "wiki",
		Aliases: []string{"llm-wiki"},
		Summary: "Sync the repository wiki with the source tree (scaffold on first run, reconcile thereafter).",
		Usage: "/wiki [-a <agent>]   sync <cwd>/wiki/ + <cwd>/wiki.yml with the source tree",
	}
}

// Handle implements command.SlashCommandFactory. /wiki takes
// no subcommand — Args[0] is "wiki", Args[1:] are user flags.
//
// F-51 invariant: commander.Dispatch always prefixes the argv
// with the command name ("wiki"). After the merge there are
// no subcommands, so the rest of Args is the flag list.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices, mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	return f.runWiki(ctx, cs, input)
}
