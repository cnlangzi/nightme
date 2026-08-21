package wfe

import (
	"context"
	"testing"
	"time"
)

// fakeRT is a no-op Runtime for testing expression evaluation.
// RunShell/SendPrompt/RunAction are not called by expr tests.
type fakeRT struct{ now time.Time }

func (f fakeRT) RunShell(_ context.Context, _ ShellSpec) (*ShellResult, error) {
	return nil, nil
}
func (f fakeRT) SendPrompt(_ context.Context, _ PromptSpec) (*Reply, error) {
	return nil, nil
}
func (f fakeRT) RunAction(_ context.Context, _ ActionSpec) (*ActionResult, error) {
	return nil, nil
}
func (f fakeRT) Now() time.Time { return f.now }

func TestEvalString_NoExpression(t *testing.T) {
	got := EvalString("hello world", ExprCtx{}, fakeRT{now: time.Now()})
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestEvalString_Env(t *testing.T) {
	ec := ExprCtx{Env: map[string]string{"TOKEN": "abc123"}}
	got := EvalString("token=${{ env.TOKEN }}", ec, fakeRT{})
	if got != "token=abc123" {
		t.Errorf("got %q, want %q", got, "token=abc123")
	}
}

func TestEvalString_Event(t *testing.T) {
	ec := ExprCtx{Event: map[string]any{
		"pr": map[string]any{"number": 42, "title": "fix bug"},
	}}
	got := EvalString("PR #${{ event.pr.number }}: ${{ event.pr.title }}", ec, fakeRT{})
	if got != "PR #42: fix bug" {
		t.Errorf("got %q", got)
	}
}

func TestEvalString_Steps(t *testing.T) {
	ec := ExprCtx{Steps: map[string]map[string]string{
		"ai": {"verdict": "needs-fix"},
	}}
	got := EvalString("verdict=${{ steps.ai.verdict }}", ec, fakeRT{})
	if got != "verdict=needs-fix" {
		t.Errorf("got %q", got)
	}
}

func TestEvalString_FunctionSuccess(t *testing.T) {
	ec := ExprCtx{}
	got := EvalString("${{ success() }}", ec, fakeRT{})
	if got != "true" {
		t.Errorf("got %q, want true", got)
	}
}

func TestEvalString_FunctionFailure(t *testing.T) {
	got := EvalString("${{ failure() }}", ExprCtx{}, fakeRT{})
	if got != "false" {
		t.Errorf("got %q, want false", got)
	}
}

func TestEvalString_UnknownIdentifier(t *testing.T) {
	got := EvalString("${{ bogus.x }}", ExprCtx{}, fakeRT{})
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestEvalString_MultipleExpressions(t *testing.T) {
	ec := ExprCtx{Env: map[string]string{"A": "x", "B": "y"}}
	got := EvalString("a=${{ env.A }} b=${{ env.B }}", ec, fakeRT{})
	if got != "a=x b=y" {
		t.Errorf("got %q", got)
	}
}

func TestEvalMap(t *testing.T) {
	ec := ExprCtx{Env: map[string]string{"USER": "alice"}}
	m := map[string]any{
		"channel": "feishu",
		"target":  "${{ env.USER }}",
		"nested":  map[string]any{"key": "${{ env.USER }}"},
		"count":   3,
	}
	got := EvalMap(m, ec, fakeRT{})
	if got["target"] != "alice" {
		t.Errorf("target = %v", got["target"])
	}
	if got["channel"] != "feishu" {
		t.Errorf("channel = %v", got["channel"])
	}
	if got["count"] != 3 {
		t.Errorf("count = %v (should be unchanged)", got["count"])
	}
	nested, ok := got["nested"].(map[string]any)
	if !ok || nested["key"] != "alice" {
		t.Errorf("nested.key = %v", got["nested"])
	}
}

func TestEvalCond_Empty(t *testing.T) {
	ok, err := EvalCond("", ExprCtx{}, fakeRT{})
	if err != nil || !ok {
		t.Errorf("empty cond should be true, got %v err=%v", ok, err)
	}
}

func TestEvalCond_True(t *testing.T) {
	ok, _ := EvalCond("${{ success() }}", ExprCtx{}, fakeRT{})
	if !ok {
		t.Error("success() should be true")
	}
}

func TestEvalCond_False(t *testing.T) {
	ok, _ := EvalCond("${{ failure() }}", ExprCtx{}, fakeRT{})
	if ok {
		t.Error("failure() should be false")
	}
}
