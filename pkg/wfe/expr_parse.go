package wfe

import (
	"fmt"
	"strconv"
	"strings"
)

// The expression language of the workflow engine.
//
// A condition is an expression: it is parsed into a tree and the tree
// is evaluated. The tree is what makes a comparison possible — a
// condition that asks "is the status 429" cannot be answered by a
// string that holds an interpolated value.
//
//	expr       := or
//	or         := and ( "||" and )*
//	and        := equality ( "&&" equality )*
//	equality   := comparison ( ("==" | "=" | "!=") comparison )*
//	comparison := unary ( ("<" | "<=" | ">" | ">=") unary )*
//	unary      := "!" unary | primary
//	primary    := "(" expr ")" | literal | call | path
//	literal    := string | number | "true" | "false" | "null"
//	call       := ident "(" [ expr ( "," expr )* ] ")"
//	path       := ident ( "." ident )*
//
// The `!` binds tighter than a comparison, so `!a == b` reads as
// `(!a) == b`. The equality operator comes before the comparison one,
// so `a == b < c` reads as `a == (b < c)`.

// exprTokenKind is the kind of a token of an expression.
type exprTokenKind int

const (
	tokenEOF exprTokenKind = iota
	tokenIdent
	tokenNumber
	tokenString
	tokenLParen
	tokenRParen
	tokenComma
	tokenDot
	tokenOr
	tokenAnd
	tokenEq
	tokenNe
	tokenLt
	tokenLe
	tokenGt
	tokenGe
	tokenNot
)

// exprToken is one token of an expression.
//
// The text is what an error message names; the number is what a
// numeric literal holds.
type exprToken struct {
	kind exprTokenKind
	text string
	num  float64
}

// tokenizeExpr splits an expression into its tokens.
func tokenizeExpr(src string) ([]exprToken, error) {
	var out []exprToken
	i := 0
	for i < len(src) {
		c := src[i]
		switch {
		case c == ' ' || c == '\t' || c == '\n' || c == '\r':
			i++
		case c == '(':
			out = append(out, exprToken{kind: tokenLParen, text: "("})
			i++
		case c == ')':
			out = append(out, exprToken{kind: tokenRParen, text: ")"})
			i++
		case c == ',':
			out = append(out, exprToken{kind: tokenComma, text: ","})
			i++
		case c == '.':
			out = append(out, exprToken{kind: tokenDot, text: "."})
			i++
		case c == '|':
			if !strings.HasPrefix(src[i:], "||") {
				return nil, fmt.Errorf("unexpected %q", c)
			}
			out = append(out, exprToken{kind: tokenOr, text: "||"})
			i += 2
		case c == '&':
			if !strings.HasPrefix(src[i:], "&&") {
				return nil, fmt.Errorf("unexpected %q", c)
			}
			out = append(out, exprToken{kind: tokenAnd, text: "&&"})
			i += 2
		case c == '=':
			// `=` and `==` mean the same thing: an expression has no
			// assignment, so there is nothing for a single `=` to mean
			// that `==` does not.
			if strings.HasPrefix(src[i:], "==") {
				out = append(out, exprToken{kind: tokenEq, text: "=="})
				i += 2
				continue
			}
			out = append(out, exprToken{kind: tokenEq, text: "="})
			i++
		case c == '!':
			if strings.HasPrefix(src[i:], "!=") {
				out = append(out, exprToken{kind: tokenNe, text: "!="})
				i += 2
				continue
			}
			out = append(out, exprToken{kind: tokenNot, text: "!"})
			i++
		case c == '<':
			if strings.HasPrefix(src[i:], "<=") {
				out = append(out, exprToken{kind: tokenLe, text: "<="})
				i += 2
				continue
			}
			out = append(out, exprToken{kind: tokenLt, text: "<"})
			i++
		case c == '>':
			if strings.HasPrefix(src[i:], ">=") {
				out = append(out, exprToken{kind: tokenGe, text: ">="})
				i += 2
				continue
			}
			out = append(out, exprToken{kind: tokenGt, text: ">"})
			i++
		case c == '\'' || c == '"':
			text, size, err := scanQuoted(src, i)
			if err != nil {
				return nil, err
			}
			out = append(out, exprToken{kind: tokenString, text: text})
			i += size
		case c >= '0' && c <= '9':
			num, size, err := scanNumber(src, i)
			if err != nil {
				return nil, err
			}
			out = append(out, exprToken{kind: tokenNumber, text: src[i : i+size], num: num})
			i += size
		case isIdentStart(c):
			name, size := scanIdent(src, i)
			out = append(out, exprToken{kind: tokenIdent, text: name})
			i += size
		default:
			return nil, fmt.Errorf("unexpected %q", c)
		}
	}
	return append(out, exprToken{kind: tokenEOF, text: "the end of the expression"}), nil
}

