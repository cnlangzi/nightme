package runtime

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/llm"
)

// TestBuildPersonalAgent_NoChannel — the personal agent works in a
// chat. Without a channel there is no chat to work in, so the agent
// stays off.
func TestBuildPersonalAgent_NoChannel(t *testing.T) {
	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	svc := buildPersonalAgent(cfg, nil, nil, slog.Default())
	if svc != nil {
		t.Fatal("expected no agent without a channel")
	}
}

// TestBuildPersonalAgent_StartsWithAChannel — the agent starts once
// the daemon has a channel to work in.
func TestBuildPersonalAgent_StartsWithAChannel(t *testing.T) {
	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	ch := newStubChannel("stub", nil)
	op := newOperator(nil, []channel.Channel{ch}, slog.Default())
	svc := buildPersonalAgent(cfg, []channel.Channel{ch}, op, slog.Default())
	if svc == nil {
		t.Fatal("expected an agent")
	}
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}
}

// init registers a channel the operator tests can resolve. The
// operator finds the channel of a chat by the namespace its chat id
// carries.
func init() {
	channel.Register("me_stub", "ms_", func(*config.Config) (channel.Channel, error) {
		return newStubChannel("me_stub", nil), nil
	})
}

// TestOperator_ResolvesTheChannelOfTheChat — the chat id names the
// channel that owns it.
func TestOperator_ResolvesTheChannelOfTheChat(t *testing.T) {
	ch := newStubChannel("me_stub", nil)
	op := newOperator(nil, []channel.Channel{ch}, slog.Default())

	got, err := op.channelFor("ms_test")
	if err != nil {
		t.Fatalf("channelFor: %v", err)
	}
	if got.Name() != "me_stub" {
		t.Fatalf("channel = %q, want me_stub", got.Name())
	}
	if _, err := op.channelFor("tg_test"); err == nil {
		t.Fatal("a chat of another channel was resolved")
	}
}

// TestOperator_RefusesAChannelThatCannotBeDriven — a channel that
// does not speak for a program cannot host a run, and the refusal
// says so.
func TestOperator_RefusesAChannelThatCannotBeDriven(t *testing.T) {
	ch := newStubChannel("me_stub", nil)
	op := newOperator(nil, []channel.Channel{ch}, slog.Default())

	_, err := op.impersonatorFor("ms_test")
	if err == nil {
		t.Fatal("the channel was accepted")
	}
	if !strings.Contains(err.Error(), "cannot be driven") {
		t.Fatalf("error = %v, want it to say the channel cannot be driven", err)
	}
}

// TestBuildChatModel_UsesTheInlineKey — the plaintext key of the
// chat model reaches the provider as its credential.
func TestBuildChatModel_UsesTheInlineKey(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write([]byte(`{"choices":[]}`))
	}))
	defer srv.Close()

	cfg := &config.Config{Me: config.MeConfig{LLM: config.MeLLMConfig{
		Protocol: "chat_completions", BaseURL: srv.URL, Model: "m", APIKey: "sk-inline",
	}}}
	p, issue := buildChatModel(cfg, slog.Default())
	if p == nil {
		t.Fatal("no chat model was built")
	}
	if issue != "" {
		t.Fatalf("issue = %q, want none", issue)
	}
	if _, err := p.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if auth != "Bearer sk-inline" {
		t.Fatalf("authorization = %q, want the inline key", auth)
	}
}

