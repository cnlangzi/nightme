// Package wiki implements /wiki.
//
// docs/Wiki.md is authoritative. The current implementation
// supports only /wiki init: a single Agent prompt that
// asks the Agent to read the repository, design a module
// structure, and write per-module Markdown files.
package wiki

import (
	"context"

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
				Usage:   "/wiki init [--modules] [--llmstxt] [--arch]",
			},
		},
	}
}

// Handle implements command.SlashCommandFactory.
//
// Flow (docs/Wiki.md §3.1, §3.2):
//  1. Parse the subcommand. Only "init" is accepted; any
//     other arg is a usage error.
//  2. Parse init flags. `--llmstxt` runs the deterministic
//     llms.txt build only (no Agent prompt); without it,
//     run the full init flow.
//  3. ChatSession + active CWD + selected Agent (only when
//     the Agent prompt is needed).
//  4. Git preflight — resolve repoRoot, refuse dirty tree.
//  5. Refuse when wiki/modules/ already exists.
//  6. Queue the init prompt, return an immediate ack.
//  7. Run Init in a goroutine; the final reply is sent
//     through the Emitter when validation completes.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	sub, opts, err := parseSubcommand(input.Args[1:])
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	if sub != "init" {
		return command.Reply(ctx, rt, "❌ /wiki: unknown subcommand"), nil
	}

	// Shortcut: if no modules step and no arch step are
	// requested, the deterministic llms.txt build alone is
	// enough — no Agent prompt needed.
	if !opts.Modules && !opts.Arch {
		return f.runLlmsTxt(ctx, cs, input)
	}
	return f.runInit(ctx, rt, cs, input, opts)
}

// parseSubcommand extracts the init subcommand and its flags
// from argv. The command name has already been removed by
// Factory.Handle. Returns (subcommand, opts, error).
//
// "init"                       → ("init", {all three = true},  nil)
// "init --modules"             → ("init", {Modules: true,  ...},  nil)
// "init --llmstxt"             → ("init", {Llmstxt: true,  ...},  nil)
// "init --arch"                → ("init", {Arch: true,    ...},  nil)
// "/wiki init --llmstxt --arch" → ("init", {Llmstxt, Arch = true},  nil)
// "init x"                      → ("",     {},     extra positional)
// "init --weird"                → ("",     {},     unknown flag)
//
// Each flag is an independent toggle. When no flag is set,
// all three steps run by default.
func parseSubcommand(argv []string) (string, InitOptions, error) {
	parsed, err := command.ParseCmdArgs(argv, command.CmdSpec{
		Name:       "/wiki",
		Usage:      "/wiki init [--modules] [--llmstxt] [--arch]",
		Subcommand: "init",
		Flags: map[string]command.FlagSpec{
			"--modules": {Name: "modules"},
			"--llmstxt": {Name: "llmstxt"},
			"--arch":    {Name: "arch"},
		},
		MinArgs: 0,
		MaxArgs: 0,
	})
	if err != nil {
		return "", InitOptions{}, err
	}
	// Default: all three steps. If any flag was supplied,
	// use only the explicitly-set ones.
	anySet := parsed.Has("modules") || parsed.Has("llmstxt") || parsed.Has("arch")
	return parsed.Subcommand, InitOptions{
		Modules: !anySet || parsed.Bool("modules"),
		Llmstxt: !anySet || parsed.Bool("llmstxt"),
		Arch:    !anySet || parsed.Bool("arch"),
	}, nil
}

// runInit runs the /wiki init flow. The Ack reply is sent
// immediately; the Agent prompt runs in a goroutine and the
// final reply is sent through the Emitter.
func (f *Factory) runInit(ctx context.Context, rt command.RuntimeServices, cs *chatsession.ChatSession, input command.SlashInput, opts InitOptions) (*command.SlashOutput, error) {
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
		final, _ := RunInit(ctx, cs, msg, repoRoot, opts)
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

// runLlmsTxt runs `/wiki init --llmstxt`: deterministic
// rebuild of wiki/llms.txt from the existing wiki/modules/.
// No Agent prompt is submitted, so no /use selection or
// AgentSession is required — only the CWD matters.
func (f *Factory) runLlmsTxt(ctx context.Context, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {
	if cs == nil {
		return &command.SlashOutput{Reply: "No active chat session.", Consumed: true}, nil
	}
	cwd, fail := command.RequireActiveCwd(cs)
	if fail != nil {
		return fail, nil
	}
	repoRoot, err := f.git.RepoRoot(cwd)
	if err != nil {
		return &command.SlashOutput{Reply: "❌ " + err.Error(), Consumed: true}, nil
	}

	final, _ := RunLlmsTxtOnly(repoRoot)

	// Reply directly (no Agent session involved, so no Emitter
	// detour). Reply to the original slash message via Emitter
	// so the IM client renders it on the right card.
	if em := cs.Emitter(); em != nil {
		_ = em.Send(context.Background(), messages.OutboundMessage{
			ChatID:  input.ChatID,
			ReplyTo: input.MessageID,
			Kind:    messages.OutReply,
			Text:    final,
		})
	}
	return &command.SlashOutput{Reply: "⏳ /wiki init --llmstxt", Consumed: true}, nil
}

// Compile-time check: Factory satisfies SlashCommandFactory.
var _ command.SlashCommandFactory = (*Factory)(nil)
