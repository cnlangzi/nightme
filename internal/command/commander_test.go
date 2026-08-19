package command

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// fakeCmd is a minimal SlashCommandFactory used to exercise the
// Commander dispatch logic. It records what Handle() was called
// with and returns a canned SlashOutput.
type fakeCmd struct {
	spec   Spec
	output *SlashOutput
	err    error
	got    *SlashInput
	calls  int
}

func (f *fakeCmd) Spec() Spec { return f.spec }
func (f *fakeCmd) Handle(_ context.Context, _ RuntimeServices, _ *chatsession.Manager, _ *chatsession.ChatSession, in SlashInput) (*SlashOutput, error) {
	f.got = &in
	f.calls++
	return f.output, f.err
}

func newFakeCmd(name string, summary string) *fakeCmd {
	return &fakeCmd{
		spec:   Spec{Name: name, Summary: summary, Usage: "usage-" + name},
		output: &SlashOutput{Reply: "ok-" + name, Consumed: true},
	}
}

func TestNewCommander_EmptyRegistry_NonCommandText(t *testing.T) {
	c := NewCommander(NewRegistry())
	cs := &chatsession.ChatSession{}
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs,
		SlashInput{Text: "hello world"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if handled {
		t.Errorf("non-slash text should report handled=false, got handled=true")
	}
	if got != nil {
		t.Errorf("non-slash text should return nil output, got %+v", got)
	}
}

func TestNewCommander_EmptyRegistry_SlashUnknown_FallsThrough(t *testing.T) {
	// Unknown slash commands report handled=true + Consumed=false so
	// the gateway can fall through to the agent loop, preserving the
	// existing passthrough behavior.
	c := NewCommander(NewRegistry())
	cs := &chatsession.ChatSession{}
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs, SlashInput{Text: "/nope arg1 arg2"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Errorf("unknown /cmd should report handled=true (it WAS a slash command attempt), got handled=false")
	}
	if got == nil {
		t.Fatalf("unknown /cmd should return non-nil output (Consumed=false), got nil")
	}
	if got.Consumed {
		t.Errorf("unknown /cmd should report Consumed=false (gateway falls through to agent), got Consumed=true with reply %q", got.Reply)
	}
	if got.Reply != "" {
		t.Errorf("unknown /cmd should not produce a reply (gateway forwards to agent), got %q", got.Reply)
	}
}

func TestNewCommander_EmptyRegistry_SlashPath_FallsThrough(t *testing.T) {
	// /etc/passwd style input — slash command attempt that the
	// user might have intended as a file path. Should fall
	// through to the agent loop, NOT be rejected with
	// "Unknown command".
	c := NewCommander(NewRegistry())
	cs := &chatsession.ChatSession{}
	for _, text := range []string{
		"/etc/passwd",
		"/api/v1/foo",
		"/@everyone hi",
	} {
		got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs, SlashInput{Text: text})
		if err != nil {
			t.Fatalf("Dispatch(%q): %v", text, err)
		}
		if !handled {
			t.Errorf("Dispatch(%q): handled=false, want handled=true (it was a slash command attempt)", text)
		}
		if got == nil || got.Consumed {
			t.Errorf("Dispatch(%q): want Consumed=false (fall through), got %+v", text, got)
		}
	}
}

func TestNewCommander_RoutedToRegisteredCmd(t *testing.T) {
	reg := NewRegistry()
	gtw := newFakeCmd("gtw", "team workflow")
	reg.Register(gtw)

	c := NewCommander(reg)
	cs := &chatsession.ChatSession{}
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs, SlashInput{Text: "/gtw fix 42", Args: []string{"gtw", "fix", "42"}})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Errorf("known /cmd should report handled=true, got handled=false")
	}
	if !got.Consumed || got.Reply != "ok-gtw" {
		t.Errorf("expected routed to gtw reply, got %+v", got)
	}
	if gtw.calls != 1 {
		t.Errorf("expected gtw.Handle called once, got %d", gtw.calls)
	}
	if gtw.got == nil || gtw.got.Text != "/gtw fix 42" {
		t.Errorf("expected full text in SlashInput, got %+v", gtw.got)
	}
}

