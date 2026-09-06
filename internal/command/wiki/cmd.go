// Package wiki implements the /wiki slash command.
//
// /wiki is a single command with no subcommands and no flags.
// It runs in two phases:
//
//  1. Plan — synchronous in Handle. Discover modules, compute
//     the incremental update list, write wiki.yml.pending.
//     Pure mechanical (git diff) — no LLM cost.
//
//  2. Apply — asynchronous, coordinated by a per-repo Job.
//     Consume wiki.yml.pending in bounded batches; submit each
//     batch as a MessageKindQueue prompt through the chat's
//     long-running AgentSession; wait on PromptEndBus for
//     completion; validate the framed result; stamp
//     wiki.yml.last_sha; rebuild llms.txt indexes.
//
// Both phases are persistent (wiki.yml.pending survives), so a
// crash between phases or mid-batch resumes cleanly on the next
// /wiki invocation. Job ownership (one Job per repo root, keyed
// by cs.ChatID) prevents concurrent /wiki runs against the
// same repository from corrupting wiki.yml.
//
// The package does NOT contain an Agent provider abstraction
// or one-shot runner — Agent selection is /use's responsibility
// (Wiki.md §2 out-of-scope list).
package wiki

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

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
// ExecGitRunner (calls the git binary); tests override it.
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

// Spec implements command.SlashCommandFactory. Wiki.md §3:
// zero-argument command, no aliases that suggest otherwise.
func (f *Factory) Spec() command.Spec {
	return command.Spec{
		Name:     "wiki",
		Aliases:  []string{"llm-wiki"},
		Summary:  "Sync the repository wiki with the source tree (plan + apply via chat agent).",
		Usage:    "/wiki",
		Category: "session",
	}
}

// Handle implements command.SlashCommandFactory.
//
// Flow (Wiki.md §3.1 / §3.2 / §3.3):
//
//  1. Reject any argv beyond `/wiki`.
//  2. ChatSession + active CWD + selected Agent (via
//     LookupSelectedAgentSession) — all required before any
//     filesystem write.
//  3. Git preflight — resolve repo root, refuse dirty tree.
//  4. Plan — compute pending[], write wiki.yml.
//  5. Acquire Job for the repo root — concurrent /wiki
//     against the same repo gets a clear "wiki job already
//     running" reply.
//  6. Submit the first batch (or finalise no-op when the plan
//     is empty) and return an immediate ack reply.
func (f *Factory) Handle(ctx context.Context, rt command.RuntimeServices,
	mgr *chatsession.Manager, cs *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, error) {

	if err := parseWikiArgs(input.Args); err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}
	if cs == nil {
		return command.Reply(ctx, rt, "No active chat session."), nil
	}
	if _, fail := command.RequireActiveCwd(cs); fail != nil {
		return fail, nil
	}
	if _, err := cs.LookupSelectedAgentSession(); err != nil {
		return command.Reply(ctx, rt, "❌ no active agent; run /use <agent> first"), nil
	}

	cwd := cs.SelectedCwd()
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
	head, err := f.git.Head(repoRoot)
	if err != nil {
		return command.Reply(ctx, rt, "❌ not a git repo (or git unavailable): "+err.Error()), nil
	}

	// Load or reconcile wiki.yml. When both wiki.yml AND wiki/
	// are absent, fresh scaffold (state-reconciliation row 1).
	yml, err := loadOrReconcileYml(repoRoot)
	if err != nil {
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	if err := Plan(repoRoot, head, yml, f.git); err != nil {
		return command.Reply(ctx, rt, "❌ plan: "+err.Error()), nil
	}

	// Acquire Job. Concurrent same-repo /wiki gets ErrJobRunning.
	job, err := Acquire(repoRoot, cs.Context(), cs.ChatID)
	if err != nil {
		if IsErrJobRunning(err) {
			return command.Reply(ctx, rt, "❌ wiki job already running"), nil
		}
		return command.Reply(ctx, rt, "❌ "+err.Error()), nil
	}

	// Build the orchestrator message anchor. The Message.ID is
	// the channel-native slash message id so ordinary Agent
	// output can attach to a real message; events are correlated
	// against this id AND the Job id (Wiki.md §3.2 last
	// paragraph).
	msg := chatsession.Message{
		ID:     input.MessageID,
		ChatID: input.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: "/wiki"}},
		Kind:   chatsession.MessageKindQueue,
	}

	coord := &Coordinator{
		Job:   job,
		CS:    cs,
		Input: msg,
		Yml:   yml,
		Head:  head,
		Git:   f.git,
	}

	// Decide whether anything actually needs an Agent prompt.
	// A no-op plan (no pending live-module entries and no
	// dirty aggregate) short-circuits to a sync finalise.
	if isPurelyMechanical(yml) {
		coord.finalize()
		job.Release()
		return command.Reply(ctx, rt, "✅ /wiki\n\nAlready in sync with the source tree."), nil
	}

	ack := formatAckReply(yml, repoRoot)
	go func() {
		final := coord.Start()
		if em := cs.Emitter(); em != nil {
			out := messages.OutboundMessage{
				ChatID:  input.ChatID,
				ReplyTo: input.MessageID,
				Kind:    messages.OutReply,
				Text:    final,
			}
			_ = em.Send(context.Background(), out)
		}
	}()

	return command.Reply(ctx, rt, ack), nil
}

