package gateway

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/session"
)

// handlerContext is the dependency bag the gateway handlers close
// over. It is built once at daemon startup and shared by every
// dispatch. Keeping the dependencies explicit makes the handlers
// trivial to unit-test with a fake session manager.
type handlerContext struct {
	manager   session.Manager
	agents    *agent.Registry
	responder Responder
}

// Responder is the channel-side equivalent of a chat reply. The
// gateway handlers do not call the channel adapter directly — they
// go through this interface so tests can assert what was sent
// without spinning up a full IM client.
type Responder interface {
	Reply(ctx context.Context, chatID, text string) error
}

// RegisterDefaultCommands wires the four v0.1 slash commands into
// gw. /help is always last so /help's list enumerates the commands
// registered so far.
func RegisterDefaultCommands(gw Gateway, mgr session.Manager, agents *agent.Registry, resp Responder) {
	hc := &handlerContext{manager: mgr, agents: agents, responder: resp}
	gw.Register(Command{
		Name:        "cwd",
		Aliases:     []string{"workspace", "ws"},
		Description: "Set workspace (session-level)",
		Handler:     hc.cwd,
	})
	gw.Register(Command{
		Name:        "run",
		Description: "Ensure CLI running (spawn or attach)",
		Handler:     hc.run,
	})
	gw.Register(Command{
		Name:        "kill",
		Description: "Stop current CLI (keep session)",
		Handler:     hc.kill,
	})
	gw.Register(Command{
		Name:        "help",
		Aliases:     []string{"?"},
		Description: "Show this help",
		Handler:     hc.help,
	})
	gw.Register(Command{
		Name:        "agents",
		Description: "List registered agents",
		Handler:     hc.listAgents,
	})
}

// cwd handles `/cwd [path]`. With no path it returns the current
// session's workspace (or a hint when none is bound). With a path
// it validates the directory and binds the chat to a session with
// that workspace. An already-running session is rejected on the
// bind path so the user explicitly /kill first.
func (h *handlerContext) cwd(ctx context.Context, msg *Message, args []string) (*CommandResult, error) {
	if len(args) == 0 {
		existing, err := h.manager.GetByChat(msg.ChatID)
		if err != nil || existing == nil {
			return h.reply(ctx, msg.ChatID, "no workspace set. Send /cwd <path> to bind one."), nil
		}
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("current workspace: %s", existing.Workspace)), nil
	}
	path := args[0]
	if strings.HasPrefix(path, "~") {
		if home, err := os.UserHomeDir(); err == nil {
			path = filepath.Join(home, strings.TrimPrefix(path, "~"))
		}
	} else if !filepath.IsAbs(path) {
		home, err := os.UserHomeDir()
		if err != nil {
			return h.reply(ctx, msg.ChatID, fmt.Sprintf("/cwd: resolve home directory: %v", err)), nil
		}
		path = filepath.Join(home, path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/cwd: %v", err)), nil
	}
	info, err := os.Stat(abs)
	if err != nil {
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/cwd: workspace not found: %v", err)), nil
	}
	if !info.IsDir() {
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/cwd: not a directory: %s", abs)), nil
	}

	// Use the existing agent name if a session is already bound,
	// otherwise fall back to the first registered agent so the
	// record is meaningful for /run.
	agentName := ""
	if existing, err := h.manager.GetByChat(msg.ChatID); err == nil && existing != nil {
		agentName = existing.Agent
	}
	if agentName == "" && h.agents != nil {
		for _, a := range h.agents.List() {
			if a != nil {
				agentName = a.Name()
				break
			}
		}
	}
	if agentName == "" {
		// No agents registered — nightme is essentially unconfigured.
		// We still record the workspace so /run can fill in the agent
		// name later (see /run handler).
		agentName = "claude"
	}

	sess, err := h.manager.CreateOrUpdate(msg.ChatID, msg.ChatType, abs, agentName, nil)
	if err != nil {
		if errors.Is(err, session.ErrChatAlreadyBound) {
			return h.reply(ctx, msg.ChatID, "session already active, /kill first"), nil
		}
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/cwd: %v", err)), nil
	}
	_ = sess // session registered; reply is the same regardless of ID
	return h.reply(ctx, msg.ChatID,
		fmt.Sprintf("Workspace set to %s. Send /run <agent> to start CLI.", abs)), nil
}

// run handles `/run <agent> [args...]`. It is a no-op when the
// session already has a live CLI; otherwise it spawns one in the
// session's workspace.
func (h *handlerContext) run(ctx context.Context, msg *Message, args []string) (*CommandResult, error) {
	if len(args) == 0 {
		return h.reply(ctx, msg.ChatID, "usage: /run <agent> [args...]"), nil
	}
	agentName := args[0]
	extra := args[1:]

	// Reject unknown agents before checking workspace so the user
	// gets the most actionable error first.
	if h.agents != nil {
		if _, err := h.agents.Get(agentName); err != nil {
			return h.reply(ctx, msg.ChatID, fmt.Sprintf("unknown agent: %s", agentName)), nil
		}
	}

	sess, err := h.manager.Run(ctx, msg.ChatID, agentName, extra)
	if err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return h.reply(ctx, msg.ChatID, "no workspace set, send /cwd <path> first"), nil
		}
		if errors.Is(err, agent.ErrUnknownAgent) {
			return h.reply(ctx, msg.ChatID, fmt.Sprintf("unknown agent: %s", agentName)), nil
		}
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/run: %v", err)), nil
	}

	// "Already running" is the safe default reply; the spec just
	// wants the user to see their CLI is alive. Producers can
	// /kill first if they want a "Started:" confirmation.
	snap := sess.Snapshot()
	text := fmt.Sprintf("Already running (pid=%d). Connected.", snap.PID)
	if err := h.trySendReply(ctx, msg.ChatID, text); err != nil {
		return nil, err
	}
	return &CommandResult{Consumed: true, Reply: text}, nil
}