// TestBuildDecider_UsesTheInlineKey — the plaintext key of the
// decision model reaches the decider as its credential.
func TestBuildDecider_UsesTheInlineKey(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	cfg := &config.Config{Me: config.MeConfig{Decider: config.MeDeciderConfig{
		Protocol: "decisions", BaseURL: srv.URL, Model: "m", APIKey: "sk-inline",
	}}}
	d, issue := buildDecider(cfg, slog.Default())
	if d == nil {
		t.Fatal("no decision model was built")
	}
	if issue != "" {
		t.Fatalf("issue = %q, want none", issue)
	}
	if _, err := d.Decide(context.Background(), llm.DecisionRequest{
		State:     "halfway",
		Questions: []llm.Question{{Name: "next", Type: llm.QuestionYesNo}},
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if auth != "Bearer sk-inline" {
		t.Fatalf("authorization = %q, want the inline key", auth)
	}
}

// TestBuildChatModel_WarnsOnBothChannels — a configuration that names
// the key twice is reported, and the report carries no credential.
func TestBuildChatModel_WarnsOnBothChannels(t *testing.T) {
	t.Setenv("NIGHTME_TEST_KEY", "sk-env-value")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := &config.Config{Me: config.MeConfig{LLM: config.MeLLMConfig{
		Protocol: "chat_completions", BaseURL: "https://example.test", Model: "m",
		APIKey: "sk-inline", APIKeyEnv: "NIGHTME_TEST_KEY",
	}}}
	buildChatModel(cfg, logger)

	out := buf.String()
	if !strings.Contains(out, "api_key_env") || !strings.Contains(out, "api_key wins") {
		t.Fatalf("the warning does not name the two keys and the winner: %s", out)
	}
	if strings.Contains(out, "sk-inline") || strings.Contains(out, "sk-env-value") {
		t.Fatalf("the warning carries a credential: %s", out)
	}
}

// TestBuildChatModel_NoWarningOnOneChannel — a single channel is not
// ambiguous and stays quiet.
func TestBuildChatModel_NoWarningOnOneChannel(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := &config.Config{Me: config.MeConfig{LLM: config.MeLLMConfig{
		Protocol: "chat_completions", BaseURL: "https://example.test", Model: "m",
		APIKey: "sk-inline",
	}}}
	buildChatModel(cfg, logger)

	if out := buf.String(); strings.Contains(out, "api_key") {
		t.Fatalf("the log mentions the credential: %s", out)
	}
}

// TestBuildChatModel_NoWarningWithoutAProtocol — a capability that is
// not configured is not built, so its credentials are nobody's
// business.
func TestBuildChatModel_NoWarningWithoutAProtocol(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := &config.Config{Me: config.MeConfig{LLM: config.MeLLMConfig{
		APIKey: "sk-inline", APIKeyEnv: "NIGHTME_TEST_KEY",
	}}}
	p, issue := buildChatModel(cfg, logger)
	if p != nil {
		t.Fatal("a chat model without a protocol was built")
	}
	if issue != "" {
		t.Fatalf("issue = %q, want none", issue)
	}
	if out := buf.String(); out != "" {
		t.Fatalf("the log is not empty: %s", out)
	}
}

func TestWorkflowsDir(t *testing.T) {
	dir, err := WorkflowsDir(&config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}})
	if err != nil {
		t.Fatalf("workflowsDir: %v", err)
	}
	if filepath.Base(dir) != "workflows" {
		t.Errorf("dir = %q, want it to end in %q", dir, "workflows")
	}
	if _, err := WorkflowsDir(&config.Config{}); err == nil {
		t.Error("expected an error for an empty data dir")
	}
}

// TestCountIMChannels — the count says how many channels a person can
// talk to.
func TestCountIMChannels(t *testing.T) {
	if got := countIMChannels(nil); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	if got := countIMChannels([]channel.Channel{newStubChannel("stub", nil)}); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
	if got := countIMChannels([]channel.Channel{newStubChannel("a", nil), newStubChannel("b", nil)}); got != 2 {
		t.Errorf("count = %d, want 2", got)
	}
}

// TestBuildChatModel_ReportsAMissingProtocol — a chat model that
// names no protocol is not built, and the issue says which key is
// missing.
//
// The key that names the protocol is not read under its old name any
// more: a file that carries only that one has no chat model, and the
// operator learns it from the issue.
func TestBuildChatModel_ReportsAMissingProtocol(t *testing.T) {
	cfg := &config.Config{Me: config.MeConfig{LLM: config.MeLLMConfig{
		BaseURL: "https://example.test", Model: "m",
	}}}
	p, issue := buildChatModel(cfg, slog.Default())
	if p != nil {
		t.Fatal("a chat model without a protocol was built")
	}
	want := "me.llm.protocol is not set (known: chat_completions, messages)"
	if issue != want {
		t.Fatalf("issue = %q, want %q", issue, want)
	}
}

// TestBuildChatModel_ReportsAMissingModel — a chat model that names
// no model is not built, and the issue says which key is missing.
func TestBuildChatModel_ReportsAMissingModel(t *testing.T) {
	cfg := &config.Config{Me: config.MeConfig{LLM: config.MeLLMConfig{
		Protocol: "chat_completions", BaseURL: "https://example.test",
	}}}
	p, issue := buildChatModel(cfg, slog.Default())
	if p != nil {
		t.Fatal("a chat model without a model was built")
	}
	if issue != "me.llm.model is not set" {
		t.Fatalf("issue = %q, want the missing model", issue)
	}
}