// loadOrReconcileYml implements the §9 state-reconciliation
// table for the four (yml exists, wiki/ exists) combinations.
//
// Returns a wikiYml ready for Plan. Callers MUST NOT mutate
// the returned yml until after Acquire so concurrent /wiki
// runs do not stomp each other.
func loadOrReconcileYml(repoRoot string) (*wikiYml, error) {
	ymlPath := filepath.Join(repoRoot, "wiki.yml")
	wikiDir := filepath.Join(repoRoot, "wiki")
	ymlExists := fileExists(ymlPath)
	wikiExists := dirExists(wikiDir)

	switch {
	case !ymlExists && !wikiExists:
		// Fresh scaffold: empty yml + ensure aggregates exist.
		return newFreshYml(), nil
	case ymlExists && !wikiExists:
		// wiki/ missing — recover by recreating empty directories
		// and skeleton pages for every live module.
		data, err := os.ReadFile(ymlPath)
		if err != nil {
			return nil, fmt.Errorf("read wiki.yml: %w", err)
		}
		yml, err := parseWikiYml(data)
		if err != nil {
			return nil, err
		}
		if err := scaffoldEmptyRecovery(repoRoot, yml); err != nil {
			return nil, fmt.Errorf("recover wiki/: %w", err)
		}
		return yml, nil
	case !ymlExists && wikiExists:
		// yml missing — reconstruct from wiki/ contents.
		return reconstructYmlFromWiki(wikiDir)
	default:
		// Both present — read yml.
		data, err := os.ReadFile(ymlPath)
		if err != nil {
			return nil, fmt.Errorf("read wiki.yml: %w", err)
		}
		return parseWikiYml(data)
	}
}

// reconstructYmlFromWiki walks wiki/modules/ and produces a
// wikiYml from the file paths. Every <pkg-path>/index.md
// becomes a moduleYml with last_sha=nil (we have no record of
// git SHAs from filenames alone). The next /wiki invocation
// will regenerate any module whose page no longer validates.
func reconstructYmlFromWiki(wikiDir string) (*wikiYml, error) {
	modulesDir := filepath.Join(wikiDir, "modules")
	out := &wikiYml{
		Version:    SchemaVersion,
		Aggregates: map[string]*aggregateYml{},
	}
	if err := filepath.WalkDir(modulesDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		name := d.Name()
		if name != "index.md" {
			return nil
		}
		rel, relErr := filepath.Rel(modulesDir, filepath.Dir(path))
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if rel == "." {
			return nil
		}
		out.Modules = append(out.Modules, moduleYml{
			Path:      rel,
			File:      "modules/" + rel + "/index.md",
			PromptVer: ModulePromptVersion,
		})
		return nil
	}); err != nil {
		return nil, err
	}
	sort.SliceStable(out.Modules, func(i, j int) bool {
		return out.Modules[i].Path < out.Modules[j].Path
	})
	ensureAggregate(out, ArchitectureKey)
	ensureAggregate(out, QuickstartKey)
	return out, nil
}

// newFreshYml returns an empty schema-v1 wikiYml with aggregates
// seeded for Plan to populate.
func newFreshYml() *wikiYml {
	yml := &wikiYml{Version: SchemaVersion, Aggregates: map[string]*aggregateYml{}}
	ensureAggregate(yml, ArchitectureKey)
	ensureAggregate(yml, QuickstartKey)
	return yml
}

// isPurelyMechanical returns true when the plan has no Agent
// work — no live-module regenerate / new, no dirty aggregate.
// Deletes are mechanical (§11.2), so we let them through too.
func isPurelyMechanical(yml *wikiYml) bool {
	for _, p := range yml.Pending {
		if p.Action == pendingActionNew || p.Action == pendingActionRegenerate {
			return false
		}
	}
	if archDirty(yml) || qstDirty(yml) {
		return false
	}
	return true
}

// fileExists / dirExists are kept local to cmd.go so the wiki
// package compiles without a separate "fs" helper file.
func fileExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && !info.IsDir()
}

func dirExists(p string) bool {
	info, err := os.Stat(p)
	return err == nil && info.IsDir()
}

// formatAckReply renders the immediate ack sent before the
// orchestrator goroutine begins. Wiki.md §14:
//
//	✅ /wiki queued
//	12 module page(s) to generate
//	2 module page(s) to delete
//	Queued in the ChatSession for /workspace/nightme
func formatAckReply(yml *wikiYml, repoRoot string) string {
	var gen, del int
	for _, p := range yml.Pending {
		switch p.Action {
		case pendingActionRegenerate, pendingActionNew:
			if p.Path == ArchitectureKey || p.Path == QuickstartKey {
				continue
			}
			gen++
		case pendingActionDelete:
			del++
		}
	}
	var b strings.Builder
	b.WriteString("✅ /wiki queued\n\n")
	fmt.Fprintf(&b, "%d module page(s) to generate\n", gen)
	if del > 0 {
		fmt.Fprintf(&b, "%d module page(s) to delete\n", del)
	}
	if archDirty(yml) {
		fmt.Fprintf(&b, "1 architecture page pending\n")
	}
	if qstDirty(yml) {
		fmt.Fprintf(&b, "1 quickstart page pending\n")
	}
	fmt.Fprintf(&b, "Queued for %s\n", repoRoot)
	return b.String()
}

func archDirty(yml *wikiYml) bool {
	if a, ok := yml.Aggregates[ArchitectureKey]; ok && a != nil && a.Dirty {
		return true
	}
	return false
}

func qstDirty(yml *wikiYml) bool {
	if a, ok := yml.Aggregates[QuickstartKey]; ok && a != nil && a.Dirty {
		return true
	}
	return false
}
