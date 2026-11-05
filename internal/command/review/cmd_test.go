package review

// Dispatcher tests for /review.
//
// v7: Handle is fully async — it returns Consumed=true and launches
// a goroutine to do the actual review work. The synchronous error
// paths (arg check, no active AS, unknown agent) still return inline
// replies; only the starter.Review call is async.
//
// The async behavior is covered by:
//   - internal/agent/review_test.go (tests agent.Review itself
//     using a fakeStarter that records RunOnce calls + injects
//     into a fake)
//   - internal/agent/review_per_bridge_test.go (tests the
//     per-bridge delegation contract)
//   - F-review.md §3.5 smoke test (real /tmp/review-smoke run
//     against the live daemon, where the chat session's
//     readpump continues processing events while the review
//     goroutine runs RunOnce in a subprocess)
//
// The reason we don't add a full async-handle test here: building
// a real *chatsession.ChatSession for the dispatcher requires
// NewManager + persistence + spawner setup, which is heavy for
// what's a structural check (Handle launches a goroutine and
// returns immediately). The async contract is well-isolated
// in the agent package, where it's tested directly.

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/messages"
)

// TestSpec_RejectsArgs covers the arg-rejection path. /review takes
// no positional args, so a stray word or an unknown flag gets a
// "❌" reply with the offending token echoed.
//
// The wording comes from the shared lexer (command.ParseCmdArgs):
// an unknown flag reports "unknown flag", a stray word reports
// "unexpected positional argument".
//
// (command.Reply always sets Consumed=true — the runtime shim
// consumes the slash output and routes Reply to the channel. We
// only check Reply text here, not Consumed.)
func TestSpec_RejectsArgs(t *testing.T) {
	cases := []struct {
		name           string
		args           []string
		wantArgInReply string
	}{
		{"single extra arg", []string{"review", "foo"}, "foo"},
		{"multiple extra args", []string{"review", "foo", "bar"}, "foo"},
		{"flag-looking arg", []string{"review", "--base", "main"}, "--base"},
		{"em-dash variant", []string{"review", "—agent", "codex"}, "—agent"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, err := dispatchWithNoCS(t, tc.args)
			if err != nil {
				t.Fatalf("Handle returned error: %v", err)
			}
			if out == nil {
				t.Fatal("Handle returned nil SlashOutput")
			}
			if out.Reply == "" {
				t.Fatal("Handle returned empty Reply; expected error message")
			}
			if !strings.Contains(out.Reply, "unknown flag") &&
				!strings.Contains(out.Reply, "unexpected positional argument") {
				t.Errorf("Reply %q does not contain expected error", out.Reply)
			}
			// The first extra arg should be echoed back so the
			// user knows which one was the offender.
			if !strings.Contains(out.Reply, tc.wantArgInReply) {
				t.Errorf("Reply %q does not echo offending arg %q", out.Reply, tc.wantArgInReply)
			}
		})
	}
}

// TestParseReviewArgs covers the flag parser in isolation. The
// dispatcher delegates to parseReviewArgs, which delegates to the
// shared lexer.
func TestParseReviewArgs(t *testing.T) {
	cases := []struct {
		name     string
		argv     []string
		wantSpec Spec
		wantErr  bool
	}{
		{"empty", []string{}, Spec{}, false},
		{"--agent codex", []string{"--agent", "codex"}, Spec{Agent: "codex"}, false},
		{"-a codex (short form)", []string{"-a", "codex"}, Spec{Agent: "codex"}, false},
		{"--agent at end", []string{"--agent", "dsh"}, Spec{Agent: "dsh"}, false},
		{"-g", []string{"-g"}, Spec{Global: true}, false},
		{"--global", []string{"--global"}, Spec{Global: true}, false},
		{"--unset", []string{"--unset"}, Spec{Unset: true}, false},
		{"-a codex -g", []string{"-a", "codex", "-g"}, Spec{Agent: "codex", Global: true}, false},
		{"-g --unset", []string{"-g", "--unset"}, Spec{Global: true, Unset: true}, false},
		{"--agent without value", []string{"--agent"}, Spec{}, true},
		{"-a without value", []string{"-a"}, Spec{}, true},
		{"positional arg rejected", []string{"foo"}, Spec{}, true},
		{"unknown flag rejected", []string{"--base", "main"}, Spec{}, true},
		{"empty agent name rejected", []string{"--agent", ""}, Spec{}, true},
		{"blank agent name rejected", []string{"--agent", "   "}, Spec{}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			spec, err := parseReviewArgs(tc.argv)
			if (err != nil) != tc.wantErr {
				t.Fatalf("parseReviewArgs(%v) err = %v, wantErr %v", tc.argv, err, tc.wantErr)
			}
			if !tc.wantErr && spec != tc.wantSpec {
				t.Errorf("parseReviewArgs(%v) = %+v, want %+v", tc.argv, spec, tc.wantSpec)
			}
		})
	}
}