// scanQuoted reads the string literal that starts at i.
//
// The literal ends at its closing quote: a `}` or a `}}` inside it is
// ordinary text, so the scanner of an interpolation does not mistake it
// for the end of the expression.
func scanQuoted(src string, i int) (string, int, error) {
	quote := src[i]
	var b strings.Builder
	j := i + 1
	for j < len(src) {
		c := src[j]
		switch c {
		case '\\':
			// Only the quote and the backslash are escaped: a path that
			// holds a backslash keeps it.
			if j+1 < len(src) {
				switch src[j+1] {
				case '\\', '\'', '"':
					b.WriteByte(src[j+1])
					j += 2
					continue
				}
			}
			b.WriteByte(c)
			j++
		case quote:
			return b.String(), j + 1 - i, nil
		default:
			b.WriteByte(c)
			j++
		}
	}
	return "", 0, fmt.Errorf("the string is not closed")
}

// scanNumber reads the number that starts at i.
func scanNumber(src string, i int) (float64, int, error) {
	j := i
	for j < len(src) && src[j] >= '0' && src[j] <= '9' {
		j++
	}
	if j < len(src) && src[j] == '.' && j+1 < len(src) && src[j+1] >= '0' && src[j+1] <= '9' {
		j++
		for j < len(src) && src[j] >= '0' && src[j] <= '9' {
			j++
		}
	}
	num, err := strconv.ParseFloat(src[i:j], 64)
	if err != nil {
		return 0, 0, fmt.Errorf("%q is not a number", src[i:j])
	}
	return num, j - i, nil
}

// isIdentStart reports whether c may start an identifier.
func isIdentStart(c byte) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// isIdentPart reports whether c may continue an identifier.
func isIdentPart(c byte) bool {
	return isIdentStart(c) || (c >= '0' && c <= '9')
}

// scanIdent reads the identifier that starts at i.
//
// A hyphen belongs to the identifier when a letter follows it: the
// names of arguments may hold one, and the language has no arithmetic
// that a hyphen could start.
func scanIdent(src string, i int) (string, int) {
	j := i
	for j < len(src) {
		if isIdentPart(src[j]) {
			j++
			continue
		}
		if src[j] == '-' && j+1 < len(src) && isIdentStart(src[j+1]) {
			j += 2
			continue
		}
		break
	}
	return src[i:j], j - i
}

// exprNode is a node of an expression's tree.
type exprNode interface {
	// exprNode marks the type of the node.
	exprNode()
}

// literalNode is a constant of the language.
type literalNode struct {
	value exprValue
}

// pathNode is a path into the context: env.NAME, steps.id.outputs.key.
type pathNode struct {
	segments []string
}

// callNode is a call of one of the functions of the language.
type callNode struct {
	name string
	args []exprNode
}

// unaryNode is the negation of an operand.
type unaryNode struct {
	operand exprNode
}

// binaryNode is an operation on two operands.
type binaryNode struct {
	op          string
	left, right exprNode
}

func (n *literalNode) exprNode() {}
func (n *pathNode) exprNode()    {}
func (n *callNode) exprNode()    {}
func (n *unaryNode) exprNode()   {}
func (n *binaryNode) exprNode()  {}

// exprFunctions are the functions an expression may call.
//
// The list is short on purpose: an outcome has one verdict and one
// status, and everything else is a path and an operator.
var exprFunctions = map[string]int{
	"success": 0,
	"always":  0,
	"empty":   1,
}

// exprParser reads the tokens of an expression.
type exprParser struct {
	tokens []exprToken
	pos    int
}

// peek returns the token the parser stands on.
func (p *exprParser) peek() exprToken {
	return p.tokens[p.pos]
}