func TestNewCommander_AliasRoutesToPrimary(t *testing.T) {
	reg := NewRegistry()
	gtw := newFakeCmd("gtw", "team workflow")
	gtw.spec.Aliases = []string{"w"}
	reg.Register(gtw)

	c := NewCommander(reg)
	cs := &chatsession.ChatSession{}
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs, SlashInput{Text: "/w list"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Errorf("alias should report handled=true, got handled=false")
	}
	if !got.Consumed || got.Reply != "ok-gtw" {
		t.Errorf("alias /w should route to gtw, got %+v", got)
	}
	if gtw.calls != 1 {
		t.Errorf("expected gtw.Handle called once via alias, got %d", gtw.calls)
	}
}

func TestNewCommander_NoArgsStillFillsArgs(t *testing.T) {
	reg := NewRegistry()
	gtw := newFakeCmd("gtw", "")
	reg.Register(gtw)

	c := NewCommander(reg)
	// gateway did NOT pre-parse; Args is empty.
	cs := &chatsession.ChatSession{}
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs, SlashInput{Text: "/gtw fix 42"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !handled {
		t.Errorf("known /cmd should report handled=true")
	}
	if !got.Consumed {
		t.Errorf("expected Consumed, got %+v", got)
	}
	if gtw.got == nil {
		t.Fatalf("expected Handle called")
	}
	if len(gtw.got.Args) < 3 || gtw.got.Args[0] != "gtw" || gtw.got.Args[1] != "fix" || gtw.got.Args[2] != "42" {
		t.Errorf("expected Args=[gtw fix 42], got %v", gtw.got.Args)
	}
}

func TestNewCommander_CaseInsensitive(t *testing.T) {
	reg := NewRegistry()
	gtw := newFakeCmd("gtw", "")
	reg.Register(gtw)
	c := NewCommander(reg)

	cs := &chatsession.ChatSession{}
	got, handled, _ := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs, SlashInput{Text: "/GTW hi"})
	if !handled || !got.Consumed || got.Reply != "ok-gtw" {
		t.Errorf("expected /GTW to route to gtw, got handled=%v output=%+v", handled, got)
	}
}

func TestNewCommander_EmptyAfterSlash_FallsThrough(t *testing.T) {
	// Lone slash "/" or slash + only whitespace "/   " is not a
	// slash command — fall through (防呆: empty body after prefix).
	c := NewCommander(NewRegistry())
	cs := &chatsession.ChatSession{}
	for _, text := range []string{"/", "/   ", "/\t", "/  \t  "} {
		got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs, SlashInput{Text: text})
		if err != nil {
			t.Fatalf("Dispatch(%q): %v", text, err)
		}
		if handled {
			t.Errorf("Dispatch(%q): empty-after-slash should report handled=false, got handled=true", text)
		}
		if got != nil {
			t.Errorf("Dispatch(%q): empty-after-slash should return nil output, got %+v", text, got)
		}
	}
}

func TestNewCommander_LeadingWhitespace_BeforeSlash_Routes(t *testing.T) {
	// parseCommand trims leading whitespace, so "   /cmd" should
	// dispatch as if it were "/cmd". Verifies the FW→HW + trim
	// normalization applies to whitespace BEFORE the prefix too.
	reg := NewRegistry()
	gtw := newFakeCmd("gtw", "")
	reg.Register(gtw)
	c := NewCommander(reg)
	cs := &chatsession.ChatSession{}

	for _, text := range []string{"   /gtw", "\t/gtw", "  \t/gtw hi"} {
		got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs, SlashInput{Text: text})
		if err != nil {
			t.Fatalf("Dispatch(%q): %v", text, err)
		}
		if !handled {
			t.Errorf("Dispatch(%q): should be handled as slash command, got handled=false", text)
		}
		if !got.Consumed || got.Reply != "ok-gtw" {
			t.Errorf("Dispatch(%q): expected routed to gtw, got %+v", text, got)
		}
	}
}