// TestValidateReviewFlags pins the error strings of the flag
// combinations /review cannot act on.
func TestValidateReviewFlags(t *testing.T) {
	cases := []struct {
		name    string
		spec    Spec
		wantErr string
	}{
		{"unset without global", Spec{Unset: true}, "--unset requires --global"},
		{"unset with agent", Spec{Unset: true, Global: true, Agent: "pi"}, "--unset and --agent are mutually exclusive"},
		{"global without agent", Spec{Global: true}, "--global requires --agent <name>"},
		{"agent alone", Spec{Agent: "pi"}, ""},
		{"global with agent", Spec{Global: true, Agent: "pi"}, ""},
		{"global with unset", Spec{Global: true, Unset: true}, ""},
		{"nothing", Spec{}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateReviewFlags(tc.spec)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Fatalf("validateReviewFlags(%+v) = %v, want nil", tc.spec, err)
			case tc.wantErr != "" && err == nil:
				t.Fatalf("validateReviewFlags(%+v) = nil, want %q", tc.spec, tc.wantErr)
			case tc.wantErr != "" && err.Error() != tc.wantErr:
				t.Fatalf("validateReviewFlags(%+v) = %q, want %q", tc.spec, err.Error(), tc.wantErr)
			}
		})
	}
}

// TestResolveRunner covers the four tiers of the runner chain:
// -a / --agent → the global scope → the session's agent → none.
func TestResolveRunner(t *testing.T) {
	registered := newTestAgents(t).register("review-resolve-agent")

	cases := []struct {
		name              string
		cli, cfg, session string
		wantName          string
		wantWarn          string
	}{
		{
			name: "tier 1 wins over the global scope and the session",
			cli:  "cli-agent", cfg: registered.name, session: "session-agent",
			wantName: "cli-agent",
		},
		{
			name: "tier 2 wins over the session",
			cfg:  registered.name, session: "session-agent",
			wantName: registered.name,
		},
		{
			name:     "tier 3 when there is no global scope",
			session:  "session-agent",
			wantName: "session-agent",
		},
		{
			name: "an unregistered global scope falls back to tier 3",
			cfg:  "review-unregistered-agent", session: "session-agent",
			wantName: "session-agent",
			wantWarn: "review-unregistered-agent",
		},
		{
			name:     "tier 4 when nothing names an agent",
			wantName: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, warn := resolveRunner(tc.cli, tc.cfg, tc.session)
			if got != tc.wantName {
				t.Errorf("resolveRunner = %q, want %q", got, tc.wantName)
			}
			switch {
			case tc.wantWarn == "" && warn != "":
				t.Errorf("unexpected warning: %q", warn)
			case tc.wantWarn != "" && !strings.Contains(warn, tc.wantWarn):
				t.Errorf("warning %q does not name %q", warn, tc.wantWarn)
			}
		})
	}
}

// TestHandle_GlobalScopePersistsTheRunner covers R5: `-a <name> -g`
// stores the runner in the global config and confirms the write.
func TestHandle_GlobalScopePersistsTheRunner(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("NIGHTME_CONFIG", path)

	starter := newTestAgents(t).register("review-global-agent")
	cs := newReviewChatSession(t, starter.name, dir)

	out := dispatchReview(t, cs, "review", "-a", starter.name, "-g")

	if !strings.Contains(out.Reply, "✅ review.agent = "+starter.name+" (global)") {
		t.Fatalf("reply does not confirm the write: %q", out.Reply)
	}
	if !strings.Contains(out.Reply, "→ file: "+path) {
		t.Fatalf("reply does not name the file: %q", out.Reply)
	}
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Review.Agent != starter.name {
		t.Errorf("review.agent = %q, want %q", cfg.Review.Agent, starter.name)
	}
}