// next takes the token the parser stands on.
func (p *exprParser) next() exprToken {
	t := p.tokens[p.pos]
	if p.pos < len(p.tokens)-1 {
		p.pos++
	}
	return t
}

// parseExpression parses an expression into its tree.
func parseExpression(src string) (exprNode, error) {
	tokens, err := tokenizeExpr(src)
	if err != nil {
		return nil, err
	}
	p := &exprParser{tokens: tokens}
	node, err := p.parseOr()
	if err != nil {
		return nil, err
	}
	if p.peek().kind != tokenEOF {
		return nil, fmt.Errorf("unexpected %q", p.peek().text)
	}
	return node, nil
}

// parseOr parses the `||` level.
func (p *exprParser) parseOr() (exprNode, error) {
	left, err := p.parseAnd()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokenOr {
		p.next()
		right, err := p.parseAnd()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: "||", left: left, right: right}
	}
	return left, nil
}

// parseAnd parses the `&&` level.
func (p *exprParser) parseAnd() (exprNode, error) {
	left, err := p.parseEquality()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokenAnd {
		p.next()
		right, err := p.parseEquality()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: "&&", left: left, right: right}
	}
	return left, nil
}

// parseEquality parses the `==` level.
func (p *exprParser) parseEquality() (exprNode, error) {
	left, err := p.parseComparison()
	if err != nil {
		return nil, err
	}
	for p.peek().kind == tokenEq || p.peek().kind == tokenNe {
		op := p.next().text
		right, err := p.parseComparison()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: op, left: left, right: right}
	}
	return left, nil
}

// parseComparison parses the `<` level.
func (p *exprParser) parseComparison() (exprNode, error) {
	left, err := p.parseUnary()
	if err != nil {
		return nil, err
	}
	for {
		kind := p.peek().kind
		if kind != tokenLt && kind != tokenLe && kind != tokenGt && kind != tokenGe {
			return left, nil
		}
		op := p.next().text
		right, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		left = &binaryNode{op: op, left: left, right: right}
	}
}

// parseUnary parses the `!` level.
func (p *exprParser) parseUnary() (exprNode, error) {
	if p.peek().kind == tokenNot {
		p.next()
		operand, err := p.parseUnary()
		if err != nil {
			return nil, err
		}
		return &unaryNode{operand: operand}, nil
	}
	return p.parsePrimary()
}

// parsePrimary parses a literal, a call, a path or a parenthesised
// expression.
func (p *exprParser) parsePrimary() (exprNode, error) {
	tok := p.peek()
	switch tok.kind {
	case tokenLParen:
		p.next()
		node, err := p.parseOr()
		if err != nil {
			return nil, err
		}
		if p.peek().kind != tokenRParen {
			return nil, fmt.Errorf("expected %q", ")")
		}
		p.next()
		return node, nil
	case tokenNumber:
		p.next()
		return &literalNode{value: numberValue(tok.num)}, nil
	case tokenString:
		p.next()
		return &literalNode{value: stringValue(tok.text)}, nil
	case tokenIdent:
		p.next()
		switch tok.text {
		case "true":
			return &literalNode{value: boolValue(true)}, nil
		case "false":
			return &literalNode{value: boolValue(false)}, nil
		case "null":
			return &literalNode{value: nullValue()}, nil
		}
		if p.peek().kind == tokenLParen {
			return p.parseCall(tok.text)
		}
		return p.parsePath(tok.text)
	}
	if tok.kind == tokenEOF {
		return nil, fmt.Errorf("the expression ends too early")
	}
	return nil, fmt.Errorf("unexpected %q", tok.text)
}

// parseCall parses the arguments of a call.
func (p *exprParser) parseCall(name string) (exprNode, error) {
	p.next() // the "("
	want, known := exprFunctions[name]
	if !known {
		return nil, fmt.Errorf("unknown function %q", name)
	}
	var args []exprNode
	if p.peek().kind != tokenRParen {
		for {
			arg, err := p.parseOr()
			if err != nil {
				return nil, err
			}
			args = append(args, arg)
			if p.peek().kind != tokenComma {
				break
			}
			p.next()
		}
	}
	if p.peek().kind != tokenRParen {
		return nil, fmt.Errorf("expected %q", ")")
	}
	p.next()
	if len(args) != want {
		return nil, fmt.Errorf("the function %q takes %d arguments, got %d", name, want, len(args))
	}
	return &callNode{name: name, args: args}, nil
}

