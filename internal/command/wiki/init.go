// Package wiki — /wiki init command.
//
// The init command runs a single Agent prompt that asks the
// Agent to read the repository, design a module structure,
// and write one Markdown file per module under
// <REPO_ROOT>/wiki/modules/. Init is a one-shot command:
// there is no Job registry, no Apply phase, no batched
// prompts. The wiki is established or it is not.
//
// docs/Wiki.md is authoritative for the contract.
package wiki

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// initPrompt is the Agent's task for /wiki init. The runtime
// substitutes <REPO_ROOT> with the resolved repository root
// before submitting. The Agent writes plain Markdown files
// directly — no frontmatter, no central index, no
// `wiki.yml`. Each file's identity is its filename.
//
// docs/Wiki.md §5.
const initPrompt = `# /wiki init — establish the wiki module structure

You are the lead architect of the repository at <REPO_ROOT>.
You know this codebase deeply. You are creating a durable
project documentation asset that the team will rely on
for years.

## Tasks

### 1. Read the codebase

Use your file tools to understand this project — its purpose,
its major conceptual areas, the patterns it uses, and what
a new contributor needs to know. Read enough to ground your
design.

### 2. Design the module structure

A module is a CONCEPT — a unit of coherent responsibility.
Modules may span directories; one directory may contain
multiple concepts. Design the partitioning that makes the
most sense to a new contributor who will read this wiki
next month.

### 3. Write per-module files

For each module, write one file at:

  <REPO_ROOT>/wiki/modules/<name>.md

The filename <name> is the module's identity. Use kebab-case
(lowercase letters, digits, and hyphens; 1-5 words). Do not
include any extension other than .md.

Each file MUST be plain Markdown with exactly this shape:

  # <Display Name>

  > <One-line purpose — what this concept IS responsible for>

  <body — your call>

That's it. No frontmatter. No covers list. No metadata block.
The runtime only reads three things: the filename, the H1,
and the blockquote. Everything else is yours.

## When done

Reply with a one-paragraph summary of the modules you created.
`
// --arch. Designed for LLM attention: critical action in
// the first two lines (primacy), file format as a code
// block (visual prominence), output protocol at the end
// (recency). Rules are bolded so they survive the
// lost-in-the-middle effect.
const archPrompt = `# Write wiki/architecture.md

You are the lead architect of <REPO_ROOT>.

**Your job:** write <REPO_ROOT>/wiki/architecture.md.

The wiki module pages are at <REPO_ROOT>/wiki/modules/` + "*.md" + `.
They cover per-concept detail; this page covers the system
shape. An architecture document is the prose overview of
a system for readers who want a mental model before
drilling into source — see how any major open-source
project writes its root-level ARCHITECTURE.md (Kubernetes,
Prometheus) for the conventions readers expect.

**When uncertain about a claim's accuracy, verify against
the source code.** The modules are summaries, not authority.

## File format

` + "```" + `
# Architecture

> <one-line purpose — what the system IS, in one sentence>

<body>
` + "```" + `

## When done

Reply with a one-paragraph summary of what the page
covers and which module pages it references.
`
// initTimeout is how long /wiki init waits for the Agent to
// respond. The Agent is reading the codebase and writing N
// files, so the budget is generous. The user can interrupt
// via the chat's normal stop path.
const initTimeout = 30 * time.Minute

// archMsgIDSuffix is appended to the user message ID for
// the architecture.md Agent prompt. Two distinct message
// IDs are required so the runtime can correlate events
// separately for the modules prompt and the arch prompt.
const archMsgIDSuffix = "-arch"

// InitOptions selects which steps /wiki init runs. Each
// field is an independent toggle. When all three are false
// the caller is expected to short-circuit before RunInit
// (only llms.txt is deterministic; the other two need an
// Agent prompt).
//
// The full flag matrix:
//
//	/wiki init                       → Modules, Llmstxt, Arch = true
//	/wiki init --modules             → Modules only
//	/wiki init --llmstxt             → Llmstxt only (if modules on disk: also arch)
//	/wiki init --arch                → Arch only
//	/wiki init --modules --llmstxt   → Modules + Llmstxt
//	/wiki init --modules --arch      → Modules + Arch
//	/wiki init --llmstxt --arch      → Llmstxt + Arch (if modules on disk)
//	/wiki init --modules --llmstxt --arch → all three
type InitOptions struct {
	Modules bool
	Llmstxt bool
	Arch    bool
}