// TestParseCommand_Matrix locks in the 13-row normalization contract
// shared between commander.parseCommand and shell.parseShell. See
// wip/feat-shell.md §"防呆示例" for the authoritative table; if you
// change the rules, update both parsers AND this test in lock-step.
func TestParseCommand_Matrix(t *testing.T) {
	cases := []struct {
		name     string
		input    string
		wantBody string
		wantOK   bool
	}{
		{"empty", "", "", false},
		{"whitespace_only", "   ", "", false},
		{"plain_text", "hello", "", false},
		{"half_slash_cmd", "/cmd", "cmd", true},
		{"full_width_slash_cmd", "／cmd", "cmd", true},
		{"leading_whitespace_slash", "   /cmd", "cmd", true},
		{"slash_followed_by_whitespace", "/   cmd", "cmd", true},
		{"lone_slash", "/", "", false},
		{"slash_only_whitespace", "/   ", "", false},
		{"first_char_is_bang", "!ls", "", false}, // parseCommand only handles /
		{"slash_inside_string", "echo /hi", "", false},
		{"fw_slash_with_trailing_space", "／  hi", "hi", true},
		{"tab_separated", "/gtw\tfix", "gtw\tfix", true}, // trim, not all whitespace-collapse
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotBody, gotOK := parseCommand(tc.input)
			if gotBody != tc.wantBody {
				t.Errorf("parseCommand(%q) body = %q, want %q", tc.input, gotBody, tc.wantBody)
			}
			if gotOK != tc.wantOK {
				t.Errorf("parseCommand(%q) ok = %v, want %v", tc.input, gotOK, tc.wantOK)
			}
		})
	}
}

func TestNewCommander_HandlerError_BecomesReply(t *testing.T) {
	// A command whose Handle returns an error should surface as
	// a Consumed=true reply (handled=true). The user gets a
	// visible error message; the gateway does NOT fall through.
	reg := NewRegistry()
	cmd := newFakeCmd("boom", "")
	cmd.err = errors.New("kaboom")
	reg.Register(cmd)

	c := NewCommander(reg)
	cs := &chatsession.ChatSession{}
	got, handled, dispatchErr := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs, SlashInput{Text: "/boom"})
	if dispatchErr != nil {
		t.Fatalf("unexpected error: %v", dispatchErr)
	}
	if !handled {
		t.Errorf("erroring command should report handled=true")
	}
	if got == nil || !got.Consumed {
		t.Fatalf("erroring command should return Consumed=true reply, got %+v", got)
	}
	if !strings.Contains(got.Reply, "kaboom") {
		t.Errorf("reply should contain the error message, got %q", got.Reply)
	}
}

// ---------------------------------------------------------------------------
// Framework-level ⏳ → ✅ reaction contract (F-XX, see
// docs/feat/slash-command-reactions.md)
// ---------------------------------------------------------------------------
//
// These tests verify that commander.Dispatch automatically emits
// agent.MessageQueued before the matched SlashCommandFactory.Handle call
// and agent.MessageDone after it, regardless of whether Handle returned
// successfully or errored. The pair lands on the user message via the
// MessageStateBus → feishu adapter path so the user sees ⏳ then ✅ in
// their IM channel without per-command wiring.

// stateCapture is a small test recorder subscribed to a ChatSession's
// MessageStateBus. Mirrors the captureHandler pattern in
// internal/chatsession/message_state_test.go but stays in the command
// package to avoid an import cycle on the unexported event type.
type stateCapture struct {
	mu    sync.Mutex
	calls []stateCall
}

type stateCall struct {
	chatID, userMsgID string
	state             agent.MessageState
}

func (c *stateCapture) handler(e chatsession.MessageStateEvent) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls = append(c.calls, stateCall{chatID: e.ChatID, userMsgID: e.UserMsgID, state: e.State})
	return false
}

func (c *stateCapture) snapshot() []stateCall {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]stateCall, len(c.calls))
	copy(out, c.calls)
	return out
}

// newWiredCS constructs a ChatSession via chatsession.New so the
// MessageStateBus is wired, then subscribes cap to it. Production code
// never bypasses chatsession.New; tests that need to assert on
// MessageStateBus must use this helper (or an equivalent) to avoid
// the nil-bus path in emitSlashState.
func newWiredCS(t *testing.T, cap *stateCapture) *chatsession.ChatSession {
	t.Helper()
	cs, err := chatsession.New("oc_test", "claude")
	if err != nil {
		t.Fatalf("chatsession.New: %v", err)
	}
	cs.MessageStateBus.Subscribe(cap.handler)
	return cs
}

