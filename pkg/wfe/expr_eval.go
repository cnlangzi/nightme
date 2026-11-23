package wfe

import (
	"math"
	"strconv"
	"strings"
	"time"
)

// The value model of the expression language.
//
// A value is one of four things: null, a bool, a number or a string.
// The language compares what it can and treats the rest as false: a
// condition that cannot be decided must not take the run down with it.
type valueKind int

const (
	valueNull valueKind = iota
	valueBool
	valueNumber
	valueString
)

// exprValue is a value of the expression language.
type exprValue struct {
	kind valueKind
	num  float64
	str  string
	flag bool
}

// nullValue is the value of a path that names nothing.
func nullValue() exprValue {
	return exprValue{kind: valueNull}
}

// boolValue is a boolean value.
func boolValue(b bool) exprValue {
	return exprValue{kind: valueBool, flag: b}
}

// numberValue is a numeric value.
func numberValue(n float64) exprValue {
	return exprValue{kind: valueNumber, num: n}
}

// stringValue is a string value.
func stringValue(s string) exprValue {
	return exprValue{kind: valueString, str: s}
}

// toBool reads the value as a truth.
//
// A null is false, a bool is itself, a number is true when it is not
// zero, and a string is false when it is empty, "false" or "0".
func (v exprValue) toBool() bool {
	switch v.kind {
	case valueNull:
		return false
	case valueBool:
		return v.flag
	case valueNumber:
		return v.num != 0
	case valueString:
		switch strings.TrimSpace(v.str) {
		case "", "false", "0":
			return false
		}
		return true
	}
	return false
}

// toNumber reads the value as a number.
//
// A string that does not read as a number is not a number: the
// comparison that meets it is false rather than an error.
func (v exprValue) toNumber() float64 {
	switch v.kind {
	case valueNull:
		return 0
	case valueBool:
		if v.flag {
			return 1
		}
		return 0
	case valueNumber:
		return v.num
	case valueString:
		text := strings.TrimSpace(v.str)
		if text == "" {
			return 0
		}
		num, err := strconv.ParseFloat(text, 64)
		if err != nil {
			return math.NaN()
		}
		return num
	}
	return math.NaN()
}

// toString reads the value as a string.
//
// A null is the empty string: an expression that names nothing reads
// as nothing, the way it did before the language had a tree.
func (v exprValue) toString() string {
	switch v.kind {
	case valueNull:
		return ""
	case valueBool:
		if v.flag {
			return "true"
		}
		return "false"
	case valueNumber:
		return strconv.FormatFloat(v.num, 'f', -1, 64)
	case valueString:
		return v.str
	}
	return ""
}

// evalExpr evaluates a node of an expression's tree.
func evalExpr(node exprNode, ec ExprCtx) exprValue {
	switch n := node.(type) {
	case *literalNode:
		return n.value
	case *pathNode:
		return evalPath(n.segments, ec)
	case *callNode:
		return evalCall(n, ec)
	case *unaryNode:
		return boolValue(!evalExpr(n.operand, ec).toBool())
	case *binaryNode:
		return evalBinary(n, ec)
	}
	return nullValue()
}

// evalPath reads a path of the context.
//
// A path that names nothing is null: the step that did not run yet, the
// fact it did not report, the argument the workflow does not declare.
func evalPath(segments []string, ec ExprCtx) exprValue {
	if len(segments) == 1 {
		if segments[0] == "now" {
			return stringValue(ec.Now.Format(time.RFC3339))
		}
		return nullValue()
	}
	switch segments[0] {
	case "env":
		value, ok := ec.Env[segments[1]]
		if !ok {
			return nullValue()
		}
		return stringValue(value)
	case "args":
		value, ok := ec.Args[segments[1]]
		if !ok {
			return nullValue()
		}
		return stringValue(value)
	case "event":
		return lookupEvent(ec.Event, segments[1:])
	case "steps":
		if len(segments) < 3 {
			return nullValue()
		}
		out, ok := ec.Steps[segments[1]]
		if !ok {
			return nullValue()
		}
		return lookupOutput(out, segments[2:])
	case "needs":
		if len(segments) < 3 {
			return nullValue()
		}
		out, ok := ec.Needs[segments[1]]
		if !ok {
			return nullValue()
		}
		return lookupOutput(out, segments[2:])
	}
	return nullValue()
}

// lookupOutput resolves the path of an output.
//
// The `outputs` segment is optional: the rest of the path is joined
// with "." into one key. A key the outputs do not hold falls back to
// the data namespace, so `steps.fix.outputs.issue` finds the outcome's
// `issue` fact.
func lookupOutput(out map[string]string, path []string) exprValue {
	if len(path) == 0 {
		return nullValue()
	}
	if path[0] == "outputs" {
		path = path[1:]
	}
	if len(path) == 0 {
		return nullValue()
	}
	key := strings.Join(path, ".")
	if value, ok := out[key]; ok {
		return stringValue(value)
	}
	if !strings.HasPrefix(key, "data.") {
		if value, ok := out["data."+key]; ok {
			return stringValue(value)
		}
	}
	return nullValue()
}