// TestBuildChatModel_ReportsAnUnknownProtocol — a chat model that
// names a protocol nobody registered is not built, and the issue says
// what went wrong.
func TestBuildChatModel_ReportsAnUnknownProtocol(t *testing.T) {
	cfg := &config.Config{Me: config.MeConfig{LLM: config.MeLLMConfig{
		Protocol: "nope", BaseURL: "https://example.test", Model: "m",
	}}}
	p, issue := buildChatModel(cfg, slog.Default())
	if p != nil {
		t.Fatal("a chat model of an unknown protocol was built")
	}
	want := `the chat model could not be built: llm: unknown chat protocol "nope" (known: chat_completions, messages)`
	if issue != want {
		t.Fatalf("issue = %q, want %q", issue, want)
	}
}

// TestBuildDecider_ReportsAMissingProtocol — a decision model that
// names no protocol is not built, and the issue says which key is
// missing.
func TestBuildDecider_ReportsAMissingProtocol(t *testing.T) {
	cfg := &config.Config{Me: config.MeConfig{Decider: config.MeDeciderConfig{
		BaseURL: "https://example.test", Model: "m",
	}}}
	d, issue := buildDecider(cfg, slog.Default())
	if d != nil {
		t.Fatal("a decision model without a protocol was built")
	}
	want := "me.decider.protocol is not set (known: decisions, systemone)"
	if issue != want {
		t.Fatalf("issue = %q, want %q", issue, want)
	}
}

// TestBuildDecider_ReportsAMissingModel — a decision model that names
// no model is not built, and the issue says which key is missing.
func TestBuildDecider_ReportsAMissingModel(t *testing.T) {
	cfg := &config.Config{Me: config.MeConfig{Decider: config.MeDeciderConfig{
		Protocol: "decisions", BaseURL: "https://example.test",
	}}}
	d, issue := buildDecider(cfg, slog.Default())
	if d != nil {
		t.Fatal("a decision model without a model was built")
	}
	if issue != "me.decider.model is not set" {
		t.Fatalf("issue = %q, want the missing model", issue)
	}
}

// TestBuildDecider_ReportsAnUnknownProtocol — a decision model that
// names a protocol nobody registered is not built, and the issue says
// what went wrong.
func TestBuildDecider_ReportsAnUnknownProtocol(t *testing.T) {
	cfg := &config.Config{Me: config.MeConfig{Decider: config.MeDeciderConfig{
		Protocol: "nope", BaseURL: "https://example.test", Model: "m",
	}}}
	d, issue := buildDecider(cfg, slog.Default())
	if d != nil {
		t.Fatal("a decision model of an unknown protocol was built")
	}
	want := `the decision model could not be built: llm: unknown decision protocol "nope" (known: decisions, systemone)`
	if issue != want {
		t.Fatalf("issue = %q, want %q", issue, want)
	}
}

// TestBuildPersonalAgent_LogsABrokenModelAsAnError — a model that
// is configured but cannot be built is an error the daemon reports.
func TestBuildPersonalAgent_LogsABrokenModelAsAnError(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := &config.Config{
		Paths: config.PathsConfig{DataDir: t.TempDir()},
		Me: config.MeConfig{LLM: config.MeLLMConfig{
			BaseURL: "https://example.test", Model: "m",
		}},
	}
	ch := newStubChannel("stub", nil)
	op := newOperator(nil, []channel.Channel{ch}, slog.Default())
	svc := buildPersonalAgent(cfg, []channel.Channel{ch}, op, logger)
	if svc == nil {
		t.Fatal("expected an agent")
	}
	out := buf.String()
	if !strings.Contains(out, "level=ERROR") {
		t.Fatalf("the broken model was not logged as an error: %s", out)
	}
	if !strings.Contains(out, "me.llm.protocol is not set") {
		t.Fatalf("the log does not name the missing protocol: %s", out)
	}
}

// TestBuildPersonalAgent_LogsNothingConfiguredAsInfo — no model at
// all is a state, not an error.
func TestBuildPersonalAgent_LogsNothingConfiguredAsInfo(t *testing.T) {
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	ch := newStubChannel("stub", nil)
	op := newOperator(nil, []channel.Channel{ch}, slog.Default())
	svc := buildPersonalAgent(cfg, []channel.Channel{ch}, op, logger)
	if svc == nil {
		t.Fatal("expected an agent")
	}
	out := buf.String()
	if !strings.Contains(out, "level=INFO") {
		t.Fatalf("the missing models were not logged as info: %s", out)
	}
	if !strings.Contains(out, "neither me.llm nor me.decider is configured") {
		t.Fatalf("the log does not say that no model is configured: %s", out)
	}
	if strings.Contains(out, "level=ERROR") {
		t.Fatalf("a missing model was logged as an error: %s", out)
	}
}

