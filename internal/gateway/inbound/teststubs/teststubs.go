// Package teststubs — test-double implementations of every
// inbound.Router dependency. Used by the dispatch_*_test.go
// files in the same parent package and (via direct import)
// by external packages that need to exercise the dispatch
// chain end-to-end (cmd/nightme/debug.go's noopCommander,
// internal/command/e2e_slash_test.go's e2eStubMgr).
//
// Why a dedicated package: each test file in the inbound
// suite was originally creating its own per-test stub type,
// which meant the same interface was implemented 2-3 times
// in the same directory. Consolidating here makes the shared
// behaviour obvious and lets new tests pick a stub off the
// shelf instead of reimplementing the contract.
//
// Every stub is safe for concurrent use unless the doc
// comment says otherwise. Stubs that record call history
// (e.g. Message.Hits, Shell.Calls) protect the record with
// a sync.Mutex and use atomic.Int32 for the hit counter.
package teststubs

import (
	"context"
	"sync"
	"sync/atomic"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	commandServices "github.com/cnlangzi/nightme/internal/command/services"
	"github.com/cnlangzi/nightme/internal/shell"
)

// ─── MessageHandler (chatsession.Manager) ──────────────────────────

// NewMessage returns the underlying *chatsession.Manager
// directly. v1.3+ multi-channel: the inbound.Router accepts a
// concrete *chatsession.Manager as its csMgr field; tests
// no longer need the old MessageHandler interface. The
// per-call GetOrCreate is exercised through the real Manager.
//
// Pass mgr=nil if your test never reaches the command or
// shell branch; the dispatch chain will get cs=nil from
// GetOrCreate and the commander / shell shims short-circuit
// gracefully.
func NewMessage(mgr *chatsession.Manager) *chatsession.Manager {
	return mgr
}

// ─── CommandDispatcher (command.Commander) ─────────────────────────

// Commander is a configurable command.Commander stub. It
// recognises a fixed set of slash commands (Recognized) and
// otherwise reports handled=true + Consumed=false (the F-51
// fall-through contract). Plain text (no "/" prefix) is
// reported as handled=false (chain continues).
//
// Used by fallthrough + shell tests that need to exercise
// the "known vs unknown slash command" branching.
type Commander struct {
	mu         sync.Mutex
	Recognized map[string]*Result // text → result for known commands
	calls      atomic.Int32
}

func NewCommander() *Commander {
	return &Commander{}
}

// Recognize registers a known command. The chain will return
// Consumed=result.Consumed + Reply=result.Reply when input
// matches the given text.
func (c *Commander) Recognize(text string, result Result) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.Recognized == nil {
		c.Recognized = make(map[string]*Result)
	}
	c.Recognized[text] = &result
}

func (c *Commander) Dispatch(_ context.Context, _ command.RuntimeServices, _ *chatsession.Manager, _ *chatsession.ChatSession, input command.SlashInput) (*command.SlashOutput, bool, error) {
	c.calls.Add(1)
	c.mu.Lock()
	defer c.mu.Unlock()
	if r, ok := c.Recognized[input.Text]; ok {
		return &command.SlashOutput{Consumed: r.Consumed, Reply: r.Reply}, true, nil
	}
	if len(input.Text) > 0 && input.Text[0] == '/' {
		// Slash command attempt, no factory — fall through.
		return &command.SlashOutput{Consumed: false}, true, nil
	}
	// Plain text — commander reports handled=false.
	return nil, false, nil
}

// Match implements command.Commander. Mirrors the Dispatch
// fall-through contract: returns true only when text is a
// recognised slash command (i.e. Recognized contains the
// text). Slash-prefixed inputs that aren't in Recognized
// return false — preserving the "/etc/passwd" passthrough
// semantics in the priority chain.
func (c *Commander) Match(text string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, ok := c.Recognized[text]; ok {
		return text, true
	}
	return "", false
}

// Calls returns the number of times Dispatch has been
// called. Concurrent-safe.
func (c *Commander) Calls() int32 { return c.calls.Load() }

// AlwaysFallThrough is a command.Commander that always
// reports handled=false. Used by action + shell tests that
// only care about one branch.
type AlwaysFallThrough struct{}

func (AlwaysFallThrough) Dispatch(_ context.Context, _ command.RuntimeServices, _ *chatsession.Manager, _ *chatsession.ChatSession, _ command.SlashInput) (*command.SlashOutput, bool, error) {
	return nil, false, nil
}

// Match implements command.Commander — always false. The
// chain skips this branch and falls through to the next
// tryDispatch.
func (AlwaysFallThrough) Match(_ string) (string, bool) { return "", false }

// ─── ShellDispatcher (shell.Dispatcher) ────────────────────────────

// Shell is a configurable shell.Dispatcher stub. It
// recognises a fixed set of "!" commands (Recognized) and
// otherwise reports Consumed=false (chain continues to the
// next tryDispatch).
//
// Used by shell tests that need to exercise the
// "recognised vs not-recognised" branching.
type Shell struct {
	mu         sync.Mutex
	Recognized map[string]bool // text → claim
	calls      atomic.Int32
}

func NewShell() *Shell {
	return &Shell{}
}

func (s *Shell) Recognize(text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Recognized == nil {
		s.Recognized = make(map[string]bool)
	}
	s.Recognized[text] = true
}

func (s *Shell) Handle(_ *chatsession.Manager, _ *chatsession.ChatSession, ir shell.InboundRequest) (*shell.ShellOutput, bool) {
	s.calls.Add(1)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Recognized[ir.Request.Text] {
		return &shell.ShellOutput{Consumed: true}, true
	}
	return &shell.ShellOutput{Consumed: false}, true
}

func (s *Shell) Calls() int32 { return s.calls.Load() }

// AlwaysFallThroughShell is a shell.Dispatcher that always
// reports handled=false. Used by action + fallthrough tests
// that only care about one branch.
type AlwaysFallThroughShell struct{}

func (AlwaysFallThroughShell) Handle(_ *chatsession.Manager, _ *chatsession.ChatSession, _ shell.InboundRequest) (*shell.ShellOutput, bool) {
	return nil, false
}

// ─── ReactionRouter (services.ReactionRouter) ──────────────────────

// Reaction is a configurable services.ReactionRouter stub.
// Consumed controls whether Handle reports the event as
// consumed (true) or drops it (false). Events records every
// call Handle receives so tests can assert on routing.
type Reaction struct {
	mu       sync.Mutex
	Consumed bool
	Events   []commandServices.ReactionEvent
	CtxSeen  context.Context
}

func NewReaction(consumed bool) *Reaction {
	return &Reaction{Consumed: consumed}
}

func (r *Reaction) Handle(ctx context.Context, _ string, ev commandServices.ReactionEvent) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Events = append(r.Events, ev)
	r.CtxSeen = ctx
	return r.Consumed
}

// ─── Fixtures (not coupled to inbound types) ───────────────────────

// Result is a test fixture for the CommandResult shape.
// Test files in `package inbound` use this with
// teststubs.Commander.Recognize, which converts to the
// real *command.SlashOutput internally:
//
//	cmd := teststubs.NewCommander()
//	cmd.Recognize("/known", teststubs.Result{Consumed: true, Reply: "handled"})
//
// Kept as a local type (not a type alias for
// inbound.CommandResult) because teststubs cannot import
// inbound (the test files in package inbound already import
// teststubs; the cycle would be fatal).
type Result struct {
	Reply    string
	Consumed bool
	Dropped  bool
}
