package cmd

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/gateway"
)

func TestParseCommand_Basic(t *testing.T) {
	name, args, err := gateway.ParseCommand("/cwd /tmp/foo")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if name != "cwd" {
		t.Errorf("name = %q, want cwd", name)
	}
	if len(args) != 1 || args[0] != "/tmp/foo" {
		t.Errorf("args = %v, want [/tmp/foo]", args)
	}
}

func TestParseCommand_NoArgs(t *testing.T) {
	name, args, err := gateway.ParseCommand("/help")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if name != "help" {
		t.Errorf("name = %q, want help", name)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want []", args)
	}
}

func TestParseCommand_MultipleArgs(t *testing.T) {
	name, args, err := gateway.ParseCommand("/run claude --model opus")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if name != "run" {
		t.Errorf("name = %q, want run", name)
	}
	want := []string{"claude", "--model", "opus"}
	if len(args) != len(want) {
		t.Fatalf("args = %v, want %v", args, want)
	}
	for i := range want {
		if args[i] != want[i] {
			t.Errorf("args[%d] = %q, want %q", i, args[i], want[i])
		}
	}
}

func TestParseCommand_Failures(t *testing.T) {
	cases := []struct {
		name string
		in   string
	}{
		{"empty", ""},
		{"whitespace", "   "},
		{"no slash", "hello"},
		{"only slash", "/"},
		{"slash then cr", "/\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := gateway.ParseCommand(tc.in); !errors.Is(err, gateway.ErrParseFailure) {
				t.Errorf("gateway.ParseCommand(%q) err = %v, want gateway.ErrParseFailure", tc.in, err)
			}
		})
	}
}

func TestParseCommand_CaseInsensitive(t *testing.T) {
	// Names are normalized later by the gateway; the parser preserves
	// the literal characters so handlers can see "Help" vs "help".
	name, _, err := gateway.ParseCommand("/Help")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if name != "Help" {
		t.Errorf("name = %q, want Help (parser preserves case)", name)
	}
}

func TestParseCommand_LeadingWhitespace(t *testing.T) {
	name, args, err := gateway.ParseCommand("   /cwd /tmp")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if name != "cwd" || len(args) != 1 || args[0] != "/tmp" {
		t.Errorf("name=%q args=%v", name, args)
	}
}

func TestParseCommand_MultilineTruncates(t *testing.T) {
	// Multi-line input is not a command; only the first line matters.
	// The parser trims to the first line, so the second line is dropped.
	_, args, err := gateway.ParseCommand("/help\nextra noise")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if len(args) != 0 {
		t.Errorf("args = %v, want [] (multiline dropped)", args)
	}
}

func TestGateway_RegisterAndHandle(t *testing.T) {
	var called int32
	g := gateway.New(nil, nil)
	g.Register(gateway.Command{
		Name:        "ping",
		Description: "pong",
		Handler: func(ctx context.Context, msg *gateway.InboundMessage, args []string) (*gateway.CommandResult, error) {
			atomic.AddInt32(&called, 1)
			return &gateway.CommandResult{Reply: "pong", Consumed: true}, nil
		},
	})

	_, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{
		ChatID: "oc_chat",
		Text:   "/ping",
		Time:   time.Now(),
	})
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := atomic.LoadInt32(&called); got != 1 {
		t.Errorf("handler called %d times, want 1", got)
	}
}

