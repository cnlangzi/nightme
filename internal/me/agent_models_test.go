package me

import (
	"context"
	"testing"

	"github.com/cnlangzi/nightme/internal/llm"
)

// TestModelsReason_OneModelIsEnough — one model is all the agent
// needs: it has something to decide with.
func TestModelsReason_OneModelIsEnough(t *testing.T) {
	svc := New(Config{Provider: &scriptedProvider{}})
	if reason := svc.ModelsReason(); reason != "" {
		t.Fatalf("ModelsReason = %q, want it empty", reason)
	}
	svc = New(Config{Decider: &scriptedDecider{}})
	if reason := svc.ModelsReason(); reason != "" {
		t.Fatalf("ModelsReason = %q, want it empty", reason)
	}
}

// TestModelsReason_TheIssuesSayWhichModelIsBroken — an agent with no
// model says which key of which one is broken.
func TestModelsReason_TheIssuesSayWhichModelIsBroken(t *testing.T) {
	svc := New(Config{ModelIssues: []string{
		"me.llm.protocol is not set (known: chat_completions, messages)",
		"me.decider.model is not set",
	}})
	want := "me.llm.protocol is not set (known: chat_completions, messages); me.decider.model is not set"
	if reason := svc.ModelsReason(); reason != want {
		t.Fatalf("ModelsReason = %q, want %q", reason, want)
	}
}

// TestModelsReason_WithoutAnIssueTheFallbackStands — nothing is
// configured and nothing is broken: the reason says so.
func TestModelsReason_WithoutAnIssueTheFallbackStands(t *testing.T) {
	svc := New(Config{})
	want := "neither me.llm nor me.decider is configured"
	if reason := svc.ModelsReason(); reason != want {
		t.Fatalf("ModelsReason = %q, want %q", reason, want)
	}
}

// TestDisabledReason_SaysWhichKeyIsBroken — the reason the agent is
// off names the key that is broken.
func TestDisabledReason_SaysWhichKeyIsBroken(t *testing.T) {
	svc := New(Config{ModelIssues: []string{"me.llm.protocol is not set (known: chat_completions, messages)"}})
	want := "me.llm.protocol is not set (known: chat_completions, messages)"
	if reason := svc.DisabledReason(); reason != want {
		t.Fatalf("DisabledReason = %q, want %q", reason, want)
	}
}

// TestDecide_RefusesWhenThereIsNoDecisionModel — the run asks nothing
// of a model that is not there.
func TestDecide_RefusesWhenThereIsNoDecisionModel(t *testing.T) {
	svc := New(Config{})
	_, err := svc.decide(context.Background(), &run{})
	if err == nil {
		t.Fatal("the decision was made without a model")
	}
	if err.Error() != "the decision model is not configured" {
		t.Fatalf("error = %q, want the missing model", err)
	}
}

// TestAskModel_RefusesWhenThereIsNoChatModel — the run asks nothing of
// a model that is not there.
func TestAskModel_RefusesWhenThereIsNoChatModel(t *testing.T) {
	svc := New(Config{})
	_, err := svc.askModel(context.Background(), &run{}, llm.Request{})
	if err == nil {
		t.Fatal("the chat model was asked without a model")
	}
	if err.Error() != "the chat model is not configured" {
		t.Fatalf("error = %q, want the missing model", err)
	}
}
