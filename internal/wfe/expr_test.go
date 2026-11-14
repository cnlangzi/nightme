package wfe

import (
	"testing"
)

func TestEvalString_NoExpression(t *testing.T) {
	got := EvalString("hello world", ExprCtx{})
	if got != "hello world" {
		t.Errorf("got %q, want %q", got, "hello world")
	}
}

func TestEvalString_Env(t *testing.T) {
	ec := ExprCtx{Env: map[string]string{"TOKEN": "abc123"}}
	got := EvalString("token=${{ env.TOKEN }}", ec)
	if got != "token=abc123" {
		t.Errorf("got %q, want %q", got, "token=abc123")
	}
}

func TestEvalString_Event(t *testing.T) {
	ec := ExprCtx{Event: map[string]any{
		"pr": map[string]any{"number": 42, "title": "fix bug"},
	}}
	got := EvalString("PR #${{ event.pr.number }}: ${{ event.pr.title }}", ec)
	if got != "PR #42: fix bug" {
		t.Errorf("got %q", got)
	}
}

func TestEvalString_Steps(t *testing.T) {
	ec := ExprCtx{Steps: map[string]map[string]string{
		"ai": {"verdict": "needs-fix"},
	}}
	got := EvalString("verdict=${{ steps.ai.verdict }}", ec)
	if got != "verdict=needs-fix" {
		t.Errorf("got %q", got)
	}
}

func TestEvalString_StepOutputs(t *testing.T) {
	ec := ExprCtx{Steps: map[string]map[string]string{
		"fix": {
			"status":      "ok",
			"summary":     "picked an issue",
			"data.issue":  "42",
			"data.branch": "nightme/issue-42",
		},
	}}
	tests := []struct {
		expr string
		want string
	}{
		{"${{ steps.fix.outputs.status }}", "ok"},
		{"${{ steps.fix.outputs.summary }}", "picked an issue"},
		{"${{ steps.fix.outputs.data.issue }}", "42"},
		{"${{ steps.fix.status }}", "ok"},
		{"${{ steps.fix.data.branch }}", "nightme/issue-42"},
		{"${{ steps.fix.outputs.issue }}", "42"},
		{"${{ steps.fix.outputs.data.missing }}", ""},
	}
	for _, tt := range tests {
		if got := EvalString(tt.expr, ec); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.expr, got, tt.want)
		}
	}
}

func TestEvalString_NeedsOutputs(t *testing.T) {
	ec := ExprCtx{Needs: map[string]map[string]string{
		"ship": {"status": "ok", "data.pr": "7"},
	}}
	if got := EvalString("${{ needs.ship.outputs.status }}", ec); got != "ok" {
		t.Errorf("status = %q, want ok", got)
	}
	if got := EvalString("${{ needs.ship.outputs.data.pr }}", ec); got != "7" {
		t.Errorf("data.pr = %q, want 7", got)
	}
}

func TestEvalString_FunctionSuccess(t *testing.T) {
	ec := ExprCtx{}
	got := EvalString("${{ success() }}", ec)
	if got != "true" {
		t.Errorf("got %q, want true", got)
	}
}

func TestEvalString_FunctionFailure(t *testing.T) {
	got := EvalString("${{ failure() }}", ExprCtx{})
	if got != "false" {
		t.Errorf("got %q, want false", got)
	}
}

func TestEvalString_UnknownIdentifier(t *testing.T) {
	got := EvalString("${{ bogus.x }}", ExprCtx{})
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestEvalString_MultipleExpressions(t *testing.T) {
	ec := ExprCtx{Env: map[string]string{"A": "x", "B": "y"}}
	got := EvalString("a=${{ env.A }} b=${{ env.B }}", ec)
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
	got := EvalMap(m, ec)
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
	ok, err := EvalCond("", ExprCtx{})
	if err != nil || !ok {
		t.Errorf("empty cond should be true, got %v err=%v", ok, err)
	}
}

func TestEvalCond_True(t *testing.T) {
	ok, _ := EvalCond("${{ success() }}", ExprCtx{})
	if !ok {
		t.Error("success() should be true")
	}
}

func TestEvalCond_False(t *testing.T) {
	ok, _ := EvalCond("${{ failure() }}", ExprCtx{})
	if ok {
		t.Error("failure() should be false")
	}
}