// RunInit is the body of /wiki init. It runs in a goroutine
// spawned by Handle; the Ack reply is sent by Handle and the
// final reply is sent by RunInit through the chat's Emitter.
//
// The function returns the final reply text and a non-nil
// error when validation failed. The error message is also
// embedded in the reply.
func RunInit(ctx context.Context, cs *chatsession.ChatSession, input chatsession.Message, repoRoot string, opts InitOptions) (string, error) {
	if repoRoot == "" {
		return "❌ /wiki init: repository root could not be resolved", fmt.Errorf("empty repoRoot")
	}
	wikiRoot := filepath.Join(repoRoot, "wiki")

	// Modules step: refused when wiki/modules/ already
	// exists. Skipped entirely when --modules is not set
	// (modules must already be on disk from a prior run).
	if opts.Modules && HasExistingWiki(wikiRoot) {
		return "❌ /wiki init: wiki/modules/ already exists; refusing to overwrite", fmt.Errorf("wiki already exists")
	}

	var modulesSummary string
	if opts.Modules {
		reply, err := runModulesStep(ctx, cs, input, repoRoot)
		if err != nil {
			return reply, err
		}
		modulesSummary = reply
	}

	// Validate the module set on disk. The validation only
	// makes sense against modules we (or a prior run) wrote
	// this turn; --llmstxt / --arch on a fresh repo would
	// otherwise report "rejected" for files that were never
	// in scope. Validate when modules were just written, OR
	// when downstream steps need a clean on-disk wiki.
	var passed []ModuleResult
	if opts.Modules {
		var failed []ModuleResult
		passed, failed = ValidateAll(wikiRoot)
		if len(failed) > 0 {
			return formatFailureReply(modulesSummary, failed), fmt.Errorf("validation failed")
		}
	} else if opts.Arch || opts.Llmstxt {
		var failed []ModuleResult
		passed, failed = ValidateAll(wikiRoot)
		if len(failed) > 0 {
			return formatPreconditionReply(wikiRoot, failed), fmt.Errorf("existing wiki validation failed")
		}
	}

	// Build wiki/llms.txt deterministically from the
	// validated module set. Skipped when --llmstxt is not set.
	llmsNote := ""
	if opts.Llmstxt {
		if body, err := BuildLlmsTxt(repoRoot); err != nil {
			llmsNote = fmt.Sprintf("\n⚠ llms.txt build failed: %v (re-run with `--wiki init --llmstxt`)", err)
		} else if err := os.WriteFile(filepath.Join(repoRoot, "wiki", "llms.txt"), []byte(body), 0o644); err != nil {
			llmsNote = fmt.Sprintf("\n⚠ llms.txt write failed: %v", err)
		}
	}

	// Architecture step: submit a second Agent prompt for
	// the cross-cutting overview. Skipped when no module
	// files exist (the arch prompt needs them in context).
	archNote := ""
	if opts.Arch {
		archNote = runArchIfPossible(ctx, cs, input, repoRoot)
	}

	return formatReply(opts, modulesSummary, llmsNote, archNote, passed), nil
}

// formatReply composes the user-visible reply for a /wiki init
// completion. The exact shape depends on which steps ran.
//
// passed is the validated-module count from RunInit; it is
// non-nil only when --modules was set this turn, so the
// success line correctly distinguishes "N modules written"
// from "0 modules written" (--llmstxt / --arch only).
func formatReply(opts InitOptions, modulesSummary, llmsNote, archNote string, passed []ModuleResult) string {
	ranArch := opts.Arch && archNote != ""
	switch {
	case opts.Modules && opts.Llmstxt && opts.Arch:
		// Full three-step path.
		return formatSuccessReply(modulesSummary, passed) + llmsNote + archNote
	case opts.Modules && !opts.Llmstxt && !opts.Arch:
		// --modules only.
		return "✅ /wiki init --modules\n\nwiki/modules/ rebuilt.\n" + modulesSummary
	case !opts.Modules && opts.Llmstxt && !opts.Arch:
		// --llmstxt only.
		return "✅ /wiki init --llmstxt\n\nwiki/llms.txt rebuilt." + llmsNote
	case !opts.Modules && !opts.Llmstxt && opts.Arch:
		// --arch only. The arch prompt needs module files
		// in context; runArchIfPossible returns an empty
		// note when none exist, so surface that explicitly
		// rather than pretending the file was written.
		if !ranArch {
			return "⚠ /wiki init --arch\n\nno wiki/modules/ found; architecture step needs existing modules.\nRe-run with `--modules` (or `--modules --arch`) first."
		}
		return "✅ /wiki init --arch\n\nwiki/architecture.md rebuilt." + archNote
	default:
		// Combinations: modules+llmstxt, modules+arch,
		// llmstxt+arch. modules+llmstxt and modules+arch
		// always run their steps (modules step always
		// writes); llmstxt+arch mirrors the bare --arch
		// shape when the arch step was a no-op.
		if !opts.Modules && opts.Arch && !ranArch {
			return "⚠ /wiki init --llmstxt --arch\n\nllms.txt rebuilt; architecture step skipped — no wiki/modules/ found.\nRe-run with `--modules` (or `--modules --arch`) first." + llmsNote
		}
		return "✅ /wiki init\n\n" + modulesSummary + llmsNote + archNote
	}
}

