package command

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The Data keys of the outcome contract are registered: every command
// declares the keys it produces in its own package, and a key that is
// not declared anywhere is a typo the compiler cannot catch.
//
// The test reads the source rather than the values: a key that is
// misspelled at a call site is a string literal nobody checks, and
// the whole point of the registry is that the spelling is checked.

// repoRoot is the root of the repository, relative to this package.
const repoRoot = "../.."

// keyConsts are the constants that declare a Data key.
var keyConsts = []string{"Key", "BoolKey"}

// registeredKey is one declared Data key.
type registeredKey struct {
	// name is the constant that declares it.
	name string
	// value is the key it stands for.
	value string
	// boolean says the key's value is "true" or "false".
	boolean bool
	// file is where the constant lives.
	file string
}

// TestDataKeys_AreRegistered — every key the code reads is declared.
func TestDataKeys_AreRegistered(t *testing.T) {
	declared := declaredKeys(t)
	used := usedKeys(t)
	for key, files := range used {
		if _, ok := declared[key]; !ok {
			t.Errorf("the key %q is read in %s but no package declares it", key, strings.Join(files, ", "))
		}
	}
}

// TestDataKeys_AreUsed — a declared key is a key the code writes.
func TestDataKeys_AreUsed(t *testing.T) {
	declared := declaredKeys(t)
	written := writtenKeys(t)
	for key, reg := range declared {
		if !written[key] {
			t.Errorf("the key %q is declared in %s but nothing writes it", key, reg.file)
		}
	}
}

// TestDataKeys_WrittenKeysAreRegistered — a key written into a fact
// map is a key the code invents, and a misspelled one reaches the
// program as a fact nobody reads.
//
// The packages that produce facts are the ones checked: their maps
// carry the facts of the outcome and nothing else.
func TestDataKeys_WrittenKeysAreRegistered(t *testing.T) {
	declared := declaredKeys(t)
	for _, dir := range producingPackages {
		for _, file := range packageFiles(t, dir) {
			for _, key := range literalMapKeys(t, file) {
				if nonOutcomeKeys[key] {
					continue
				}
				if _, ok := declared[key]; !ok {
					t.Errorf("the key %q is written in %s but no package declares it", key, file)
				}
			}
		}
	}
}

// TestDataKeys_OneNameOneKey — a constant name stands for one key, so
// two packages cannot give the same name two meanings.
func TestDataKeys_OneNameOneKey(t *testing.T) {
	byName := map[string]registeredKey{}
	for _, reg := range declaredKeys(t) {
		prev, ok := byName[reg.name]
		if ok && prev.value != reg.value {
			t.Errorf("the constant %s stands for %q in %s and for %q in %s",
				reg.name, prev.value, prev.file, reg.value, reg.file)
		}
		byName[reg.name] = reg
	}
}

// TestDataKeys_BooleansAreTrueOrFalse — a boolean key carries
// "true" or "false", never "1" or "0".
func TestDataKeys_BooleansAreTrueOrFalse(t *testing.T) {
	booleans := map[string]bool{}
	for _, reg := range declaredKeys(t) {
		if reg.boolean {
			booleans[reg.value] = true
		}
	}
	for _, lit := range booleanLiterals(t) {
		if !booleans[lit.key] {
			continue
		}
		if lit.value != "true" && lit.value != "false" {
			t.Errorf("the boolean key %q carries %q in %s", lit.key, lit.value, lit.file)
		}
	}
}

// declaredKeys reads every package's key constants.
func declaredKeys(t *testing.T) map[string]registeredKey {
	t.Helper()
	out := map[string]registeredKey{}
	for _, file := range sourceFiles(t) {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		for _, decl := range f.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok {
					continue
				}
				for i, name := range vs.Names {
					if !isKeyConst(name.Name) || i >= len(vs.Values) {
						continue
					}
					lit, ok := vs.Values[i].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						continue
					}
					value, err := strconv.Unquote(lit.Value)
					if err != nil {
						continue
					}
					out[value] = registeredKey{
						name:    name.Name,
						value:   value,
						boolean: strings.HasPrefix(name.Name, "BoolKey"),
						file:    file,
					}
				}
			}
		}
	}
	return out
}

// isKeyConst reports whether the constant name declares a Data key.
func isKeyConst(name string) bool {
	for _, prefix := range keyConsts {
		if strings.HasPrefix(name, prefix) && len(name) > len(prefix) {
			return true
		}
	}
	return false
}

// usedKeys reads the keys the code reads out of an outcome.
func usedKeys(t *testing.T) map[string][]string {
	t.Helper()
	declared := declaredKeys(t)
	byName := map[string]string{}
	for _, reg := range declared {
		byName[reg.name] = reg.value
	}
	out := map[string][]string{}
	for _, file := range sourceFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			idx, ok := n.(*ast.IndexExpr)
			if !ok {
				return true
			}
			sel, ok := idx.X.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Data" {
				return true
			}
			switch index := idx.Index.(type) {
			case *ast.BasicLit:
				if index.Kind != token.STRING {
					return true
				}
				key, err := strconv.Unquote(index.Value)
				if err != nil {
					return true
				}
				out[key] = append(out[key], file)
			case *ast.Ident:
				key, ok := byName[index.Name]
				if !ok {
					return true
				}
				out[key] = append(out[key], file)
			}
			return true
		})
	}
	return out
}

