// Package bot — implements channel.Channel. bot is the synthetic
// channel that drives workflow YAML automations: it feeds trigger
// events into its own Incoming() (where the gateway's pumpInbound
// reads them), and receives agent replies via Send (called by the
// gateway's messages.Emitter).
//
// Locked design invariant: bot does NOT import
// internal/chatsession, internal/agentsession, or any other
// nightme internal package. bot's only nightme-facing surface is
// channel.Channel itself: pushing messages into bot.Incoming()
// and receiving them via bot.Send(). /gtw fix, /cwd, /use agent
// are all invoked by sending the corresponding slash-command
// messages; bot never calls them as Go functions.
package bot

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// Bot implements channel.Channel. See package doc for the design
// invariant.
type Bot struct {
	cfg    Config
	logger *slog.Logger

	workflows []*wfe.Workflow
	triggers  *TriggerManager
	actions   *ActionRegistry
	stateStore *StateStore

	// in is the channel the gateway's pumpInbound reads from.
	// Buffer size 256: small enough to back-pressure if bot outpaces
	// dispatch, large enough to absorb a cron tick + immediate
	// setup messages without dropping.
	in chan messages.InboundMessage

	// runsByChatID routes agent replies (delivered via Send) back
	// to the per-run reply channel. Each entry is alive for the
	// lifetime of one workflow run.
	muRuns         sync.RWMutex
	runsByChatID   map[string]*botRun
}

type botRun struct {
	runID     string
	chatID    string
	workspace string
	workflow  *wfe.Workflow
	env       map[string]string
	reply     chan string
}

// Config holds bot's external configuration. Zero value is not
// useful; populate WorkflowsDir at minimum.
type Config struct {
	// WorkflowsDir is the directory bot scans for *.yaml files.
	WorkflowsDir string

	// ActionsDir is the directory bot scans for user-defined
	// action scripts (auto-registered into the ActionRegistry).
	ActionsDir string

	// StateDir is where bot persists per-run state. Defaults to
	// ~/.nightme/workflows/state/ if empty.
	StateDir string
}

// New constructs a Bot. Workflows are loaded from cfg.WorkflowsDir
// at Start time, not here.
func New(cfg Config) *Bot {
	return &Bot{
		cfg:          cfg,
		in:           make(chan messages.InboundMessage, 256),
		runsByChatID: make(map[string]*botRun),
	}
}

// Compile-time check: Bot satisfies channel.Channel.
var _ channel.Channel = (*Bot)(nil)

// Name implements channel.Channel.
func (b *Bot) Name() string { return "bot" }

// Start implements channel.Channel. Loads workflows, builds the
// workspace→repo map, registers action channels, starts the
// trigger manager.
func (b *Bot) Start(ctx context.Context) error {
	// 1. load workflows
	wfs, err := wfe.LoadDir(b.cfg.WorkflowsDir)
	if err != nil {
		return fmt.Errorf("bot: load workflows: %w", err)
	}
	b.workflows = wfs
	b.logger.Info("bot: loaded workflows", "count", len(wfs))

	// 2. build workspace→repo map (for trigger filtering)
	wsMap, err := buildWorkspaceRepoMap(wfs)
	if err != nil {
		return fmt.Errorf("bot: build workspace map: %w", err)
	}

	// 3. register action channels (built-in + user scripts)
	if b.actions == nil {
		b.actions = NewActionRegistry()
	}
	registerBuiltinActions(b.actions)
	if b.cfg.ActionsDir != "" {
		if err := b.actions.ScanUserScripts(b.cfg.ActionsDir); err != nil {
			return fmt.Errorf("bot: scan action scripts: %w", err)
		}
	}

	// 4. init state store
	if b.stateStore == nil {
		dir, err := stateDirOrDefault(b.cfg.StateDir)
		if err != nil {
			return fmt.Errorf("bot: init state store: %w", err)
		}
		store, err := NewStateStore(dir)
		if err != nil {
			return fmt.Errorf("bot: init state store: %w", err)
		}
		b.stateStore = store
	}

	// 5. start trigger manager
	b.triggers = newTriggerManager(b.workflows, wsMap, b.onTrigger, b.logger)
	return b.triggers.Start(ctx)
}