// kill handles `/kill`. The session record is preserved so the
// user can /run again to restart.
func (h *handlerContext) kill(ctx context.Context, msg *Message, _ []string) (*CommandResult, error) {
	if err := h.manager.KillByChat(msg.ChatID); err != nil {
		if errors.Is(err, session.ErrSessionNotFound) {
			return h.reply(ctx, msg.ChatID, "no session to kill"), nil
		}
		return h.reply(ctx, msg.ChatID, fmt.Sprintf("/kill: %v", err)), nil
	}
	if err := h.trySendReply(ctx, msg.ChatID, "session killed"); err != nil {
		return nil, err
	}
	return &CommandResult{Consumed: true, Reply: "session killed"}, nil
}

// agents handles `/agents`. It renders the registered agent set as
// a short IM-friendly list (one bullet per agent) so the user can
// answer "/run with what name?" without leaving the chat.
//
// Format:
//
//	Registered agents:
//	• claude    — claude
//	• codex     — codex-acp
//	• opencode  — opencode acp
//
//	Use /run [name]. Omit name to use the configured default.
func (h *handlerContext) listAgents(ctx context.Context, msg *Message, _ []string) (*CommandResult, error) {
	text := renderAgents(h.agents)
	if err := h.trySendReply(ctx, msg.ChatID, text); err != nil {
		return nil, err
	}
	return &CommandResult{Consumed: true, Reply: text}, nil
}

// renderAgents builds the IM-friendly agent list. Returns the empty
// message when the registry is nil (defensive — tests sometimes
// construct handlerContext without a registry).
func renderAgents(reg *agent.Registry) string {
	if reg == nil {
		return "no agents registered"
	}
	agents := reg.List()
	if len(agents) == 0 {
		return "no agents registered"
	}
	var b strings.Builder
	b.WriteString("Registered agents:\n")
	for _, a := range agents {
		if a == nil {
			continue
		}
		name := a.Name()
		cmd := a.Command()
		args := a.Args()
		fmt.Fprintf(&b, "\u2022 %s", name)
		if cmd != "" {
			fmt.Fprintf(&b, " \u2014 %s", cmd)
		}
		if len(args) > 0 {
			fmt.Fprintf(&b, " %s", strings.Join(args, " "))
		}
		b.WriteString("\n")
	}
	b.WriteString("\nUse /run [name]. Omit name to use the configured default.")
	return b.String()
}

// help handles `/help`. It renders the registered commands.
func (h *handlerContext) help(ctx context.Context, msg *Message, _ []string) (*CommandResult, error) {
	gw, ok := gwFromContext(ctx)
	if !ok {
		return h.reply(ctx, msg.ChatID, "help: gateway unavailable"), nil
	}
	cmds := gw.ListCommands()
	text := renderHelp(cmds)
	if err := h.trySendReply(ctx, msg.ChatID, text); err != nil {
		return nil, err
	}
	return &CommandResult{Consumed: true, Reply: text}, nil
}

// renderHelp builds the multi-line help body. Exposed so tests can
// assert the layout without constructing a Gateway.
func renderHelp(cmds []Command) string {
	var b strings.Builder
	b.WriteString("Available commands:\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "/%s", c.Name)
		if c.Description != "" {
			fmt.Fprintf(&b, " — %s", c.Description)
		}
		b.WriteString("\n")
	}
	b.WriteString("\nWorkflow:\n")
	b.WriteString("  1. /cwd <path>\n")
	b.WriteString("  2. /run <agent>\n")
	b.WriteString("  3. ... work ...\n")
	b.WriteString("  4. /kill    (or restart with /run again)\n")
	b.WriteString("\nAnything else (including unknown /-commands) is sent to the agent.\n")
	return b.String()
}

// reply pushes the text through the responder (so the user sees
// it in their IM) and returns a CommandResult the Gateway can
// ignore. A nil responder degrades to a no-op so unit tests that
// only care about Reply text can skip the responder.
func (h *handlerContext) reply(ctx context.Context, chatID, text string) *CommandResult {
	_ = h.trySendReply(ctx, chatID, text)
	return &CommandResult{Reply: text, Consumed: true}
}

// trySendReply is best-effort: if the responder is nil (tests) we
// just return nil so the handler can still return its text.
func (h *handlerContext) trySendReply(ctx context.Context, chatID, text string) error {
	if h.responder == nil {
		return nil
	}
	return h.responder.Reply(ctx, chatID, text)
}

// gwFromContext extracts the Gateway from the context. The daemon
// is expected to install it via WithGateway before serving the
// chat loop. When missing, /help degrades gracefully.
func gwFromContext(ctx context.Context) (Gateway, bool) {
	if ctx == nil {
		return nil, false
	}
	gw, ok := ctx.Value(gatewayKey{}).(Gateway)
	return gw, ok
}

// WithGateway installs gw into ctx so handlers that need it
// (currently just /help) can recover it without taking it as a
// closure.
func WithGateway(ctx context.Context, gw Gateway) context.Context {
	return context.WithValue(ctx, gatewayKey{}, gw)
}

type gatewayKey struct{}