// TestHandle_UnsetRemovesTheGlobalScope covers R10: `-g --unset`
// removes the persisted runner and starts no review.
func TestHandle_UnsetRemovesTheGlobalScope(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("NIGHTME_CONFIG", path)
	if err := os.WriteFile(path, []byte("review:\n  agent: pi\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	starter := newTestAgents(t).register("review-unset-agent")
	cs := newReviewChatSession(t, starter.name, dir)

	out := dispatchReview(t, cs, "review", "-g", "--unset")

	if !strings.Contains(out.Reply, "✅ review.agent unset (global)") {
		t.Fatalf("reply does not confirm the unset: %q", out.Reply)
	}
	if !strings.Contains(out.Reply, "→ file: "+path) {
		t.Fatalf("reply does not name the file: %q", out.Reply)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if strings.Contains(string(data), "agent: pi") {
		t.Fatalf("review.agent survived the unset:\n%s", data)
	}
	select {
	case <-starter.called:
		t.Fatal("the unset path started a review")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestHandle_UnsetOnAnEmptyConfigCreatesNothing covers the edge case
// where nothing was ever configured.
func TestHandle_UnsetOnAnEmptyConfigCreatesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("NIGHTME_CONFIG", path)

	starter := newTestAgents(t).register("review-unset-empty-agent")
	cs := newReviewChatSession(t, starter.name, dir)

	out := dispatchReview(t, cs, "review", "-g", "--unset")

	if !strings.Contains(out.Reply, "✅ review.agent unset (global)") {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("the unset created a config file, stat err = %v", err)
	}
}

// TestHandle_UnregisteredGlobalScopeFallsBackToTheSessionAgent
// covers R11: a stale name in the global scope must not brick
// /review — the session's agent runs the review and the user is
// told why.
func TestHandle_UnregisteredGlobalScopeFallsBackToTheSessionAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("NIGHTME_CONFIG", path)
	if err := os.WriteFile(path, []byte("review:\n  agent: review-stale-agent\n"), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	starter := newTestAgents(t).register("review-session-agent")
	cs := newReviewChatSession(t, starter.name, dir)

	out := dispatchReview(t, cs, "review")

	if !strings.Contains(out.Reply, `review.agent "review-stale-agent" is not a registered agent`) {
		t.Fatalf("reply does not carry the warning: %q", out.Reply)
	}
	if !strings.Contains(out.Reply, "using "+starter.name) {
		t.Fatalf("reply does not name the fallback agent: %q", out.Reply)
	}
	select {
	case <-starter.called:
	case <-time.After(2 * time.Second):
		t.Fatal("the review did not run on the session agent")
	}
}

// TestHandle_MalformedConfigFallsBackToTheSessionAgent covers the
// edge case where the global config file does not parse: the file
// is left alone, the user is told, and the session's agent runs the
// review.
func TestHandle_MalformedConfigFallsBackToTheSessionAgent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("NIGHTME_CONFIG", path)
	bad := "primary: [unclosed\n"
	if err := os.WriteFile(path, []byte(bad), 0o600); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	starter := newTestAgents(t).register("review-malformed-agent")
	cs := newReviewChatSession(t, starter.name, dir)

	out := dispatchReview(t, cs, "review")

	if !strings.Contains(out.Reply, "⚠️ cannot read review.agent from "+path) {
		t.Fatalf("reply does not report the unreadable config: %q", out.Reply)
	}
	if !strings.Contains(out.Reply, "using "+starter.name) {
		t.Fatalf("reply does not name the fallback agent: %q", out.Reply)
	}
	data, _ := os.ReadFile(path)
	if string(data) != bad {
		t.Fatalf("the malformed file was rewritten:\n%s", data)
	}
	select {
	case <-starter.called:
	case <-time.After(2 * time.Second):
		t.Fatal("the review did not run on the session agent")
	}
}

// TestHandle_FailedGlobalWriteDoesNotStopTheReview covers R12: an
// unwritable config file is reported, and the review still runs with
// the requested agent.
func TestHandle_FailedGlobalWriteDoesNotStopTheReview(t *testing.T) {
	dir := t.TempDir()
	// A regular file where a directory is expected makes every
	// write to the config path fail — no permission games needed.
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a directory"), 0o600); err != nil {
		t.Fatalf("write blocker: %v", err)
	}
	t.Setenv("NIGHTME_CONFIG", filepath.Join(blocker, "config.yaml"))

	starter := newTestAgents(t).register("review-write-fail-agent")
	cs := newReviewChatSession(t, starter.name, dir)

	out := dispatchReview(t, cs, "review", "-a", starter.name, "-g")

	if !strings.Contains(out.Reply, "⚠️ failed to save review.agent:") {
		t.Fatalf("reply does not report the failed write: %q", out.Reply)
	}
	if !strings.Contains(out.Reply, "reviewing with "+starter.name+" anyway") {
		t.Fatalf("reply does not say the review still runs: %q", out.Reply)
	}
	select {
	case <-starter.called:
	case <-time.After(2 * time.Second):
		t.Fatal("the review did not start after the failed write")
	}
}

// TestHandle_FlagCombinationsAreRejected covers R9: the rejected
// combinations report their own error and leave no trace on disk.
func TestHandle_FlagCombinationsAreRejected(t *testing.T) {
	cases := []struct {
		name    string
		argv    []string
		wantErr string
	}{
		{"unset without global", []string{"review", "--unset"}, "--unset requires --global"},
		{"unset with agent", []string{"review", "-a", "pi", "-g", "--unset"}, "--unset and --agent are mutually exclusive"},
		{"global without agent", []string{"review", "-g"}, "--global requires --agent <name>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "config.yaml")
			t.Setenv("NIGHTME_CONFIG", path)

			starter := newTestAgents(t).register("review-combo-agent")
			cs := newReviewChatSession(t, starter.name, dir)

			out := dispatchReview(t, cs, tc.argv...)

			if !strings.Contains(out.Reply, tc.wantErr) {
				t.Fatalf("reply %q does not contain %q", out.Reply, tc.wantErr)
			}
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Fatalf("the rejected command wrote a config file, stat err = %v", err)
			}
		})
	}
}