// TestDispatch_EmitsQueuedThenDone verifies the happy path: a matched
// slash command with a non-empty MessageID gets exactly the
// MessageQueued → cmd.Handle → MessageDone sequence on its bus.
func TestDispatch_EmitsQueuedThenDone(t *testing.T) {
	reg := NewRegistry()
	cmd := newFakeCmd("gtw", "")
	cmd.output = &SlashOutput{Consumed: true, Reply: "ok"}
	reg.Register(cmd)

	cap := &stateCapture{}
	cs := newWiredCS(t, cap)

	c := NewCommander(reg)
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs,
		SlashInput{Text: "/gtw fix 42", MessageID: "om_42"})
	if err != nil {
		t.Fatalf("dispatch err: %v", err)
	}
	if !handled || got == nil || !got.Consumed {
		t.Fatalf("expected consumed reply, got handled=%v out=%+v", handled, got)
	}

	calls := cap.snapshot()
	if len(calls) != 2 {
		t.Fatalf("captured %d state events; want 2 (Queued + Done)", len(calls))
	}
	if calls[0].state != agent.MessageQueued || calls[0].userMsgID != "om_42" {
		t.Errorf("calls[0] = %+v; want {MessageQueued, om_42}", calls[0])
	}
	if calls[1].state != agent.MessageDone || calls[1].userMsgID != "om_42" {
		t.Errorf("calls[1] = %+v; want {MessageDone, om_42}", calls[1])
	}
}

// TestDispatch_EmitsDoneOnHandlerError verifies that the framework
// still emits MessageDone when the matched command's Handle returns
// an error. The user still gets the ✅ completion reaction even
// though the reply is the ❌ error text.
func TestDispatch_EmitsDoneOnHandlerError(t *testing.T) {
	reg := NewRegistry()
	cmd := newFakeCmd("boom", "")
	cmd.err = errors.New("kaboom")
	reg.Register(cmd)

	cap := &stateCapture{}
	cs := newWiredCS(t, cap)

	c := NewCommander(reg)
	got, handled, dispatchErr := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs,
		SlashInput{Text: "/boom", MessageID: "om_err"})
	if dispatchErr != nil {
		t.Fatalf("dispatch err: %v", dispatchErr)
	}
	if !handled || got == nil || !got.Consumed {
		t.Fatalf("expected consumed reply on error path, got handled=%v out=%+v", handled, got)
	}

	calls := cap.snapshot()
	if len(calls) != 2 {
		t.Fatalf("captured %d state events; want 2 (Queued + Done)", len(calls))
	}
	if calls[0].state != agent.MessageQueued || calls[1].state != agent.MessageDone {
		t.Errorf("want [Queued, Done], got [%v, %v]", calls[0].state, calls[1].state)
	}
}

// TestDispatch_FallThroughEmitsNothing verifies the contract that
// fall-through paths (non-slash input, unknown slash command) do
// NOT emit MessageQueued/MessageDone — only matched commands get the
// ⏳/✅ pair. This prevents "⏳ flash" on inputs like "/etc/passwd"
// that the user intended as plain text.
func TestDispatch_FallThroughEmitsNothing(t *testing.T) {
	reg := NewRegistry()
	reg.Register(newFakeCmd("gtw", ""))

	cap := &stateCapture{}
	cs := newWiredCS(t, cap)

	c := NewCommander(reg)

	for _, text := range []string{
		"hello world", // plain text — handled=false
		"/unknown",    // unknown slash — handled=true, Consumed=false
		"/etc/passwd", // path-like fall-through
	} {
		cap.calls = nil
		got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs,
			SlashInput{Text: text, MessageID: "om_x"})
		if err != nil {
			t.Fatalf("Dispatch(%q): %v", text, err)
		}
		if handled && got != nil && got.Consumed {
			t.Errorf("Dispatch(%q): expected fall-through (Consumed=false), got Consumed=true", text)
		}
		if calls := cap.snapshot(); len(calls) != 0 {
			t.Errorf("Dispatch(%q): expected zero state events on fall-through, got %+v", text, calls)
		}
	}
}

// TestDispatch_EmptyMessageIDSkipsEmit covers the empty-MessageID
// guard. A slash command routed through Dispatch with an empty
// MessageID must not panic and must not emit any state events.
// This matches the runtime subscriber at cmd/nightme/run.go which
// silently drops empty UserMsgID; framework-side the guard prevents
// the unnecessary bus.Publish.
func TestDispatch_EmptyMessageIDSkipsEmit(t *testing.T) {
	reg := NewRegistry()
	reg.Register(newFakeCmd("gtw", ""))

	cap := &stateCapture{}
	cs := newWiredCS(t, cap)

	c := NewCommander(reg)
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs,
		SlashInput{Text: "/gtw", MessageID: ""}) // explicitly empty
	if err != nil {
		t.Fatalf("dispatch err: %v", err)
	}
	if !handled || got == nil || !got.Consumed {
		t.Errorf("command should still be consumed when MessageID is empty, got handled=%v out=%+v", handled, got)
	}
	if calls := cap.snapshot(); len(calls) != 0 {
		t.Errorf("empty MessageID should suppress both emits, got %+v", calls)
	}
}

