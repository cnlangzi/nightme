// Package gateway routes incoming chat messages to slash commands
// registered by nightme, or to a fallback handler (typically the
// session manager forwarding text to the live agent). See
// docs/feat/F-20-gateway.md for the full design.
package gateway

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
)

// Message is the normalized shape that every channel adapter produces
// before handing input to the Gateway. The struct is intentionally
// small so adapters can build it cheaply.
type Message struct {
	ChatID   string
	Text     string
	SenderID string
	Time     time.Time

	// ChatType is forwarded from channel.Message (see
	// internal/channel). "p2p" / "group" / "topic_group" / "".
	// Gateway commands use it to decide chat-aware behaviour
	// (e.g. /cwd in a DM is fine; /sessions list is fine in
	// either).
	ChatType string

	// MessageID is forwarded from channel.Message.MessageID. Used
	// by the adapter layer to download message resources (image /
	// file / audio / video); gateway commands do not consume it.
	MessageID string

	// Attachments is forwarded from channel.Message.Attachments.
	// The fallback handler (session manager forwarding) joins Text
	// with attachment local paths into a single user turn via
	// buildForwardedText before dispatching to the agent.
	Attachments []channel.Attachment
}

// Command is one nightme-level slash command. The Handler is
// invoked when the parsed command name (or any of its aliases) is
// matched. Returning a non-nil error aborts the dispatch and
// surfaces to the caller of Handle; returning a non-nil Reply with
// Consumed=true means nightme handled the message.
type Command struct {
	Name        string
	Aliases     []string
	Description string
	Handler     func(ctx context.Context, msg *Message, args []string) (*CommandResult, error)
}

// CommandResult is the per-dispatch outcome.
//
// Consumed=false means the message was not a nightme command; the
// caller of Handle should pass the message to the fallback handler
// (typically the session manager).
type CommandResult struct {
	Reply    string
	Consumed bool
}

// FallbackHandler is invoked when the Gateway decides the message is
// not a nightme command. A typical implementation forwards the
// message text to the session's underlying agent.
type FallbackHandler func(ctx context.Context, msg *Message) error

// Gateway is the public contract for the slash-command router.
type Gateway interface {
	// Register stores cmd in the internal registry. Names are
	// case-insensitive to match IM conventions ("/Help" == "/help").
	// The first command registered for a name wins; later
	// registrations are ignored (and reported via the returned bool
	// so tests can assert).
	Register(cmd Command) (replaced bool)

	// Handle dispatches msg to the matching command, or to the
	// fallback handler when no command matches.
	Handle(ctx context.Context, msg *Message) error

	// ListCommands returns the registered commands in deterministic
	// (alphabetical) order. Useful for /help.
	ListCommands() []Command
}

// MemoryGateway is the production implementation. It is safe for
// concurrent use after construction.
type MemoryGateway struct {
	mu       sync.RWMutex
	commands map[string]Command
	fallback FallbackHandler
}

// New constructs a MemoryGateway with the given fallback handler. A
// nil fallback is allowed; the gateway simply drops unmatched
// messages in that case.
func New(fallback FallbackHandler) *MemoryGateway {
	return &MemoryGateway{
		commands: make(map[string]Command),
		fallback: fallback,
	}
}

// Register inserts cmd by its Name and every Alias. See the
// Gateway interface for the replacement semantics.
func (g *MemoryGateway) Register(cmd Command) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	keys := append([]string{cmd.Name}, cmd.Aliases...)
	replaced := false
	for _, k := range keys {
		if k == "" {
			continue
		}
		key := normalizeKey(k)
		if _, exists := g.commands[key]; exists {
			replaced = true
			continue
		}
		g.commands[key] = cmd
	}
	return replaced
}

// Handle parses msg.Text, looks up the matched command, and
// dispatches. A non-slash or unparseable message flows to the
// fallback. The Handler's CommandResult is ignored here: the
// gateway itself does not send replies (the channel adapter does),
// but the Handler is responsible for any side effects (e.g. session
// lifecycle, channel messages).
func (g *MemoryGateway) Handle(ctx context.Context, msg *Message) error {
	if msg == nil {
		return nil
	}
	name, args, err := ParseCommand(msg.Text)
	if err != nil {
		// Not a slash command — pass through to the agent.
		if g.fallback != nil {
			return g.fallback(ctx, msg)
		}
		return nil
	}

	g.mu.RLock()
	cmd, ok := g.commands[normalizeKey(name)]
	fallback := g.fallback
	g.mu.RUnlock()

	if !ok {
		if fallback != nil {
			return fallback(ctx, msg)
		}
		return nil
	}
	if cmd.Handler == nil {
		return nil
	}
	if _, err := cmd.Handler(ctx, msg, args); err != nil {
		return err
	}
	return nil
}

// ListCommands returns the registered commands in deterministic
// (alphabetical) order.
func (g *MemoryGateway) ListCommands() []Command {
	g.mu.RLock()
	defer g.mu.RUnlock()

	byKey := make(map[string]Command, len(g.commands))
	for _, c := range g.commands {
		byKey[normalizeKey(c.Name)] = c
	}
	out := make([]Command, 0, len(byKey))
	for _, c := range byKey {
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// normalizeKey lowercases to match IM conventions where /Help and
// /help should be equivalent.
func normalizeKey(name string) string {
	return strings.ToLower(name)
}