// Stop implements channel.Channel. Stops the trigger manager.
func (b *Bot) Stop(_ context.Context) error {
	if b.triggers != nil {
		return b.triggers.Stop()
	}
	return nil
}

// Incoming implements channel.Channel. The gateway's pumpInbound
// reads from this channel.
func (b *Bot) Incoming() <-chan messages.InboundMessage { return b.in }

// Send implements channel.Channel. Called by the gateway's
// messages.Emitter when an agent reply is ready. bot looks up the
// botRun for msg.ChatID and delivers msg.Text to the run's
// reply channel.
func (b *Bot) Send(_ context.Context, msg messages.OutboundMessage) error {
	text := outboundText(msg)
	if text == "" {
		return nil // ignore non-text kinds for v0
	}
	b.muRuns.RLock()
	r, ok := b.runsByChatID[msg.ChatID]
	b.muRuns.RUnlock()
	if !ok {
		// Stale reply (run already finished or never registered).
		if b.logger != nil {
			b.logger.Debug("bot: reply for unknown chatID", "chat_id", msg.ChatID)
		}
		return nil
	}
	select {
	case r.reply <- text:
	default:
		// reply channel full; log and drop rather than block the
		// emitter (which would back-pressure the whole gateway).
		if b.logger != nil {
			b.logger.Warn("bot: reply channel full", "chat_id", msg.ChatID)
		}
	}
	return nil
}

// OnPromptEnded implements channel.Channel. No-op for bot (no UI).
func (b *Bot) OnPromptEnded(_ context.Context, _ string, _ string) {}

// HealthSnapshot implements channel.Channel.
func (b *Bot) HealthSnapshot() (string, json.RawMessage, error) {
	return b.Name(), json.RawMessage(`{"type":"bot"}`), nil
}

// SetLogger implements channel.Channel.
func (b *Bot) SetLogger(l *slog.Logger) { b.logger = l }

// BuildBlocks implements channel.Channel. bot has no rich-text;
// always returns a single text block.
func (b *Bot) BuildBlocks(text string, _ []messages.Attachment) []agent.ContentBlock {
	if text == "" {
		return nil
	}
	return []agent.ContentBlock{{Type: agent.ContentText, Text: text}}
}

// outboundText extracts the reply text from an OutboundMessage.
// Different Kinds carry the text in different fields.
func outboundText(msg messages.OutboundMessage) string {
	switch msg.Kind {
	case messages.OutReply, messages.OutThinking, messages.OutResult:
		if msg.Text != "" {
			return msg.Text
		}
		if msg.Result != nil {
			return msg.Result.Text
		}
		return ""
	}
	// Other Kinds (tool, receipt, reaction, …) are noise for the
	// workflow run; bot ignores them in v0.
	return ""
}

// stateDirOrDefault returns the configured StateDir or a default
// under the user's home directory.
func stateDirOrDefault(cfgDir string) (string, error) {
	if cfgDir != "" {
		return cfgDir, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("bot: cannot determine home dir: %w", err)
	}
	return filepath.Join(home, ".nightme", "workflows", "state"), nil
}

// gitOrigin returns the canonical owner/repo for a workspace's
// git origin, or empty string if it can't be determined.
func gitOrigin(workspace string) string {
	cmd := exec.Command("git", "-C", workspace, "remote", "get-url", "origin")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return canonicalRepoURL(strings.TrimSpace(string(out)))
}

// canonicalRepoURL normalizes a git remote URL to "owner/repo".
// Handles both HTTPS (https://github.com/foo/bar.git) and SSH
// (git@github.com:foo/bar.git) forms, with or without .git suffix.
func canonicalRepoURL(raw string) string {
	raw = strings.TrimSpace(raw)
	// Strip protocol
	if i := strings.Index(raw, "://"); i >= 0 {
		raw = raw[i+3:]
	} else if strings.HasPrefix(raw, "git@") {
		raw = strings.TrimPrefix(raw, "git@")
		if i := strings.Index(raw, ":"); i >= 0 {
			raw = raw[:i] + "/" + raw[i+1:]
		}
	}
	// Strip host (the bit before the first /)
	if i := strings.Index(raw, "/"); i >= 0 {
		raw = raw[i+1:]
	}
	// Strip .git suffix
	raw = strings.TrimSuffix(raw, ".git")
	return raw
}