// writtenKeys reads the keys the code writes into a fact map.
//
// A key written through its constant counts as written: the constant
// stands for the key, and the code says so.
func writtenKeys(t *testing.T) map[string]bool {
	t.Helper()
	declared := declaredKeys(t)
	byName := map[string]string{}
	for _, reg := range declared {
		byName[reg.name] = reg.value
	}
	out := map[string]bool{}
	for _, file := range sourceFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.CompositeLit:
				if !isStringMap(node.Type) {
					return true
				}
				for _, elt := range node.Elts {
					kv, ok := elt.(*ast.KeyValueExpr)
					if !ok {
						continue
					}
					switch key := kv.Key.(type) {
					case *ast.BasicLit:
						if key.Kind != token.STRING {
							continue
						}
						if name, err := strconv.Unquote(key.Value); err == nil {
							out[name] = true
						}
					case *ast.Ident:
						if value, ok := byName[key.Name]; ok {
							out[value] = true
						}
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range node.Lhs {
					idx, ok := lhs.(*ast.IndexExpr)
					if !ok {
						continue
					}
					switch key := idx.Index.(type) {
					case *ast.BasicLit:
						if key.Kind != token.STRING {
							continue
						}
						if name, err := strconv.Unquote(key.Value); err == nil {
							out[name] = true
						}
					case *ast.Ident:
						if value, ok := byName[key.Name]; ok {
							out[value] = true
						}
					}
				}
			}
			return true
		})
	}
	return out
}

// booleanLiteral is one place a boolean key is written.
type booleanLiteral struct {
	key   string
	value string
	file  string
}

// booleanLiterals reads every place a key is written a literal value.
func booleanLiterals(t *testing.T) []booleanLiteral {
	t.Helper()
	var out []booleanLiteral
	for _, file := range sourceFiles(t) {
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			assign, ok := n.(*ast.AssignStmt)
			if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
				return true
			}
			idx, ok := assign.Lhs[0].(*ast.IndexExpr)
			if !ok {
				return true
			}
			lit, ok := assign.Rhs[0].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			value, err := strconv.Unquote(lit.Value)
			if err != nil {
				return true
			}
			switch key := idx.Index.(type) {
			case *ast.BasicLit:
				if key.Kind != token.STRING {
					return true
				}
				name, err := strconv.Unquote(key.Value)
				if err != nil {
					return true
				}
				out = append(out, booleanLiteral{key: name, value: value, file: file})
			case *ast.Ident:
				out = append(out, booleanLiteral{key: key.Name, value: value, file: file})
			}
			return true
		})
	}
	return out
}

// isStringMap reports whether the type is a map[string]string.
func isStringMap(expr ast.Expr) bool {
	mapType, ok := expr.(*ast.MapType)
	if !ok {
		return false
	}
	key, ok := mapType.Key.(*ast.Ident)
	if !ok || key.Name != "string" {
		return false
	}
	value, ok := mapType.Value.(*ast.Ident)
	return ok && value.Name == "string"
}

// producingPackages are the packages that produce the facts of an
// outcome.
var producingPackages = []string{
	"../../internal/chatsession",
	"../../internal/command/cwd",
	"../../internal/command/gtw",
	"../../internal/command/me",
	"../../internal/command/review",
	"../../internal/command/stop",
	"../../internal/command/use",
	"../../internal/command/wf",
	"../../internal/me",
}

// nonOutcomeKeys are the string keys of the maps that are not facts of
// an outcome: the environment the run hands the workflow.
var nonOutcomeKeys = map[string]bool{
	"WORKSPACE_CWD": true,
}

// packageFiles lists the Go files of a package.
func packageFiles(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading %s: %v", dir, err)
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		out = append(out, filepath.Join(dir, e.Name()))
	}
	return out
}

// literalMapKeys reads the string keys of the map[string]string
// literals of a file.
func literalMapKeys(t *testing.T, file string) []string {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("%s: %v", file, err)
	}
	var out []string
	ast.Inspect(f, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok || !isStringMap(lit.Type) {
			return true
		}
		for _, elt := range lit.Elts {
			kv, ok := elt.(*ast.KeyValueExpr)
			if !ok {
				continue
			}
			key, ok := kv.Key.(*ast.BasicLit)
			if !ok || key.Kind != token.STRING {
				continue
			}
			name, err := strconv.Unquote(key.Value)
			if err == nil {
				out = append(out, name)
			}
		}
		return true
	})
	return out
}

// sourceFiles lists the Go files of the repository.
func sourceFiles(t *testing.T) []string {
	t.Helper()
	var out []string
	for _, dir := range []string{"internal"} {
		root := filepath.Join(repoRoot, dir)
		err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			out = append(out, path)
			return nil
		})
		if err != nil {
			t.Fatalf("walking %s: %v", root, err)
		}
	}
	return out
}