// parsePath parses the segments of a path.
func (p *exprParser) parsePath(first string) (exprNode, error) {
	segments := []string{first}
	for p.peek().kind == tokenDot {
		p.next()
		if p.peek().kind != tokenIdent {
			return nil, fmt.Errorf("expected a name after %q", ".")
		}
		segments = append(segments, p.next().text)
	}
	return &pathNode{segments: segments}, nil
}

// exprPathRoots are the roots a path may have: the namespaces of the
// context, and the clock.
var exprPathRoots = map[string]bool{
	"env":   true,
	"args":  true,
	"event": true,
	"steps": true,
	"needs": true,
	"now":   true,
}

// conditionArgs returns the arguments a condition names.
//
// A condition is an expression, and an argument it names is a path
// that starts with `args`.
func conditionArgs(node exprNode) []string {
	var out []string
	collectArgs(node, &out)
	return out
}

// conditionPaths returns the paths a condition names.
//
// A condition is an expression, and it reads the run through its paths:
// a path that names a namespace the run does not have is a condition
// the engine cannot read.
func conditionPaths(node exprNode) []*pathNode {
	var out []*pathNode
	collectPaths(node, &out)
	return out
}

// collectPaths walks an expression's tree and collects the paths it
// names.
func collectPaths(node exprNode, out *[]*pathNode) {
	switch n := node.(type) {
	case *pathNode:
		*out = append(*out, n)
	case *callNode:
		for _, arg := range n.args {
			collectPaths(arg, out)
		}
	case *unaryNode:
		collectPaths(n.operand, out)
	case *binaryNode:
		collectPaths(n.left, out)
		collectPaths(n.right, out)
	}
}

// collectArgs walks an expression's tree and collects the arguments it
// names.
func collectArgs(node exprNode, out *[]string) {
	switch n := node.(type) {
	case *pathNode:
		if len(n.segments) == 2 && n.segments[0] == "args" {
			*out = append(*out, n.segments[1])
		}
	case *callNode:
		for _, arg := range n.args {
			collectArgs(arg, out)
		}
	case *unaryNode:
		collectArgs(n.operand, out)
	case *binaryNode:
		collectArgs(n.left, out)
		collectArgs(n.right, out)
	}
}

// parseCondition parses the condition of a step or of a job.
//
// The condition is an expression. It is written bare, or wrapped in one
// `${{ ... }}` — the two forms are the same thing. Any other shape is
// a mistake: a condition that carries text around its expression says
// something the engine cannot read.
func parseCondition(text string) (exprNode, error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return nil, nil
	}
	if !strings.Contains(trimmed, "${{") {
		return parseExpression(trimmed)
	}
	return parseWrappedCondition(trimmed)
}

// parseWrappedCondition parses a condition that is wrapped in `${{ }}`.
func parseWrappedCondition(text string) (exprNode, error) {
	if !strings.HasPrefix(text, "${{") {
		return nil, fmt.Errorf("a condition is one expression: the text around ${{ }} is not allowed")
	}
	end := scanExpr(text, 3)
	if end < 0 {
		return nil, fmt.Errorf("a condition has an unclosed ${{ }}")
	}
	inner := strings.TrimSpace(text[3:end])
	if strings.TrimSpace(text[end+2:]) != "" {
		return nil, fmt.Errorf("a condition is one expression: the text around ${{ }} is not allowed")
	}
	if strings.Contains(inner, "${{") {
		return nil, fmt.Errorf("a condition is one expression")
	}
	return parseExpression(inner)
}

// scanExpr finds the `}}` that closes an interpolation.
//
// The scan skips the quoted strings: a `}}` inside one is ordinary text
// and does not end the expression.
func scanExpr(s string, start int) int {
	i := start
	for i < len(s) {
		switch s[i] {
		case '\'', '"':
			_, size, err := scanQuoted(s, i)
			if err != nil {
				return -1
			}
			i += size
		case '}':
			if i+1 < len(s) && s[i+1] == '}' {
				return i
			}
			i++
		default:
			i++
		}
	}
	return -1
}