// lookupEvent resolves a path into the event of the run.
func lookupEvent(event map[string]any, path []string) exprValue {
	if len(path) == 0 {
		return nullValue()
	}
	value, ok := event[path[0]]
	if !ok {
		return nullValue()
	}
	if len(path) == 1 {
		return eventValue(value)
	}
	sub, ok := value.(map[string]any)
	if !ok {
		return nullValue()
	}
	return lookupEvent(sub, path[1:])
}

// eventValue reads a value of the event as a value of the language.
//
// The event carries what the trigger produced: a number stays a number
// and a bool stays a bool, so a condition can compare them.
func eventValue(value any) exprValue {
	switch v := value.(type) {
	case nil:
		return nullValue()
	case string:
		return stringValue(v)
	case bool:
		return boolValue(v)
	case int:
		return numberValue(float64(v))
	case int64:
		return numberValue(float64(v))
	case float64:
		return numberValue(v)
	}
	return stringValue(toString(value))
}

// evalCall calls one of the functions of the language.
func evalCall(node *callNode, ec ExprCtx) exprValue {
	switch node.name {
	case "success":
		return boolValue(lastSucceeded(ec))
	case "always":
		return boolValue(true)
	case "empty":
		value := evalExpr(node.args[0], ec)
		switch value.kind {
		case valueNull:
			return boolValue(true)
		case valueString:
			return boolValue(strings.TrimSpace(value.str) == "")
		}
		return boolValue(false)
	}
	return nullValue()
}

// lastSucceeded reports the verdict of the last step that ran.
//
// A run that ran no step yet has nothing that broke: the verdict is
// true.
func lastSucceeded(ec ExprCtx) bool {
	if ec.Last == nil {
		return true
	}
	value, ok := ec.Last["success"]
	if !ok {
		return true
	}
	return stringValue(value).toBool()
}

// evalBinary applies an operator to two operands.
func evalBinary(node *binaryNode, ec ExprCtx) exprValue {
	left := evalExpr(node.left, ec)
	right := evalExpr(node.right, ec)
	switch node.op {
	case "&&":
		return boolValue(left.toBool() && right.toBool())
	case "||":
		return boolValue(left.toBool() || right.toBool())
	case "==", "=":
		return boolValue(valuesEqual(left, right))
	case "!=":
		return boolValue(!valuesEqual(left, right))
	case "<", "<=", ">", ">=":
		return boolValue(compareValues(node.op, left, right))
	}
	return nullValue()
}

// valuesEqual reports whether two values are the same.
//
// Two strings are the same when they read the same. A bool on one side
// makes the comparison a question of truth. A null is the same as
// another null and as nothing else. The rest are numbers: a string
// that does not read as a number is not equal to anything.
func valuesEqual(a, b exprValue) bool {
	if a.kind == valueString && b.kind == valueString {
		return a.str == b.str
	}
	if (a.kind == valueBool) != (b.kind == valueBool) {
		return a.toBool() == b.toBool()
	}
	if a.kind == valueNull || b.kind == valueNull {
		return a.kind == valueNull && b.kind == valueNull
	}
	x, y := a.toNumber(), b.toNumber()
	if math.IsNaN(x) || math.IsNaN(y) {
		return false
	}
	return x == y
}

// compareValues compares two values.
//
// Two numbers are compared as numbers, two strings as text. Anything
// else cannot be compared: the comparison is false.
//
// A null is one of the things that cannot be compared: a path that
// names nothing says nothing about the order of two values, so the
// comparison is false rather than a comparison of nothing with zero.
func compareValues(op string, a, b exprValue) bool {
	if a.kind == valueNull || b.kind == valueNull {
		return false
	}
	x, y := a.toNumber(), b.toNumber()
	if !math.IsNaN(x) && !math.IsNaN(y) {
		return compareNumbers(op, x, y)
	}
	if a.kind == valueString && b.kind == valueString {
		return compareOrder(op, strings.Compare(a.str, b.str))
	}
	return false
}

// compareOrder applies an operator to the order of two values.
func compareOrder(op string, order int) bool {
	switch op {
	case "<":
		return order < 0
	case "<=":
		return order <= 0
	case ">":
		return order > 0
	case ">=":
		return order >= 0
	}
	return false
}

// compareNumbers applies an operator to two numbers.
func compareNumbers(op string, x, y float64) bool {
	switch op {
	case "<":
		return x < y
	case "<=":
		return x <= y
	case ">":
		return x > y
	case ">=":
		return x >= y
	}
	return false
}
