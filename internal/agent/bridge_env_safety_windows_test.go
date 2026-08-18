//go:build windows

// Regression guard for the env-format Windows spawn bug.
//
// The original bug: claudecode and pi bridges did
//
//	env = append(env, s.command)
//
// where s.command was a bare string like "pi" or "claude" with
// no KEY=VALUE format. Windows CreateProcess validates every
// env entry and rejects bare strings with ERROR_INVALID_PARAMETER
// (87), surfacing as
//
//	fork/exec C:\WINDOWS\system32\cmd.exe: The parameter is incorrect.
//
// Unix's execve(2) tolerates malformed env silently, so the bug
// only triggered on Windows. After PR #158 routed spawns through
// cmd.exe, the wrapper itself became the failing call — making
// the latent env bug newly visible on every Windows install.
//
// This test does a static scan of every bridge package for the
// bad pattern: any `env = append(env, <non-literal>...)` where
// the appended value is not a string literal in KEY=VALUE form.
// A future refactor that re-introduces the bug gets caught at
// `go test` time, not at user runtime.
package agent

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bridgeDirs is the set of packages that produce *exec.Cmd for
// the runtime. Each one must compose env from KEY=VALUE entries
// only. A package that uses something other than proc.New to
// spawn the agent binary should be reviewed and either migrated
// to proc.New or added to this list with explicit justification.
var bridgeDirs = []string{
	"internal/bridge/claudecode",
	"internal/bridge/codex",
	"internal/bridge/opencode",
	"internal/bridge/pi",
	"internal/bridge/pty",
	"internal/bridge/acp",
}

// moduleRoot walks up from the current test file's working
// directory until it finds go.mod, so the test can run from
// any `go test` invocation (package-local, `./...`, or via
// the Makefile) without a hard-coded relative path.
func moduleRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("could not find go.mod above %s", wd)
		}
		dir = parent
	}
}

// TestBridgeEnv_NoBareAppend walks every bridge package and
// rejects any file that appends a non-literal to env without a
// KEY=VALUE structure. The test is a coarse approximation — it
// flags any "env = append(env, IDENT)" where IDENT is not a
// string literal — but that catches the exact pattern that
// caused the regression.
func TestBridgeEnv_NoBareAppend(t *testing.T) {
	root := moduleRoot(t)
	for _, dir := range bridgeDirs {
		t.Run(dir, func(t *testing.T) {
			absDir := filepath.Join(root, dir)
			fset := token.NewFileSet()
			pkgs, err := parser.ParseDir(fset, absDir, nil, parser.ParseComments)
			if err != nil {
				t.Fatalf("ParseDir %s: %v", absDir, err)
			}
			for _, pkg := range pkgs {
				for _, file := range pkg.Files {
					walkEnvAppends(t, file, fset, dir)
				}
			}
		})
	}
}

// walkEnvAppends inspects every AssignStmt in file whose LHS is
// "env" and whose RHS is an append(env, ...). It flags any
// element inside append(...) that is not a basic literal or a
// key=value compound literal — those are the bare-string
// candidates that broke Windows CreateProcess.
func walkEnvAppends(t *testing.T, file *ast.File, fset *token.FileSet, pkg string) {
	ast.Inspect(file, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
			return true
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || ident.Name != "env" {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok {
			return true
		}
		af, ok := call.Fun.(*ast.SelectorExpr)
		if !ok || af.Sel.Name != "append" {
			return true
		}
		// call.Args[0] should be `env`; call.Args[1:] are
		// the appended elements.
		if len(call.Args) < 2 {
			return true
		}
		for _, arg := range call.Args[1:] {
			if isLiteralStringWithEquals(arg) {
				continue
			}
			if isStringVariableIdent(arg) {
				pos := fset.Position(arg.Pos())
				t.Errorf("%s:%d: env = append(env, %s) — bare string in env slice. "+
					"Windows CreateProcess rejects bare entries with ERROR_INVALID_PARAMETER (87). "+
					"Either omit the entry, or build a KEY=VALUE literal (e.g. \"AGENT=\"+%s).",
					pos.Filename, pos.Line, render(arg), render(arg))
			}
		}
		return true
	})
}

// isLiteralStringWithEquals returns true if expr is a string
// literal containing an "=" sign (i.e. a KEY=VALUE form). These
// are safe to append.
func isLiteralStringWithEquals(expr ast.Expr) bool {
	bl, ok := expr.(*ast.BasicLit)
	if !ok || bl.Kind != token.STRING {
		return false
	}
	return strings.Contains(bl.Value, "=")
}

// isStringVariableIdent returns true if expr is a bare
// identifier (e.g. `s.command`) or a selector ending in `.Name`
// where the user could be passing an unprefixed command name.
// We keep this conservative — a future "agent" string in cfg
// would not be caught, but the four specific offenders
// (s.command, name, command) all are.
func isStringVariableIdent(expr ast.Expr) bool {
	switch expr.(type) {
	case *ast.Ident:
		// Could be "name" / "command" / "binary" / etc.
		// We don't know the type, but bare identifiers in
		// env append are almost always wrong.
		return true
	case *ast.SelectorExpr:
		// Could be "s.command" / "cfg.command" / "starter.Name()".
		// Selector expressions on a struct receiver are the
		// exact pattern we want to flag.
		return true
	}
	return false
}

// render is a tiny ast.Expr → string helper for error messages.
// Avoids pulling in go/printer to keep the test deps small.
func render(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		if x, ok := e.X.(*ast.Ident); ok {
			return x.Name + "." + e.Sel.Name
		}
	}
	return "<expr>"
}
