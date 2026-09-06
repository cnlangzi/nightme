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
	"context"
	"fmt"
	"path/filepath"
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

// initTimeout is how long /wiki init waits for the Agent to
// respond. The Agent is reading the codebase and writing N
// files, so the budget is generous. The user can interrupt
// via the chat's normal stop path.
const initTimeout = 30 * time.Minute

// RunInit is the body of /wiki init. It runs in a goroutine
// spawned by Handle; the Ack reply is sent by Handle and the
// final reply is sent by RunInit through the chat's Emitter.
//
// The function returns the final reply text and a non-nil
// error when validation failed. The error message is also
// embedded in the reply.
func RunInit(ctx context.Context, cs *chatsession.ChatSession, input chatsession.Message, repoRoot string) (string, error) {
	if repoRoot == "" {
		return "❌ /wiki init: repository root could not be resolved", fmt.Errorf("empty repoRoot")
	}
	wikiRoot := filepath.Join(repoRoot, "wiki")
	if HasExistingWiki(wikiRoot) {
		return "❌ /wiki init: wiki/modules/ already exists; refusing to overwrite", fmt.Errorf("wiki already exists")
	}

	// Subscribe to AgentEventBus BEFORE submitting so the
	// Agent's last text event is observed. The text is the
	// summary the runtime surfaces in the final reply.
	collector := startCollector(cs, input.ID)

	// Build and submit the init prompt.
	prompt := strings.ReplaceAll(initPrompt, "<REPO_ROOT>", repoRoot)
	msg := chatsession.Message{
		ID:     input.ID,
		ChatID: input.ChatID,
		Blocks: []agent.ContentBlock{{Type: agent.ContentText, Text: prompt}},
		Kind:   chatsession.MessageKindQueue,
	}
	if err := cs.QueueUserMessage(msg); err != nil {
		stopCollector(collector)
		return fmt.Sprintf("❌ /wiki init: queue failed: %v", err), err
	}

	// Wait for PromptEnd.
	if err := waitForPromptEnd(ctx, cs, input.ID, initTimeout); err != nil {
		stopCollector(collector)
		return fmt.Sprintf("❌ /wiki init: %v", err), err
	}
	stopCollector(collector)

	// Validate the file system.
	passed, failed := ValidateAll(wikiRoot)

	if len(failed) > 0 {
		return formatFailureReply(collector.text(), failed), fmt.Errorf("validation failed")
	}
	if len(passed) == 0 {
		return "❌ /wiki init: no module files found under wiki/modules/", fmt.Errorf("no modules")
	}
	return formatSuccessReply(collector.text(), passed), nil
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