// runArchIfPossible runs the architecture Agent prompt
// when module files exist. Returns an empty string when
// there are no modules to summarize. Failures surface as
// inline warnings — the rest of the init reply is not
// blocked by an arch failure.
func runArchIfPossible(ctx context.Context, cs *chatsession.ChatSession, input chatsession.Message, repoRoot string) string {
	modsDir := filepath.Join(repoRoot, "wiki", "modules")
	entries, err := os.ReadDir(modsDir)
	if err != nil {
		return "" // no modules dir → nothing to do
	}
	hasModule := false
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".md") {
			hasModule = true
			break
		}
	}
	if !hasModule {
		return ""
	}
	reply, err := runArchStep(ctx, cs, input, repoRoot)
	if err != nil {
		return fmt.Sprintf("\n⚠ architecture step: %v", err)
	}
	return "\n\narchitecture.md written (see prompt summary above)." + "\n" + reply
}

// runModulesStep submits the modules extraction prompt and
// waits for the Agent to finish. Returns the Agent's summary
// text for inclusion in the final reply.
func runModulesStep(ctx context.Context, cs *chatsession.ChatSession, input chatsession.Message, repoRoot string) (string, error) {
	collector := startCollector(cs, input.ID)
	prompt := strings.ReplaceAll(initPrompt, "<REPO_ROOT>", repoRoot)
	msg := chatsession.Message{
		ID:     input.ID,
		ChatID: input.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: prompt}},
		Kind:   chatsession.MessageKindQueue,
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		stopCollector(collector)
		return "", fmt.Errorf("queue failed: %w", err)
	}
	if err := waitForPromptEnd(ctx, cs, input.ID, initTimeout); err != nil {
		stopCollector(collector)
		return "", err
	}
	stopCollector(collector)
	return collector.text(), nil
}

// runArchStep submits the architecture.md prompt and
// validates the produced file. The Agent session is the
// same as the modules step (no re-select), so the modules
// are still in its working context.
func runArchStep(ctx context.Context, cs *chatsession.ChatSession, input chatsession.Message, repoRoot string) (string, error) {
	archID := input.ID + archMsgIDSuffix
	collector := startCollector(cs, archID)
	prompt := strings.ReplaceAll(archPrompt, "<REPO_ROOT>", repoRoot)
	msg := chatsession.Message{
		ID:     archID,
		ChatID: input.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: prompt}},
		Kind:   chatsession.MessageKindQueue,
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		stopCollector(collector)
		return "", fmt.Errorf("queue failed: %w", err)
	}
	if err := waitForPromptEnd(ctx, cs, archID, initTimeout); err != nil {
		stopCollector(collector)
		return "", err
	}
	stopCollector(collector)

	// Validate architecture.md on disk.
	archPath := filepath.Join(repoRoot, "wiki", "architecture.md")
	h1, bq := readHeader(archPath)
	if h1 == "" {
		return "", fmt.Errorf("architecture.md missing H1")
	}
	if bq == "" {
		return "", fmt.Errorf("architecture.md missing blockquote after H1")
	}
	return collector.text(), nil
}

// RunLlmsTxtOnly runs the deterministic llms.txt build
// without any Agent prompt. Used by `/wiki init --llmstxt`
// when the modules already exist from a previous run.
//
// wiki/modules/ must already contain at least one valid
// module file; otherwise the reply reports a missing wiki.
func RunLlmsTxtOnly(repoRoot string) (string, error) {
	if repoRoot == "" {
		return "❌ /wiki init --llmstxt: repository root could not be resolved", fmt.Errorf("empty repoRoot")
	}
	wikiRoot := filepath.Join(repoRoot, "wiki")
	if !HasExistingWiki(wikiRoot) {
		return "❌ /wiki init --llmstxt: wiki/modules/ does not exist; run `/wiki init` first", fmt.Errorf("wiki missing")
	}
	body, err := BuildLlmsTxt(repoRoot)
	if err != nil {
		return fmt.Sprintf("❌ /wiki init --llmstxt: %v", err), err
	}
	if err := os.WriteFile(filepath.Join(repoRoot, "wiki", "llms.txt"), []byte(body), 0o644); err != nil {
		return fmt.Sprintf("❌ /wiki init --llmstxt: write failed: %v", err), err
	}
	return "✅ /wiki init --llmstxt\n\nwiki/llms.txt rebuilt.\nSee the file for the discovery index.", nil
}

