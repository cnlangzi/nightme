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
	"github.com/cnlangzi/nightme/internal/channel/loopback"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/llm"
)

// TestBuildPersonalAgent_NoLoopbackChannel — the personal agent
// drives the daemon through the loopback channel. Without that
// channel there is no chat to drive it through, so the agent stays
// off.
func TestBuildPersonalAgent_NoLoopbackChannel(t *testing.T) {
	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	svc := buildPersonalAgent(cfg, nil, slog.Default())
	if svc != nil {
		t.Fatal("expected no agent without a loopback channel")
	}
}

// TestBuildPersonalAgent_OtherChannelOnly — a daemon whose only
// channel is not the loopback one runs without the agent.
func TestBuildPersonalAgent_OtherChannelOnly(t *testing.T) {
	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	svc := buildPersonalAgent(cfg, []channel.Channel{newStubChannel("stub", nil)}, slog.Default())
	if svc != nil {
		t.Fatal("expected no agent without a loopback channel")
	}
}

// TestBuildPersonalAgent_StartsWithLoopbackChannel — the agent
// starts once the loopback channel is up.
func TestBuildPersonalAgent_StartsWithLoopbackChannel(t *testing.T) {
	cfg := &config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}}
	ch := loopback.New()
	svc := buildPersonalAgent(cfg, []channel.Channel{ch}, slog.Default())
	if svc == nil {
		t.Fatal("expected an agent")
	}
	if err := svc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
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
		Provider: "openai", BaseURL: srv.URL, Model: "m", APIKey: "sk-inline",
	}}}
	p := buildChatModel(cfg, slog.Default())
	if p == nil {
		t.Fatal("no chat model was built")
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
	d := buildDecider(cfg, slog.Default())
	if d == nil {
		t.Fatal("no decision model was built")
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
		Provider: "openai", BaseURL: "https://example.test", Model: "m",
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
		Provider: "openai", BaseURL: "https://example.test", Model: "m",
		APIKey: "sk-inline",
	}}}
	buildChatModel(cfg, logger)

	if out := buf.String(); strings.Contains(out, "api_key") {
		t.Fatalf("the log mentions the credential: %s", out)
	}
}

func TestWorkflowsDir(t *testing.T) {
	dir, err := workflowsDir(&config.Config{Paths: config.PathsConfig{DataDir: t.TempDir()}})
	if err != nil {
		t.Fatalf("workflowsDir: %v", err)
	}
	if filepath.Base(dir) != "workflows" {
		t.Errorf("dir = %q, want it to end in %q", dir, "workflows")
	}
	if _, err := workflowsDir(&config.Config{}); err == nil {
		t.Error("expected an error for an empty data dir")
	}
}

// TestCountIMChannels — the loopback channel is the daemon's own
// chat: it must not hide the fact that no IM channel is configured.
func TestCountIMChannels(t *testing.T) {
	if got := countIMChannels(nil); got != 0 {
		t.Errorf("count = %d, want 0", got)
	}
	if got := countIMChannels([]channel.Channel{loopback.New()}); got != 0 {
		t.Errorf("count = %d, want 0 — the loopback channel is not an IM channel", got)
	}
	if got := countIMChannels([]channel.Channel{loopback.New(), newStubChannel("stub", nil)}); got != 1 {
		t.Errorf("count = %d, want 1", got)
	}
}