// TestPersonalAgentConfig_CarriesTheIssues — the issues reach the
// personal agent, so it can tell the operator what is broken.
func TestPersonalAgentConfig_CarriesTheIssues(t *testing.T) {
	issues := []string{"me.llm.protocol is not set (known: chat_completions, messages)"}
	cfg := &config.Config{Me: config.MeConfig{Agent: "codex"}}
	got := personalAgentConfig(cfg, t.TempDir(), nil, nil, nil, issues)
	if len(got.ModelIssues) != 1 || got.ModelIssues[0] != issues[0] {
		t.Fatalf("ModelIssues = %v, want %v", got.ModelIssues, issues)
	}
}

// TestPersonalAgentStartLine_SaysWhetherTheAgentHasModels — the
// startup log says whether the agent has models, and why not when it
// has none.
func TestPersonalAgentStartLine_SaysWhetherTheAgentHasModels(t *testing.T) {
	if got := personalAgentStartLine(""); got != "run: personal agent started" {
		t.Fatalf("line = %q, want the agent that started", got)
	}
	want := "run: the personal agent has no models: me.llm.protocol is not set (known: chat_completions, messages)"
	if got := personalAgentStartLine("me.llm.protocol is not set (known: chat_completions, messages)"); got != want {
		t.Fatalf("line = %q, want %q", got, want)
	}
}

// TestPersonalAgentConfig_UsesTheMeAgent — the agent that thinks for
// the owner comes from the me section, not from the chat model.
func TestPersonalAgentConfig_UsesTheMeAgent(t *testing.T) {
	cfg := &config.Config{Me: config.MeConfig{Agent: "codex"}}
	got := personalAgentConfig(cfg, t.TempDir(), nil, nil, nil, nil)
	if got.Agent != "codex" {
		t.Fatalf("Agent = %q, want codex", got.Agent)
	}
}

// TestBuildDecider_WarnsOnBothChannels — a decision model that names
// the key twice is reported, and the report carries no credential.
func TestBuildDecider_WarnsOnBothChannels(t *testing.T) {
	t.Setenv("NIGHTME_TEST_KEY", "sk-env-value")
	var buf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&buf, nil))

	cfg := &config.Config{Me: config.MeConfig{Decider: config.MeDeciderConfig{
		Protocol: "decisions", BaseURL: "https://example.test", Model: "m",
		APIKey: "sk-inline", APIKeyEnv: "NIGHTME_TEST_KEY",
	}}}
	buildDecider(cfg, logger)

	out := buf.String()
	if !strings.Contains(out, "api_key_env") || !strings.Contains(out, "api_key wins") {
		t.Fatalf("the warning does not name the two keys and the winner: %s", out)
	}
	if strings.Contains(out, "sk-inline") || strings.Contains(out, "sk-env-value") {
		t.Fatalf("the warning carries a credential: %s", out)
	}
}

// TestBuildModels_ReachTheEndpointsOfTheirProtocols — the daemon
// builds both models from the configuration and reaches the endpoints
// their protocols name.
func TestBuildModels_ReachTheEndpointsOfTheirProtocols(t *testing.T) {
	var chatPath, deciderPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/chat/completions":
			chatPath = r.URL.Path
			w.Write([]byte(`{"choices":[]}`))
		case "/decisions":
			deciderPath = r.URL.Path
			w.Write([]byte(`{"answers":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	cfg := &config.Config{Me: config.MeConfig{
		LLM: config.MeLLMConfig{
			Protocol: "chat_completions", BaseURL: srv.URL, Model: "m",
		},
		Decider: config.MeDeciderConfig{
			Protocol: "decisions", BaseURL: srv.URL, Model: "m",
		},
	}}

	p, _ := buildChatModel(cfg, slog.Default())
	if p == nil {
		t.Fatal("no chat model was built")
	}
	if _, err := p.Complete(context.Background(), llm.Request{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	d, _ := buildDecider(cfg, slog.Default())
	if d == nil {
		t.Fatal("no decision model was built")
	}
	if _, err := d.Decide(context.Background(), llm.DecisionRequest{
		State:     "halfway",
		Questions: []llm.Question{{Name: "next", Type: llm.QuestionYesNo}},
	}); err != nil {
		t.Fatalf("decide: %v", err)
	}

	if chatPath != "/chat/completions" {
		t.Errorf("the chat model called %q, want /chat/completions", chatPath)
	}
	if deciderPath != "/decisions" {
		t.Errorf("the decision model called %q, want /decisions", deciderPath)
	}
}