// formatSuccessReply composes the success reply: the Agent's
// summary is preserved as the leading content, then a status
// line counts the modules created.
func formatSuccessReply(summary string, passed []ModuleResult) string {
	var b strings.Builder
	if summary != "" {
		b.WriteString(strings.TrimRight(summary, "\n"))
		b.WriteString("\n\n---\n\n")
	}
	b.WriteString("✅ /wiki init\n\n")
	fmt.Fprintf(&b, "%d module(s) created under wiki/modules/.\n", len(passed))
	b.WriteString("Read the summary above for module names.")
	return b.String()
}

// formatFailureReply composes the failure reply: summary
// plus a list of failed modules. Modules that the Agent
// named in its summary but did not write to disk are
// surfaced here so the user can ask the Agent to retry.
func formatFailureReply(summary string, failed []ModuleResult) string {
	var b strings.Builder
	if summary != "" {
		b.WriteString(strings.TrimRight(summary, "\n"))
		b.WriteString("\n\n---\n\n")
	}
	b.WriteString("❌ /wiki init\n\n")
	b.WriteString("Modules written but rejected:\n")
	for _, f := range failed {
		if f.Name != "" {
			fmt.Fprintf(&b, "- %s (%s): %s\n", f.Name, f.File, f.Reason)
		} else {
			fmt.Fprintf(&b, "- %s: %s\n", f.File, f.Reason)
		}
	}
	return b.String()
}

// formatPreconditionReply surfaces a validation failure
// for a wiki that already exists on disk — distinct from
// formatFailureReply so the user does not see "Modules
// written but rejected" when nothing was written this turn.
func formatPreconditionReply(wikiRoot string, failed []ModuleResult) string {
	var b strings.Builder
	b.WriteString("❌ /wiki init\n\n")
	fmt.Fprintf(&b, "Existing wiki at %s has invalid module files:\n", filepath.ToSlash(wikiRoot))
	for _, f := range failed {
		if f.Name != "" {
			fmt.Fprintf(&b, "- %s (%s): %s\n", f.Name, f.File, f.Reason)
		} else {
			fmt.Fprintf(&b, "- %s: %s\n", f.File, f.Reason)
		}
	}
	b.WriteString("\nFix the named files or remove wiki/modules/ and re-run with `--modules`.")
	return b.String()
}

// BuildLlmsTxt writes wiki/llms.txt by reading wiki/modules/
// and the project header. Deterministic — no Agent call.
//
// Project header source:
//   - <REPO_ROOT>/AGENTS.md first H1 / first blockquote
//     (preferred — it is the project's own self-description)
//   - <REPO_ROOT>/README.md first H1
//     (fallback if AGENTS.md is missing or has no header)
//   - repoRoot basename + "Project"
//     (last-resort fallback)
//
// Module list source: every .md under wiki/modules/. Each
// entry shows the module's H1 display name + blockquote
// purpose. Order is alphabetical by basename.
//
// The file is the llmstxt.org discovery index format.
func BuildLlmsTxt(repoRoot string) (string, error) {
	if repoRoot == "" {
		return "", fmt.Errorf("BuildLlmsTxt: empty repoRoot")
	}
	wikiDir := filepath.Join(repoRoot, "wiki")
	modsDir := filepath.Join(wikiDir, "modules")

	name, purpose := projectHeader(repoRoot)

	entries, err := scanModuleEntries(modsDir)
	if err != nil {
		return "", err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].name < entries[j].name })

	var b strings.Builder
	fmt.Fprintf(&b, "# %s\n\n", name)
	if purpose != "" {
		fmt.Fprintf(&b, "> %s\n\n", purpose)
	}

	if len(entries) == 0 {
		b.WriteString("<!-- no modules written yet -->\n")
		return b.String(), nil
	}

	b.WriteString("## Modules\n")
	for _, e := range entries {
		fmt.Fprintf(&b, "- [%s](modules/%s.md): %s\n", e.name, e.name, e.purpose)
	}
	return b.String(), nil
}

