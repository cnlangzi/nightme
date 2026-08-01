// Package gateway routes incoming chat messages to slash commands
// registered by nightme, or to a fallback handler (typically the
// session manager forwarding text to the live agent). See
// docs/feat/F-20-gateway.md for the original router design and
// docs/feat/F-26-gateway-hub.md for the Stage-1 hub-and-spoke
// extension.
package gateway

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
)

// Command is one nightme-level slash command. The Handler is invoked
// when the parsed command name (or any of its aliases) is matched.
// Returning a non-nil error aborts the dispatch and surfaces to the
// caller of Handle; returning a non-nil Reply with Consumed=true
// means nightme handled the message.
type Command struct {
	Name        string
	Aliases     []string
	Description string
	Handler     func(ctx context.Context, msg *InboundMessage, args []string) (*CommandResult, error)
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
type FallbackHandler func(ctx context.Context, msg *InboundMessage) error

// Gateway is the public contract for the slash-command router. It
// is implemented by the gateway struct (Stage 1; the per-channel
// inbound pump + per-session outbound pump live on top of this in
// Stage 2).
type Gateway interface {
	// Register stores cmd in the internal registry. Names are
	// case-insensitive to match IM conventions ("/Help" == "/help").
	// The first command registered for a name wins; later
	// registrations are ignored (and reported via the returned bool
	// so tests can assert).
	Register(cmd Command) (replaced bool)

	// Handle dispatches one inbound message. If a registered
	// command matches, Handle invokes its Handler and returns the
	// handler's CommandResult. If nothing matches and a fallback
	// handler was set, Handle invokes that instead. The fallback's
	// error (if any) is returned verbatim.
	Handle(ctx context.Context, msg *InboundMessage) (*CommandResult, error)

	// ListCommands returns the registered commands in
	// case-insensitive alphabetical order. Used by /help and the
	// 'nightme agents' CLI to expose the surface to the user.
	ListCommands() []Command
}

// gateway is the concrete implementation of the Gateway interface.
type gateway struct {
	mu    sync.RWMutex
	cmds  map[string]Command // canonical name -> command (case-folded)
	order []string           // insertion order so Register / Help is deterministic
	fb    FallbackHandler
}

// New constructs a Gateway. The optional fallback handler is invoked
// when no command matches the inbound message. Pass nil to drop
// unmatched messages.
func New(fallback FallbackHandler) Gateway {
	return &gateway{
		cmds: make(map[string]Command),
		fb:   fallback,
	}
}

// Register stores cmd. Names and aliases are case-folded on insert
// so Handle can do a single map lookup.
func (g *gateway) Register(cmd Command) (replaced bool) {
	name := strings.ToLower(cmd.Name)
	if _, exists := g.cmds[name]; exists {
		return true
	}
	for _, a := range cmd.Aliases {
		g.cmds[strings.ToLower(a)] = cmd
	}
	g.cmds[name] = cmd
	g.order = append(g.order, name)
	return false
}

// Handle implements the Gateway interface.
func (g *gateway) Handle(ctx context.Context, msg *InboundMessage) (*CommandResult, error) {
	if msg == nil {
		return nil, errors.New("gateway: nil message")
	}
	name, args, err := ParseCommand(strings.TrimSpace(msg.Text))
	matched := err == nil
	if err != nil {
		// Malformed slash command (e.g. lone "/"). Surface via the
		// fallback handler if registered.
		if g.fb != nil {
			return &CommandResult{}, g.fb(ctx, msg)
		}
		return &CommandResult{}, nil
	}
	if !matched {
		if g.fb != nil {
			return &CommandResult{}, g.fb(ctx, msg)
		}
		return &CommandResult{Consumed: false}, nil
	}
	g.mu.RLock()
	cmd, ok := g.cmds[strings.ToLower(name)]
	g.mu.RUnlock()
	if !ok {
		if g.fb != nil {
			return &CommandResult{}, g.fb(ctx, msg)
		}
		return &CommandResult{Consumed: false}, nil
	}
	return cmd.Handler(ctx, msg, args)
}

// ListCommands returns the registered commands in
// case-insensitive alphabetical order.
func (g *gateway) ListCommands() []Command {
	g.mu.RLock()
	defer g.mu.RUnlock()
	out := make([]Command, 0, len(g.order))
	seen := make(map[string]bool, len(g.order))
	for _, name := range g.order {
		if seen[name] {
			continue
		}
		if c, ok := g.cmds[name]; ok {
			out = append(out, c)
			seen[name] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}
