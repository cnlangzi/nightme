package wfe

import (
	"bytes"
	"fmt"
	"strconv"
	"strings"
)

// EvalString evaluates ${{ }} expressions in s. Pure function.
// Supports:
//   - ${{ env.X }}        → ec.Env["X"]
//   - ${{ event.X }}      → nested map lookup ec.Event["X"]
//   - ${{ steps.ID.K }}   → ec.Steps["ID"]["K"]
//   - ${{ needs.JOB.K }}  → ec.Needs["JOB"]["K"]
//   - ${{ success() }}    → true
//   - ${{ failure() }}    → false
//   - ${{ always() }}     → true
//   - ${{ cancelled() }}  → false
//
// The `$` is part of the syntax delimiter (matches GitHub Actions).
// Unknown identifiers become empty strings. v0; richer
// expressions can come later.
func EvalString(s string, ec ExprCtx, rt Runtime) string {
	if !strings.Contains(s, "${{") {
		return s
	}
	var out strings.Builder
	i := 0
	for i < len(s) {
		if i+2 < len(s) && s[i] == '$' && s[i+1] == '{' && s[i+2] == '{' {
			end := strings.Index(s[i+3:], "}}")
			if end < 0 {
				out.WriteByte(s[i])
				i++
				continue
			}
			expr := strings.TrimSpace(s[i+3 : i+3+end])
			out.WriteString(evalOne(expr, ec, rt))
			i = i + 3 + end + 2
			continue
		}
		out.WriteByte(s[i])
		i++
	}
	return out.String()
}

// EvalMap evaluates ${{ }} expressions in every string value of m
// (recursively). Non-string leaves are untouched.
func EvalMap(m map[string]any, ec ExprCtx, rt Runtime) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = evalValue(v, ec, rt)
	}
	return out
}

func evalValue(v any, ec ExprCtx, rt Runtime) any {
	switch x := v.(type) {
	case string:
		return EvalString(x, ec, rt)
	case map[string]any:
		inner := make(map[string]any, len(x))
		for k, vv := range x {
			inner[k] = evalValue(vv, ec, rt)
		}
		return inner
	case []any:
		inner := make([]any, len(x))
		for i, vv := range x {
			inner[i] = evalValue(vv, ec, rt)
		}
		return inner
	}
	return v
}

// EvalCond evaluates a boolean condition. Returns true if the
// condition is empty (vacuously true) or evaluates to a truthy value.
func EvalCond(cond string, ec ExprCtx, rt Runtime) (bool, error) {
	cond = strings.TrimSpace(cond)
	if cond == "" {
		return true, nil
	}
	v := EvalString(cond, ec, rt)
	if v == "" {
		return false, nil
	}
	// Truthy: any non-empty, non-"false" string.
	if v == "false" || v == "0" {
		return false, nil
	}
	return true, nil
}

func evalOne(expr string, ec ExprCtx, rt Runtime) string {
	// Function call: foo()
	if idx := strings.Index(expr, "("); idx > 0 && strings.HasSuffix(expr, ")") {
		name := strings.TrimSpace(expr[:idx])
		switch name {
		case "success":
			return "true"
		case "failure":
			return "false"
		case "always":
			return "true"
		case "cancelled":
			return "false"
		}
		// Unknown function — return empty
		return ""
	}
	// Dotted path: ns.key.subkey
	parts := strings.Split(expr, ".")
	if len(parts) < 2 {
		return ""
	}
	switch parts[0] {
	case "env":
		return ec.Env[parts[1]]
	case "event":
		return lookupMap(ec.Event, parts[1:])
	case "steps":
		if len(parts) < 3 {
			return ""
		}
		stepOut, ok := ec.Steps[parts[1]]
		if !ok {
			return ""
		}
		return stepOut[parts[2]]
	case "needs":
		if len(parts) < 3 {
			return ""
		}
		jobOut, ok := ec.Needs[parts[1]]
		if !ok {
			return ""
		}
		return jobOut[parts[2]]
	}
	return ""
}

func lookupMap(m map[string]any, path []string) string {
	if len(path) == 0 {
		return ""
	}
	v, ok := m[path[0]]
	if !ok {
		return ""
	}
	if len(path) == 1 {
		return toString(v)
	}
	sub, ok := v.(map[string]any)
	if !ok {
		return ""
	}
	return lookupMap(sub, path[1:])
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