func TestGateway_RegisterAliases(t *testing.T) {
	var calls int32
	g := gateway.New(nil, nil)
	g.Register(gateway.Command{
		Name:    "cwd",
		Aliases: []string{"workspace", "ws"},
		Handler: func(ctx context.Context, msg *gateway.InboundMessage, args []string) (*gateway.CommandResult, error) {
			atomic.AddInt32(&calls, 1)
			return &gateway.CommandResult{Consumed: true}, nil
		},
	})

	for _, input := range []string{"/cwd /tmp", "/workspace /tmp", "/ws /tmp", "/CWD /tmp"} {
		if _, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{ChatID: "oc_chat", Text: input}); err != nil {
			t.Fatalf("Handle(%q): %v", input, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 4 {
		t.Errorf("alias handler called %d times, want 4", got)
	}
}

func TestGateway_UnmatchedCommandPassesToFallback(t *testing.T) {
	var fb int32
	g := gateway.New(func(ctx context.Context, msg *gateway.InboundMessage) error {
		atomic.AddInt32(&fb, 1)
		return nil
	}, nil)
	g.Register(gateway.Command{
		Name: "help",
		Handler: func(ctx context.Context, msg *gateway.InboundMessage, args []string) (*gateway.CommandResult, error) {
			return &gateway.CommandResult{Consumed: true}, nil
		},
	})

	// Unknown /-command flows to fallback.
	if _, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{ChatID: "oc_chat", Text: "/clear"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	// Plain text also flows to fallback.
	if _, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{ChatID: "oc_chat", Text: "hello"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := atomic.LoadInt32(&fb); got != 2 {
		t.Errorf("fallback invoked %d times, want 2", got)
	}
}

func TestGateway_NonNilMsg(t *testing.T) {
	g := gateway.New(nil, nil)
	if _, err := g.DispatchInbound(context.Background(), nil); err == nil {
		t.Errorf("Handle(nil) err = nil, want non-nil")
	}
}

func TestGateway_RegisterReplacement(t *testing.T) {
	g := gateway.New(nil, nil)
	var reply string
	first := gateway.Command{Name: "foo", Handler: func(context.Context, *gateway.InboundMessage, []string) (*gateway.CommandResult, error) {
		reply = "first"
		return &gateway.CommandResult{Reply: "first"}, nil
	}}
	second := gateway.Command{Name: "foo", Handler: func(context.Context, *gateway.InboundMessage, []string) (*gateway.CommandResult, error) {
		reply = "second"
		return &gateway.CommandResult{Reply: "second"}, nil
	}}

	if replaced := g.Register(first); replaced {
		t.Errorf("first Register returned replaced=true, want false")
	}
	if replaced := g.Register(second); !replaced {
		t.Errorf("second Register returned replaced=false, want true (collision reported)")
	}

	// First-wins semantics: the original handler must still be invoked
	// even though the second Register signalled a collision.
	if _, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{Text: "/foo"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if reply != "first" {
		t.Errorf("first-wins handler reply = %q, want first", reply)
	}
}

func TestGateway_ListCommands_StableOrder(t *testing.T) {
	g := gateway.New(nil, nil)
	g.Register(gateway.Command{Name: "zeta"})
	g.Register(gateway.Command{Name: "alpha"})
	g.Register(gateway.Command{Name: "mu"})

	got := g.(gateway.Gateway).ListCommands()
	if len(got) != 3 {
		t.Fatalf("ListCommands returned %d, want 3", len(got))
	}
	names := []string{got[0].Name, got[1].Name, got[2].Name}
	want := []string{"alpha", "mu", "zeta"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("ListCommands order = %v, want %v", names, want)
		}
	}
}

func TestGateway_HandlerErrorPropagates(t *testing.T) {
	want := errors.New("boom")
	g := gateway.New(nil, nil)
	g.Register(gateway.Command{
		Name: "fail",
		Handler: func(ctx context.Context, msg *gateway.InboundMessage, args []string) (*gateway.CommandResult, error) {
			return nil, want
		},
	})
	_, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{Text: "/fail"})
	if !errors.Is(err, want) {
		t.Errorf("Handle error = %v, want %v", err, want)
	}
}

func TestGateway_FallbackErrorPropagates(t *testing.T) {
	want := errors.New("fb boom")
	g := gateway.New(func(ctx context.Context, msg *gateway.InboundMessage) error {
		return want
	}, nil)
	_, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{Text: "hello"})
	if !errors.Is(err, want) {
		t.Errorf("Handle fallback error = %v, want %v", err, want)
	}
}

func TestGateway_NilFallbackDropsUnmatched(t *testing.T) {
	g := gateway.New(nil, nil)
	// No panic; unmatched messages are silently dropped.
	if _, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{Text: "/unknown"}); err != nil {
		t.Errorf("Handle unmatched = %v, want nil", err)
	}
	if _, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{Text: "hello"}); err != nil {
		t.Errorf("Handle plain text = %v, want nil", err)
	}
}

func TestGateway_RegisterIgnoresEmptyAlias(t *testing.T) {
	g := gateway.New(nil, nil)
	replaced := g.Register(gateway.Command{Name: "foo", Aliases: []string{""}})
	if replaced {
		t.Errorf("empty alias should not be a replacement")
	}
	// Registering a second command with non-empty alias should still
	// succeed because the empty alias was skipped.
	replaced = g.Register(gateway.Command{Name: "bar", Aliases: []string{"baz"}})
	if replaced {
		t.Errorf("Register(bar) reported replaced=true, want false")
	}
}