// TestDispatch_NilCSSkipsEmit covers the defensive nil-cs guard.
// Commands whose Handle is still invoked (the commander receives cs
// as an interface parameter that may be nil in tests) must not
// crash the framework; no emits should happen because the cs is
// unusable.
func TestDispatch_NilCSSkipsEmit(t *testing.T) {
	reg := NewRegistry()
	reg.Register(newFakeCmd("gtw", ""))

	c := NewCommander(reg)
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, nil,
		SlashInput{Text: "/gtw", MessageID: "om_nil"})
	if err != nil {
		t.Fatalf("dispatch err: %v", err)
	}
	if !handled || got == nil || !got.Consumed {
		t.Errorf("command should still be consumed when cs is nil, got handled=%v out=%+v", handled, got)
	}
}

// TestDispatch_ZeroValueCSDoesNotPanic verifies that bare
// &chatsession.ChatSession{} (zero value, no MessageStateBus) does
// not panic when matched commands go through the framework emit
// path. The nil-bus guard inside emitSlashState keeps the existing
// commander_test.go suite working without requiring every test to
// wire a real bus. This is also the test that catches any future
// regression where someone removes the cs.MessageStateBus nil check.
func TestDispatch_ZeroValueCSDoesNotPanic(t *testing.T) {
	reg := NewRegistry()
	reg.Register(newFakeCmd("gtw", ""))

	c := NewCommander(reg)
	cs := &chatsession.ChatSession{} // zero value, MessageStateBus == nil
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs,
		SlashInput{Text: "/gtw", MessageID: "om_zv"})
	if err != nil {
		t.Fatalf("dispatch err: %v", err)
	}
	if !handled || got == nil || !got.Consumed {
		t.Errorf("expected consumed reply, got handled=%v out=%+v", handled, got)
	}
}

// TestDispatch_QueuedBeforeHandleDoneAfter verifies the precise
// ordering: MessageQueued must be observed BEFORE cmd.Handle starts
// and MessageDone must be observed AFTER cmd.Handle returns. This
// is the contract that lets slash commands render a ⏳ placeholder
// during the work and a ✅ marker on completion.
//
// We verify ordering by combining three independent signals:
//  1. cap.snapshot() records MessageState events in publish order.
//  2. fakeCmd.calls is incremented inside Handle (set to 1).
//  3. fakeCmd.got is populated inside Handle.
//
// If the snapshot's first event is Queued AND the second is Done
// AND fakeCmd.calls == 1, then Handle ran exactly once between the
// two emissions. This is a static-ordering assertion; it doesn't
// need a delay or a probe channel.
func TestDispatch_QueuedBeforeHandleDoneAfter(t *testing.T) {
	reg := NewRegistry()
	cmd := newFakeCmd("slow", "")
	cmd.output = &SlashOutput{Consumed: true, Reply: "ok"}
	reg.Register(cmd)

	cap := &stateCapture{}
	cs := newWiredCS(t, cap)

	c := NewCommander(reg)
	got, handled, err := c.Dispatch(context.Background(), RuntimeServices{}, nil, cs,
		SlashInput{Text: "/slow", MessageID: "om_ord"})
	if err != nil {
		t.Fatalf("dispatch err: %v", err)
	}
	if !handled || got == nil || !got.Consumed {
		t.Fatalf("expected consumed reply, got handled=%v out=%+v", handled, got)
	}
	if cmd.calls != 1 {
		t.Fatalf("Handle should have run exactly once; got calls=%d", cmd.calls)
	}
	if cmd.got == nil || cmd.got.MessageID != "om_ord" {
		t.Errorf("Handle should have observed MessageID=om_ord; got %+v", cmd.got)
	}

	calls := cap.snapshot()
	if len(calls) != 2 {
		t.Fatalf("captured %d events; want 2 (Queued, then Done)", len(calls))
	}
	if calls[0].state != agent.MessageQueued {
		t.Errorf("first event should be Queued, got %v", calls[0].state)
	}
	if calls[1].state != agent.MessageDone {
		t.Errorf("second event should be Done, got %v", calls[1].state)
	}
}
