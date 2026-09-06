// Package wiki implements /wiki.
//
// docs/Wiki.md is authoritative. The current implementation
// supports only /wiki init: a single Agent prompt that
// asks the Agent to read the repository, design a module
// structure, and write per-module Markdown files.
package wiki

import (
	"context"
	"errors"
	"fmt"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// Factory implements command.SlashCommandFactory for /wiki.
type Factory struct {
	git GitRunner
}

// NewFactory constructs a Factory. The git runner defaults to
// ExecGitRunner; tests can override it via SetGitRunner.
func NewFactory() *Factory {
	return &Factory{git: ExecGitRunner{}}
}

// SetGitRunner overrides the git runner (used by tests).
func (f *Factory) SetGitRunner(g GitRunner) { f.git = g }

// init self-registers the /wiki command.
func init() {
	command.RegisterBuilder(func(d command.Deps) command.SlashCommandFactory {
		return NewFactory()
	})
}

// Spec implements command.SlashCommandFactory. The current
// release exposes /wiki init only.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:    "wiki",
		Summary: "Establish the repository wiki as a project asset.",
		Usage:   "/wiki <subcommand>",
		Subcommands: []command.SubcommandSpec{
			{
				Name:    "init",
				Summary: "Read the codebase, design modules, write per-module files.",
				Usage:   "/wiki init",
			},
		},
	}
}

// Handle implements command.SlashCommandFactory.
//
// Flow (docs/Wiki.md §3.1, §3.2):
//  1. Parse the subcommand. Only "init" is accepted; any
//     other arg is a usage error.
//  2. ChatSession + active CWD + selected Agent.
//  3. Git preflight — resolve repoRoot, refuse dirty tree.
//  4. Refuse when wiki/modules/ already exists.
//  5. Queue the init prompt, return an immediate ack.
//  6. Run Init in a goroutine; the final reply is sent
//     through the Emitter when validation completes.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	sub, err := parseSubcommand(input.Args)
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	switch sub {
	case "init":
		return f.runInit(ctx, rt, cs, input)
	default:
		// parseSubcommand rejects unknown subcommands; this
		// branch is unreachable but kept for defensiveness.
		return command.Reply(ctx, rt, "❌ /wiki: unknown subcommand"), nil
	}
}

// parseSubcommand extracts the subcommand from argv. argv[0]
// is the command name; argv[1] (if present) is the
// subcommand. Anything else is rejected.
//
// "/wiki"        → ("", usage error)
// "/wiki init"   → ("init", nil)
// "/wiki -a"     → usage error (no subcommand)
// "/wiki init x" → usage error (extra positional)
func parseSubcommand(argv []string) (string, error) {
	if len(argv) == 1 {
		return "", errors.New("usage: /wiki init")
	}
	if len(argv) > 2 {
		return "", errors.New("usage: /wiki init")
	}
	switch argv[1] {
	case "init":
		return "init", nil
	default:
		return "", fmt.Errorf("unknown subcommand %q; usage: /wiki init", argv[1])
	}
}

// runInit runs the /wiki init flow. The Ack reply is sent
// immediately; the Agent prompt runs in a goroutine and the
// final reply is sent through the Emitter.
func (f *Factory) runInit(ctx context.Context, rt command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	if cs == nil {
		return command.Reply(ctx, rt, "No active chat session."), nil
	}
	cwd, fail := command.RequireActiveCwd(cs)
	if fail != nil {
		return fail, nil
	}
	// Preflight: confirm the user has selected an agent via /use.
	// We intentionally do NOT call cs.LookupSelectedAgentSession
	// here — that function requires the AS to be in StatusRunning
	// with a non-nil Handle, which is false for a freshly resumed
	// session that hasn't been Started yet. The runtime starts the
	// AS on the first QueueUserMessage dispatch; checking here
	// would reject a session the runtime would otherwise accept.
	if cs.SelectedAgent() == "" {
		return command.Reply(ctx, rt, "❌ no active agent; run /use <agent> first"), nil
	}

	repoRoot, err := f.git.RepoRoot(cwd)
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	clean, err := f.git.IsClean(repoRoot)
	if err != nil {
		return command.Reply(ctx, rt, "❌ git status failed: "+err.Error()), nil
	}
	if !clean {
		return command.Reply(ctx, rt, "❌ working tree has source changes; commit first"), nil
	}

	// Build the message anchor. ID is the channel-native
	// slash message id; events correlate against this id.
	msg := chatsession.Message{
		ID:     input.MessageID,
		ChatID: input.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "/wiki init"}},
		Kind:   chatsession.MessageKindQueue,
	}

	go func() {
		final, _ := RunInit(ctx, cs, msg, repoRoot)
		em := cs.Emitter()
		if em == nil {
			return
		}
		_ = em.Send(context.Background(), messages.OutboundMessage{
			ChatID:  input.ChatID,
			ReplyTo: input.MessageID,
			Kind:    messages.OutReply,
			Text:    final,
		})
	}()

	return command.Reply(ctx, rt, "⏳ /wiki init queued; Agent is reading the codebase."), nil
}

// Compile-time check: Factory satisfies SlashCommandFactory.
var _ command.SlashCommandFactory = (*Factory)(nil)