// projectHeader returns (name, purpose) by reading the
// project's own description files. Order of preference is
// AGENTS.md, README.md, repoRoot basename.
func projectHeader(repoRoot string) (name, purpose string) {
	name, purpose = readHeader(filepath.Join(repoRoot, "AGENTS.md"))
	if name != "" {
		return name, purpose
	}
	name, purpose = readHeader(filepath.Join(repoRoot, "README.md"))
	if name != "" {
		return name, purpose
	}
	base := filepath.Base(repoRoot)
	if base == "" || base == "." || base == "/" {
		base = "Project"
	}
	return base, ""
}

// llmsEntry is one module's contribution to llms.txt.
type llmsEntry struct {
	name    string
	purpose string
}

// scanModuleEntries walks wiki/modules/*.md and extracts
// each file's H1 + first blockquote. Modules that fail to
// parse are skipped (their absence surfaces in the next
// ValidateAll pass). The directory may not exist; that is
// not an error.
func scanModuleEntries(modsDir string) ([]llmsEntry, error) {
	entries, err := os.ReadDir(modsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []llmsEntry
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		path := filepath.Join(modsDir, e.Name())
		_, bq := readHeader(path)
		out = append(out, llmsEntry{name: name, purpose: bq})
	}
	return out, nil
}

// readHeader reads a Markdown file and returns the first H1
// line and the first blockquote line after it. Returns
// ("", "") when the file does not exist, cannot be read, or
// has neither an H1 nor a blockquote.
//
// Used for both AGENTS.md / README.md (project header source)
// and module .md files (module entry source).
func readHeader(path string) (h1, bq string) {
	f, err := os.Open(path)
	if err != nil {
		return "", ""
	}
	defer f.Close()
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 1<<16), 1<<20)
	state := 0
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		switch state {
		case 0:
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "# ") {
				h1 = strings.TrimPrefix(line, "# ")
				state = 1
			}
		case 1:
			if line == "" {
				continue
			}
			if strings.HasPrefix(line, "> ") {
				bq = strings.TrimPrefix(line, "> ")
				return h1, bq
			}
			// Hit a non-blockquote, non-empty line before the
			// blockquote; stop scanning.
			return h1, ""
		}
	}
	return h1, bq
}

// eventCollector accumulates text events from a single Agent
// prompt. The runtime keeps a pointer so it can stop the
// subscription when the prompt ends.
type eventCollector struct {
	mu     sync.Mutex
	buf    strings.Builder
	cancel func()
}

func (c *eventCollector) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.buf.String()
}

func (c *eventCollector) append(ev *agent.AgentEvent) {
	if ev == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	switch ev.Kind {
	case agent.EventAgentText:
		c.buf.WriteString(ev.Text)
		c.buf.WriteString("\n")
	case agent.EventAgentResult:
		if ev.Result != nil && ev.Result.Text != "" {
			c.buf.WriteString(ev.Result.Text)
			c.buf.WriteString("\n")
		}
	}
}

// startCollector subscribes to AgentEventBus for the duration
// of the prompt. It returns a handle whose text() reflects
// the Agent's accumulated reply.
func startCollector(cs *chatsession.ChatSession, userMsgID string) *eventCollector {
	c := &eventCollector{}
	if cs == nil || cs.AgentEventBus == nil {
		return c
	}
	unsub := cs.AgentEventBus.Subscribe(func(env chatsession.AgentEventEnvelope) bool {
		if env.UserMsgID != userMsgID {
			return false
		}
		c.append(env.Event)
		return false
	})
	c.cancel = unsub
	return c
}

func stopCollector(c *eventCollector) {
	if c == nil || c.cancel == nil {
		return
	}
	c.cancel()
}

// waitForPromptEnd blocks until PromptEndBus delivers an
// event for userMsgID, or ctx is cancelled, or the timeout
// elapses. Returns nil on clean termination.
func waitForPromptEnd(ctx context.Context, cs *chatsession.ChatSession, userMsgID string, timeout time.Duration) error {
	if cs == nil || cs.PromptEndBus == nil {
		return fmt.Errorf("PromptEndBus unavailable")
	}
	evCh := make(chan agentsession.PromptEndedEvent, 1)
	unsub := cs.PromptEndBus.Subscribe(func(e agentsession.PromptEndedEvent) bool {
		if e.UserMsgID != userMsgID {
			return false
		}
		select {
		case evCh <- e:
		default:
		}
		return true
	})
	defer unsub()

	if ctx == nil {
		ctx = context.Background()
	}
	timeoutCh := time.NewTimer(timeout)
	defer timeoutCh.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case ev := <-evCh:
		if ev.Reason != agentsession.PromptEndClean {
			return fmt.Errorf("prompt ended with reason %v", ev.Reason)
		}
		return nil
	case <-timeoutCh.C:
		return fmt.Errorf("wiki init timed out after %s", timeout)
	}
}
