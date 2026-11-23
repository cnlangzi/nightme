package wfe

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/pkg/outcome"
)

// exprCtx builds the context the expression tests read: an outcome of
// a rate limited step, an argument, an environment and an event.
func exprCtx() ExprCtx {
	return ExprCtx{
		Steps: map[string]map[string]string{
			"fix": {
				"success":     "false",
				"status":      "429",
				"statusText":  "the agent is rate limited",
				"data.issue":  "42",
				"data.branch": "nightme/issue-42",
			},
		},
		Needs: map[string]map[string]string{
			"ship": {"status": "200", "data.pr": "7"},
		},
		Env:  map[string]string{"TOKEN": "abc123"},
		Args: map[string]string{"issue": "42"},
		Event: map[string]any{
			"pr": map[string]any{"number": 42, "title": "fix bug"},
		},
		Now:  time.Date(2026, 11, 24, 10, 30, 0, 0, time.UTC),
		Last: map[string]string{"success": "false", "status": "429"},
	}
}

// TestEvalCond_ReadsTheOutcome — a condition compares what the outcome
// of a step holds.
func TestEvalCond_ReadsTheOutcome(t *testing.T) {
	ec := exprCtx()
	tests := []struct {
		cond string
		want bool
	}{
		{"steps.fix.outputs.status == 429", true},
		{"steps.fix.outputs.status == '429'", true},
		{"steps.fix.outputs.status != 429", false},
		{"steps.fix.outputs.status > 400", true},
		{"steps.fix.outputs.status >= 429", true},
		{"steps.fix.outputs.status < 500", true},
		{"steps.fix.outputs.status <= 429", true},
		{"steps.fix.outputs.success", false},
		{"!steps.fix.outputs.success", true},
		{"steps.fix.outputs.success == false", true},
		{"steps.fix.outputs.statusText == 'the agent is rate limited'", true},
		{"steps.fix.outputs.data.issue == 42", true},
		{"steps.fix.outputs.data.issue == '42'", true},
		{"steps.fix.outputs.issue == 42", true},
		{"steps.fix.data.issue == 42", true},
		{"needs.ship.outputs.status == 200", true},
		{"needs.ship.outputs.data.pr == 7", true},
		{"env.TOKEN == 'abc123'", true},
		{"args.issue == 42", true},
		{"event.pr.number == 42", true},
		{"event.pr.title == 'fix bug'", true},
		{"now == '2026-11-24T10:30:00Z'", true},
	}
	for _, tt := range tests {
		got, err := EvalCond(tt.cond, ec)
		if err != nil {
			t.Errorf("%s: %v", tt.cond, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s = %v, want %v", tt.cond, got, tt.want)
		}
	}
}

// TestEvalCond_ComparesByConversion — the values compare themselves:
// a string that reads as a number is a number, and a comparison that
// cannot be made is false.
func TestEvalCond_ComparesByConversion(t *testing.T) {
	tests := []struct {
		cond string
		want bool
	}{
		{"'200' == 200", true},
		{"'true' == true", true},
		{"'false' == false", true},
		{"null == 0", false},
		{"null == ''", false},
		{"null == null", true},
		{"null == false", true},
		{"null != 0", true},
		{"'1000' > '400'", true},
		{"'zebra' > 'm'", true},
		{"'abc' > 3", false},
		{"'abc' == 'abc'", true},
		{"'abc' == 'abd'", false},
		{"2 == 2.0", true},
		{"'x' == 'x' && 1 < 2", true},
	}
	for _, tt := range tests {
		got, err := EvalCond(tt.cond, ExprCtx{})
		if err != nil {
			t.Errorf("%s: %v", tt.cond, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s = %v, want %v", tt.cond, got, tt.want)
		}
	}
}

// TestEvalCond_ReadsTheTruthOfAValue — a condition holds when the
// value it comes to is true.
func TestEvalCond_ReadsTheTruthOfAValue(t *testing.T) {
	tests := []struct {
		cond string
		want bool
	}{
		{"", true},
		{"null", false},
		{"true", true},
		{"false", false},
		{"0", false},
		{"1", true},
		{"''", false},
		{"'false'", false},
		{"'0'", false},
		{"'x'", true},
		{"steps.missing.outputs.status", false},
		{"steps.fix.outputs.data.missing", false},
	}
	for _, tt := range tests {
		got, err := EvalCond(tt.cond, exprCtx())
		if err != nil {
			t.Errorf("%s: %v", tt.cond, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s = %v, want %v", tt.cond, got, tt.want)
		}
	}
}

// TestEvalCond_ReadsTheFunctions — the three functions of the language.
func TestEvalCond_ReadsTheFunctions(t *testing.T) {
	ec := exprCtx()
	tests := []struct {
		cond string
		want bool
	}{
		{"success()", false},
		{"!success()", true},
		{"always()", true},
		{"empty(steps.fix.outputs.data.missing)", true},
		{"empty(steps.fix.outputs.data.issue)", false},
		{"empty(steps.missing.outputs.status)", true},
		{"empty('')", true},
		{"empty('  ')", true},
		{"empty(0)", false},
	}
	for _, tt := range tests {
		got, err := EvalCond(tt.cond, ec)
		if err != nil {
			t.Errorf("%s: %v", tt.cond, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s = %v, want %v", tt.cond, got, tt.want)
		}
	}
}

// TestEvalCond_SuccessIsTrueWhenNoStepRan — a run that ran no step yet
// has nothing that broke.
func TestEvalCond_SuccessIsTrueWhenNoStepRan(t *testing.T) {
	got, err := EvalCond("success()", ExprCtx{})
	if err != nil {
		t.Fatalf("EvalCond: %v", err)
	}
	if !got {
		t.Error("success() is false before any step ran")
	}
}

// TestEvalCond_TakesTheTwoForms — a condition is an expression, bare
// or wrapped in one ${{ }}.
func TestEvalCond_TakesTheTwoForms(t *testing.T) {
	ec := exprCtx()
	tests := []struct {
		cond string
		want bool
	}{
		{"steps.fix.outputs.data.issue", true},
		{"${{ steps.fix.outputs.data.issue }}", true},
		{"steps.fix.outputs.status == 429", true},
		{"${{ steps.fix.outputs.status == 429 }}", true},
	}
	for _, tt := range tests {
		got, err := EvalCond(tt.cond, ec)
		if err != nil {
			t.Errorf("%s: %v", tt.cond, err)
			continue
		}
		if got != tt.want {
			t.Errorf("%s = %v, want %v", tt.cond, got, tt.want)
		}
	}
}

// TestEvalCond_RefusesTheFormsItCannotRead — a condition that carries
// text around its expression says something the engine cannot read.
func TestEvalCond_RefusesTheFormsItCannotRead(t *testing.T) {
	for _, cond := range []string{
		"steps.fix.outputs.status ==",
		"steps.fix.outputs.status == ",
		"failure()",
		"cancelled()",
		"unknown()",
		"success(1)",
		"empty()",
		"empty(a, b)",
		"${{ a }} ${{ b }}",
		"before ${{ a }}",
		"${{ a }} after",
		"${{ a",
		"(a == b",
		"a &&",
		"'unclosed",
	} {
		if _, err := EvalCond(cond, ExprCtx{}); err == nil {
			t.Errorf("%s was accepted, want an error", cond)
		}
	}
}

// TestParseExpression_BindsTheOperators — the tree of an expression
// says which operator binds which operand.
func TestParseExpression_BindsTheOperators(t *testing.T) {
	tests := []struct {
		src  string
		want string
	}{
		{"a == 1 || b == 2 && c == 3", "((path(a) == literal(1)) || ((path(b) == literal(2)) && (path(c) == literal(3))))"},
		{"a == 1 && b == 2 || c == 3", "(((path(a) == literal(1)) && (path(b) == literal(2))) || (path(c) == literal(3)))"},
		{"!a == b", "(!(path(a)) == path(b))"},
		{"!a && b", "(!(path(a)) && path(b))"},
		{"a == b < c", "(path(a) == (path(b) < path(c)))"},
		{"a < b == c", "((path(a) < path(b)) == path(c))"},
		{"a = b", "(path(a) = path(b))"},
		{"a != b", "(path(a) != path(b))"},
		{"(a || b) && c", "((path(a) || path(b)) && path(c))"},
		{"!success()", "!(call(success))"},
		{"empty(a)", "call(empty, path(a))"},
		{"'x'", "literal(x)"},
		{"42", "literal(42)"},
		{"true", "literal(true)"},
		{"null", "literal()"},
	}
	for _, tt := range tests {
		node, err := parseExpression(tt.src)
		if err != nil {
			t.Errorf("%s: %v", tt.src, err)
			continue
		}
		if got := exprTree(node); got != tt.want {
			t.Errorf("%s = %s, want %s", tt.src, got, tt.want)
		}
	}
}

// exprTree renders the tree of an expression, so a test can say which
// operator binds which operand.
func exprTree(node exprNode) string {
	switch n := node.(type) {
	case *literalNode:
		return "literal(" + n.value.toString() + ")"
	case *pathNode:
		return "path(" + strings.Join(n.segments, ".") + ")"
	case *callNode:
		parts := make([]string, 0, len(n.args))
		for _, arg := range n.args {
			parts = append(parts, exprTree(arg))
		}
		return "call(" + strings.Join(append([]string{n.name}, parts...), ", ") + ")"
	case *unaryNode:
		return "!(" + exprTree(n.operand) + ")"
	case *binaryNode:
		return "(" + exprTree(n.left) + " " + n.op + " " + exprTree(n.right) + ")"
	}
	return "?"
}

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
			"success":     "true",
			"status":      "200",
			"statusText":  "picked an issue",
			"data.issue":  "42",
			"data.branch": "nightme/issue-42",
		},
	}}
	tests := []struct {
		expr string
		want string
	}{
		{"${{ steps.fix.outputs.success }}", "true"},
		{"${{ steps.fix.outputs.status }}", "200"},
		{"${{ steps.fix.outputs.statusText }}", "picked an issue"},
		{"${{ steps.fix.outputs.data.issue }}", "42"},
		{"${{ steps.fix.status }}", "200"},
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

// TestExprContext_RendersTheOutcome — the outcome reaches the
// expressions under its four names: the verdict, the status, the
// words and the facts.
func TestExprContext_RendersTheOutcome(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: "picked an issue",
		Data:       map[string]string{"issue": "42"},
	}, false)

	ec := ExprContext(state)
	for _, tt := range []struct {
		expr string
		want string
	}{
		{"${{ steps.one.outputs.success }}", "true"},
		{"${{ steps.one.outputs.status }}", "200"},
		{"${{ steps.one.outputs.statusText }}", "picked an issue"},
		{"${{ steps.one.outputs.data.issue }}", "42"},
	} {
		if got := EvalString(tt.expr, ec); got != tt.want {
			t.Errorf("%s = %q, want %q", tt.expr, got, tt.want)
		}
	}
}

