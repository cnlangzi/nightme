package wfe

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// EvalString evaluates ${{ }} expressions in s. Pure function.
//
// An interpolation holds an expression, and the expression is evaluated
// as one: it reads the context of the run and it may compare what it
// reads.
//
//   - ${{ env.X }}        → ec.Env["X"]
//   - ${{ args.X }}       → ec.Args["X"]
//   - ${{ event.X }}      → nested map lookup ec.Event["X"]
//   - ${{ steps.ID.K }}   → ec.Steps["ID"]["K"]
//   - ${{ steps.ID.outputs.K }} → the same, with the optional
//     `outputs` segment written out
//   - ${{ needs.JOB.K }}  → ec.Needs["JOB"]["K"]
//   - ${{ now }}          → the current time
//   - ${{ success() }}    → the verdict of the last step that ran
//   - ${{ empty(X) }}     → whether X is nothing
//
// The `$` is part of the syntax delimiter (matches GitHub Actions).
// An expression that does not parse renders as nothing: the text it
// stood for is not there to be read.
func EvalString(s string, ec ExprCtx) string {
	if !strings.Contains(s, "${{") {
		return s
	}
	var out strings.Builder
	i := 0
	for i < len(s) {
		if strings.HasPrefix(s[i:], "${{") {
			end := scanExpr(s, i+3)
			if end < 0 {
				out.WriteByte(s[i])
				i++
				continue
			}
			out.WriteString(evalInterpolated(s[i+3:end], ec))
			i = end + 2
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// evalInterpolated renders the expression of an interpolation as a
// string.
func evalInterpolated(src string, ec ExprCtx) string {
	node, err := parseExpression(strings.TrimSpace(src))
	if err != nil {
		return ""
	}
	return evalExpr(node, ec).toString()
}

// EvalMap evaluates ${{ }} expressions in every string value of m
// (recursively). Non-string leaves are untouched.
func EvalMap(m map[string]any, ec ExprCtx) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = evalValue(v, ec)
	}
	return out
}

func evalValue(v any, ec ExprCtx) any {
	switch x := v.(type) {
	case string:
		return EvalString(x, ec)
	case map[string]any:
		inner := make(map[string]any, len(x))
		for k, vv := range x {
			inner[k] = evalValue(vv, ec)
		}
		return inner
	case []any:
		inner := make([]any, len(x))
		for i, vv := range x {
			inner[i] = evalValue(vv, ec)
		}
		return inner
	}
	return v
}

// EvalCond evaluates a boolean condition.
//
// The condition is an expression, written bare or wrapped in one
// `${{ ... }}`. It is true when the value it comes to is true; a
// condition that names nothing is true when the value reads as true,
// and a condition that cannot be parsed is an error the caller reports.
func EvalCond(cond string, ec ExprCtx) (bool, error) {
	node, err := parseCondition(cond)
	if err != nil {
		return false, err
	}
	if node == nil {
		// An empty condition holds: the step runs.
		return true, nil
	}
	return evalExpr(node, ec).toBool(), nil
}

func toString(v any) string {
	switch x := v.(type) {
	case string:
		return x
	case bool:
		return strconv.FormatBool(x)
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return strconv.FormatFloat(x, 'f', -1, 64)
	default:
		var b bytes.Buffer
		fmt.Fprintf(&b, "%v", v)
		return b.String()
	}
}
