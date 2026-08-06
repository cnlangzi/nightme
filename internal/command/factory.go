package command

import (
	"context"
	"sort"
	"strings"
	"sync"
)

// Spec describes one registered slash command. The runtime
// reads Name + Aliases at registration time to build the
// dispatch table; Summary + Usage surface in /help and in
// "unknown command" replies.
type Spec struct {
	// Name is the bare command name without the leading slash,
	// e.g. "gtw" for /gtw. Must be unique across the registry.
	Name string
	// Aliases are alternative names that route to the same
	// factory (e.g. "h" -> help). Lower-cased for matching.
	Aliases []string
	// Summary is a one-line help description.
	Summary string
	// Usage is a short usage hint surfaced when args are
	// missing or invalid. Free-form; may be multi-line.
	Usage string
}

// SlashCommandFactory builds the per-command implementation.
// The runtime calls Spec() at registration time; Handle() is
// called once per inbound that names this command.
type SlashCommandFactory interface {
	// Spec returns the static Spec for this command. Safe to
	// call concurrently; the runtime reads it once per
	// registration.
	Spec() Spec
	// Handle dispatches one inbound that named this command.
	// The command package is responsible for parsing args out
	// of input.Text and returning a SlashOutput. nil error
	// means success (even if the result's Reply is empty).
	Handle(ctx context.Context, rt RuntimeServices, input SlashInput) (*SlashOutput, error)
}

// Registry holds the command dispatch table. The runtime
// owns one; the gateway sees only the Commander.
type Registry struct {
	mu      sync.Mutex
	cmds    map[string]SlashCommandFactory // keyed by lower-cased name
	aliases map[string]string              // alias -> primary name
	order   []string                       // registration order (for Specs())
}

// NewRegistry returns an empty Registry.
func NewRegistry() *Registry {
	return &Registry{
		cmds:    make(map[string]SlashCommandFactory),
		aliases: make(map[string]string),
	}
}

// Register adds cmd to the dispatch table. cmd.Spec().Name is
// the primary key; cmd.Spec().Aliases are secondary keys
// (lower-cased). A second Register for the same Name or
// alias overwrites the previous binding (last-wins).
func (r *Registry) Register(cmd SlashCommandFactory) {
	if cmd == nil {
		return
	}
	spec := cmd.Spec()
	name := strings.ToLower(strings.TrimSpace(spec.Name))
	if name == "" {
		return
	}

	r.mu.Lock()
	defer r.mu.Unlock()

	if _, exists := r.cmds[name]; !exists {
		r.order = append(r.order, name)
	}
	r.cmds[name] = cmd
	for _, a := range spec.Aliases {
		alias := strings.ToLower(strings.TrimSpace(a))
		if alias == "" || alias == name {
			continue
		}
		r.aliases[alias] = name
	}
}

// Specs returns the registered commands' Specs in registration
// order. Used by /help to enumerate.
func (r *Registry) Specs() []Spec {
	r.mu.Lock()
	defer r.mu.Unlock()

	out := make([]Spec, 0, len(r.order))
	for _, name := range r.order {
		if cmd, ok := r.cmds[name]; ok {
			out = append(out, cmd.Spec())
		}
	}
	// Stable secondary sort by Name for deterministic output.
	sort.SliceStable(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// FindByName returns the factory for the given command name
// (case-insensitive, with alias resolution). nil if not found.
func (r *Registry) FindByName(name string) SlashCommandFactory {
	key := strings.ToLower(strings.TrimSpace(name))
	if key == "" {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()

	if cmd, ok := r.cmds[key]; ok {
		return cmd
	}
	if primary, ok := r.aliases[key]; ok {
		if cmd, ok := r.cmds[primary]; ok {
			return cmd
		}
	}
	return nil
}