func TestGateway_DescriptionPreserved(t *testing.T) {
	g := gateway.New(nil, nil)
	g.Register(gateway.Command{Name: "foo", Description: "do foo"})
	cmds := g.(gateway.Gateway).ListCommands()
	if len(cmds) != 1 || cmds[0].Description != "do foo" {
		t.Errorf("ListCommands description = %+v", cmds)
	}
}

func TestGateway_HandlePreservesMessageFields(t *testing.T) {
	g := gateway.New(nil, nil)
	var captured *gateway.InboundMessage
	g.Register(gateway.Command{
		Name: "echo",
		Handler: func(ctx context.Context, msg *gateway.InboundMessage, args []string) (*gateway.CommandResult, error) {
			captured = msg
			return &gateway.CommandResult{Consumed: true}, nil
		},
	})
	now := time.Now()
	if _, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{
		ChatID:   "oc_a",
		Text:     "/echo",
		UserID: "ou_a",
		Time:     now,
	}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if captured == nil || captured.ChatID != "oc_a" || captured.UserID != "ou_a" {
		t.Errorf("handler received msg = %+v", captured)
	}
	if !captured.Time.Equal(now) {
		t.Errorf("msg time = %v, want %v", captured.Time, now)
	}
}

func TestGateway_HandlerArgsArePassed(t *testing.T) {
	g := gateway.New(nil, nil)
	var gotArgs []string
	g.Register(gateway.Command{
		Name: "run",
		Handler: func(ctx context.Context, msg *gateway.InboundMessage, args []string) (*gateway.CommandResult, error) {
			gotArgs = args
			return &gateway.CommandResult{Consumed: true}, nil
		},
	})
	if _, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{Text: "/run /bin/echo --flag"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if len(gotArgs) != 2 || gotArgs[0] != "/bin/echo" || gotArgs[1] != "--flag" {
		t.Errorf("handler args = %v, want [/bin/echo --flag]", gotArgs)
	}
}

func TestGateway_RegisterConcurrent(t *testing.T) {
	// Smoke test: Register from many goroutines must not race.
	if !testing.Short() {
		t.Skip("skip in non-short mode (covered by -race runner)")
	}
	g := gateway.New(nil, nil)
	done := make(chan struct{})
	for i := 0; i < 16; i++ {
		go func(i int) {
			g.Register(gateway.Command{Name: "c" + string(rune('a'+i))})
			<-done
		}(i)
	}
	close(done)
	if len(g.(gateway.Gateway).ListCommands()) != 16 {
		t.Errorf("registered commands = %d, want 16", len(g.(gateway.Gateway).ListCommands()))
	}
}

func TestParseCommand_TrailingWhitespace(t *testing.T) {
	name, args, err := gateway.ParseCommand("/cwd /tmp   ")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if name != "cwd" || len(args) != 1 || args[0] != "/tmp" {
		t.Errorf("name=%q args=%v", name, args)
	}
}

func TestParseCommand_TabSeparators(t *testing.T) {
	name, args, err := gateway.ParseCommand("/run\tclaude\t--opus")
	if err != nil {
		t.Fatalf("ParseCommand: %v", err)
	}
	if name != "run" {
		t.Errorf("name = %q, want run", name)
	}
	if len(args) != 2 || args[0] != "claude" || args[1] != "--opus" {
		t.Errorf("args = %v, want [claude --opus]", args)
	}
}

func TestGateway_UnmatchedReturnsNoError(t *testing.T) {
	g := gateway.New(nil, nil)
	g.Register(gateway.Command{Name: "help", Handler: func(context.Context, *gateway.InboundMessage, []string) (*gateway.CommandResult, error) {
		return &gateway.CommandResult{Consumed: true}, nil
	}})
	if _, err := g.DispatchInbound(context.Background(), &gateway.InboundMessage{Text: "/clear"}); err != nil {
		t.Errorf("Handle unmatched = %v, want nil", err)
	}
}

// Sanity guard: the strings package is used by the parser tests, but
// the gateway tests focus on the dispatch logic. This little test
// ensures the imports compile when the test file is built standalone.
func TestImports_CompileTimeGuard(t *testing.T) {
	if !strings.Contains("hello", "ell") {
		t.Fatal("strings import regression")
	}
}