// TestExprContext_CarriesTheLastStep — the context holds the outputs
// of the last step that ran: it is what `success()` reads.
func TestExprContext_CarriesTheLastStep(t *testing.T) {
	wf := mustParse(t, threeStepYAML)
	state := newRunState(wf)
	RecordStep(state, "main", wf.Jobs["main"].Steps[0], outcome.Outcome{
		Status: http.StatusTooManyRequests,
	}, false)

	ec := ExprContext(state)
	if ec.Last["status"] != "429" {
		t.Errorf("the context holds the status %q, want 429", ec.Last["status"])
	}
	ok, err := EvalCond("success()", ec)
	if err != nil {
		t.Fatalf("EvalCond: %v", err)
	}
	if ok {
		t.Error("success() is true after a step that failed")
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

// TestEvalString_UnknownFunction — a function the language does not
// have renders as nothing.
func TestEvalString_UnknownFunction(t *testing.T) {
	got := EvalString("${{ failure() }}", ExprCtx{})
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestEvalString_UnknownIdentifier(t *testing.T) {
	got := EvalString("${{ bogus.x }}", ExprCtx{})
	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

// TestEvalString_QuotedBraces — a `}}` inside a string is ordinary
// text: the interpolation ends at the braces that close it.
func TestEvalString_QuotedBraces(t *testing.T) {
	got := EvalString("${{ 'a}}b' }}", ExprCtx{})
	if got != "a}}b" {
		t.Errorf("got %q, want %q", got, "a}}b")
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

// TestTheConditionReadsTheArgumentsOfTheRun — the run knows what it
// was asked for, and a condition may ask about it.
//
// The binding leaves the reference in the condition: the condition is
// an expression, and the run reads its arguments from its own context.
func TestTheConditionReadsTheArgumentsOfTheRun(t *testing.T) {
	wf := mustParse(t, `
title: t
args:
  - name: issue
jobs:
  main:
    steps:
      - id: one
        message: "/one"
        if: ${{ args.issue }}
      - id: two
        message: "/two"
        if: args.issue == "42"
`)
	bound, err := Bind(wf, map[string]string{"issue": "42"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	state := newRunState(wf)
	state.Args = map[string]string{"issue": "42"}

	decision, err := Decide(state, bound)
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if decision.Kind != DecisionRun || decision.Step.ID != "one" {
		t.Fatalf("the plan runs %q (kind %v), want the first step", decision.Step.ID, decision.Kind)
	}
}