// TestHandle_GlobalScopeWithAnUnknownAgentWritesNothing covers the
// edge case where the name to persist is not a registered agent.
func TestHandle_GlobalScopeWithAnUnknownAgentWritesNothing(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	t.Setenv("NIGHTME_CONFIG", path)

	starter := newTestAgents(t).register("review-unknown-agent")
	cs := newReviewChatSession(t, starter.name, dir)

	out := dispatchReview(t, cs, "review", "-a", "review-never-registered", "-g")

	if !strings.Contains(out.Reply, `❌ unknown agent "review-never-registered"`) {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("an unknown agent was persisted, stat err = %v", err)
	}
	select {
	case <-starter.called:
		t.Fatal("the rejected command started a review")
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSpec_AcceptsNoArgs verifies the happy-path early check:
// `/review` (input.Args == ["review"], len == 1) is accepted at the
// Spec check stage. The downstream AS lookup is what distinguishes
// "no active agent" from "all good".
func TestSpec_AcceptsNoArgs(t *testing.T) {
	starter := newTestAgents(t).register("review-no-args-agent")
	cs := newReviewChatSession(t, starter.name, "")

	out := dispatchReview(t, cs, "review")

	if !strings.Contains(out.Reply, "no workspace set") {
		t.Fatalf("unexpected reply: %q", out.Reply)
	}
}

// dispatchWithNoCS invokes the factory's Handle with a nil
// ChatSession. Most paths hit the inline arg check first (no
// cs needed); paths that touch cs will panic, which the caller
// can recover from.
func dispatchWithNoCS(t *testing.T, args []string) (*command.SlashOutput, error) {
	t.Helper()
	f := NewFactory()
	return f.Handle(context.Background(), command.RuntimeServices{}, nil, nil, command.SlashInput{
		ChatID:     "test-chat",
		UserID:     "test-user",
		Text:       "/" + strings.Join(args, " "),
		MessageID:  "msg-1",
		HasMention: false,
		Args:       args,
	})
}

// dispatchReview invokes the factory's Handle against cs and
// returns the reply.
func dispatchReview(t *testing.T, cs *chatsession.ChatSession, argv ...string) *command.SlashOutput {
	t.Helper()
	f := NewFactory()
	out, err := f.Handle(context.Background(), command.RuntimeServices{}, nil, cs, command.SlashInput{
		ChatID:    "review-test-chat",
		UserID:    "test-user",
		Text:      "/" + strings.Join(argv, " "),
		MessageID: "msg-1",
		Args:      argv,
	})
	if err != nil {
		t.Fatalf("Handle returned error: %v", err)
	}
	if out == nil {
		t.Fatal("Handle returned nil SlashOutput")
	}
	return out
}

// newReviewChatSession builds a chat session whose selected agent
// is agentName. An empty dir leaves the chat without a workspace.
func newReviewChatSession(t *testing.T, agentName, dir string) *chatsession.ChatSession {
	t.Helper()
	mgr := chatsession.NewManager()
	cs, err := mgr.GetOrCreate("review-test-chat", agentName)
	if err != nil {
		t.Fatalf("GetOrCreate: %v", err)
	}
	if dir != "" {
		if err := cs.SetSelectedCwd(dir); err != nil {
			t.Fatalf("SetSelectedCwd: %v", err)
		}
	}
	return cs
}

// reviewTestStarter is a minimal agent.Starter that records the
// Review calls it receives. The bridge packages that register the
// real starters are not linked into this test binary, so
// agent.Builtins is empty here.
type reviewTestStarter struct {
	name   string
	called chan struct{}
}

func (s *reviewTestStarter) Info() agent.Info {
	return agent.NewInfo(s.name, agent.ModeACP, "", nil, nil)
}
func (s *reviewTestStarter) Detect() error { return nil }
func (s *reviewTestStarter) Init(string)   {}
func (s *reviewTestStarter) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return nil, errors.New("reviewTestStarter: Start not implemented")
}
func (s *reviewTestStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock, ...agent.RunOnceOption) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("reviewTestStarter: RunOnce not implemented")
}
func (s *reviewTestStarter) Review(context.Context, agent.StartConfig, ...agent.RunOnceOption) (agent.RunResult, error) {
	select {
	case s.called <- struct{}{}:
	default:
	}
	return agent.RunResult{Text: "reviewed"}, nil
}

// testAgents holds the fake starters a test installs.
type testAgents struct {
	registry *agent.Registry
}

// newTestAgents installs a fresh agent registry for the test and
// restores the previous one when the test ends.
//
// The registry has no Unregister, so a fake registered in place would
// stay visible to every later test in the binary. Swapping the whole
// registry is the pattern internal/command/gtw/push_test.go uses.
func newTestAgents(t *testing.T) *testAgents {
	t.Helper()
	orig := agent.Builtins
	set := &testAgents{registry: agent.New()}
	agent.Builtins = set.registry
	t.Cleanup(func() { agent.Builtins = orig })
	return set
}

// register adds a fake starter under name and returns it.
func (s *testAgents) register(name string) *reviewTestStarter {
	starter := &reviewTestStarter{name: name, called: make(chan struct{}, 1)}
	s.registry.Register(starter)
	return starter
}

// TestBuildTerminalReply_PassesThroughEnvelope checks the helper
// is a thin wrapper: ChatID / ReplyTo / formatted text pass
// through unchanged, Kind is fixed to OutReply, and stamping is
// delegated to messages.StampRunResult (covered by
// messages/stamp_test.go).
func TestBuildTerminalReply_PassesThroughEnvelope(t *testing.T) {
	const (
		chatID     = "tg_42"
		replyTo    = "msg-99"
		formatted  = "## Code review\n\nAll clean."
		runnerName = "codex"
	)
	got := buildTerminalReply(chatID, replyTo, formatted, runnerName, agent.RunResult{
		Model:     "MiniMax-M3",
		SessionID: "sess-1",
		Usage:     &agent.UsageInfo{InputTokens: 100},
	})

	if got.ChatID != chatID {
		t.Errorf("ChatID = %q, want %q", got.ChatID, chatID)
	}
	if got.ReplyTo != replyTo {
		t.Errorf("ReplyTo = %q, want %q", got.ReplyTo, replyTo)
	}
	if got.Kind != messages.OutReply {
		t.Errorf("Kind = %v, want OutReply", got.Kind)
	}
	if got.Text != formatted {
		t.Errorf("Text = %q, want %q (must pass through unchanged)", got.Text, formatted)
	}
	if got.AgentName != runnerName {
		t.Errorf("AgentName = %q, want %q", got.AgentName, runnerName)
	}
	if got.Model != "MiniMax-M3" {
		t.Errorf("Model = %q, want MiniMax-M3", got.Model)
	}
	if got.SessionID != "sess-1" {
		t.Errorf("SessionID = %q, want sess-1", got.SessionID)
	}
	if got.Usage == nil || got.Usage.InputTokens != 100 {
		t.Errorf("Usage not stamped: %+v", got.Usage)
	}
}
