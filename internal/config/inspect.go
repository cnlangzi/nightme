// The file-level check of the config file.
//
// Loading stays lenient on purpose: Load ignores the keys the schema
// does not have, so an old config keeps starting a daemon. A typo in
// a key name is invisible that way — the field it meant to set stays
// off, and nothing says so. Inspect is the strict counterpart: it
// reports the file that does not parse, the values whose type is
// wrong and the keys the schema does not have.
//
// The check is file-level on purpose. What a value MEANS — is the
// agent name a built-in, does the binary exist, are the credentials
// complete — belongs to the runtime that consumes it.

package config

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Diagnostic is one problem in the config file.
type Diagnostic struct {
	// Line is the 1-based line the problem is on. It is 0 when the
	// problem has no place in the file.
	Line int `json:"line"`
	// Column is the 1-based column the problem is on. It is 0 when
	// the problem has no place in the file.
	Column int `json:"column"`
	// Message says what is wrong.
	Message string `json:"message"`
}

// Report is what the check found in the config file.
type Report struct {
	// Path is the file that was checked.
	Path string
	// Exists says whether the file was there.
	Exists bool
	// Diagnostics is every problem the file has.
	Diagnostics []Diagnostic
}

// Inspect checks the config file at path and reports what is wrong
// with it.
//
// The check never writes: the file that comes out is the file that
// went in. A missing file is not a problem — a user who has not set
// nightme up yet is in a legal state.
func Inspect(path string) Report {
	report := Report{Path: path}
	if path == "" {
		return report
	}
	data, err := os.ReadFile(path)
	switch {
	case errors.Is(err, os.ErrNotExist):
		return report
	case err != nil:
		report.Exists = true
		report.Diagnostics = append(report.Diagnostics, Diagnostic{
			Message: fmt.Sprintf("cannot read %s: %v", path, err),
		})
		return report
	}
	report.Exists = true
	report.Diagnostics = inspectConfig(data)
	return report
}

// inspectConfig returns every problem in the config file's bytes.
func inspectConfig(data []byte) []Diagnostic {
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return []Diagnostic{yamlDiagnostic(err)}
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil
		}
		root = doc.Content[0]
	}

	var diags []Diagnostic
	diags = append(diags, checkUnknownKeys(root, reflect.TypeFor[Config](), "")...)

	// The decoder knows the types the schema expects; the tree walk
	// above only knows the keys. Both are needed: a value of the
	// wrong type is a problem the key check cannot see.
	var cfg Config
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		diags = append(diags, decodeDiagnostics(err)...)
	}
	sortDiagnostics(diags)
	return diags
}

// checkUnknownKeys reports the keys of a mapping node the schema does
// not have.
//
// The known keys come from the yaml tags of the struct type: the
// schema lives in one place, and a field added to a struct is a key
// the check accepts.
//
// The prefix is the path of the node — "" at the root, "feishu." one
// level down — and the message names the key's own path: "foo" at the
// root, "feishu.app_secrett" one level down.
func checkUnknownKeys(node *yaml.Node, t reflect.Type, prefix string) []Diagnostic {
	if node.Kind != yaml.MappingNode {
		return nil
	}
	fields, free := configFields(t)
	var diags []Diagnostic
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valNode := node.Content[i], node.Content[i+1]
		if keyNode.Kind != yaml.ScalarNode {
			continue
		}
		// A merge key ("<<") is yaml's own: it brings the keys of
		// another mapping in, and the loader resolves it. It is not
		// a key of the schema and not a mistake of the user.
		if keyNode.Tag == "!!merge" {
			continue
		}
		name := keyNode.Value
		field, ok := fields[name]
		if !ok {
			// A level with an inline map takes any key — the map's
			// keys are the user's own.
			if !free {
				diags = append(diags, Diagnostic{
					Line:    keyNode.Line,
					Column:  keyNode.Column,
					Message: fmt.Sprintf("unknown key %q", prefix+name),
				})
			}
			continue
		}
		diags = append(diags, checkValueKeys(valNode, field.typ, prefix+name)...)
	}
	return diags
}

// checkValueKeys reports the unknown keys below one known key.
//
// The path is the key's own path — "feishu", "agents[0]" — and the
// keys below it extend it: "feishu.rate_limit.rate_per_sec",
// "agents[0].name".
func checkValueKeys(node *yaml.Node, typ reflect.Type, path string) []Diagnostic {
	if typ.Kind() == reflect.Pointer {
		// An absent optional block has nothing to check.
		if node.Tag == "!!null" {
			return nil
		}
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		return checkUnknownKeys(node, typ, path+".")
	case reflect.Slice:
		elem := typ.Elem()
		if elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		if elem.Kind() != reflect.Struct {
			return nil
		}
		var diags []Diagnostic
		for i, item := range node.Content {
			diags = append(diags, checkUnknownKeys(item, elem, fmt.Sprintf("%s[%d].", path, i))...)
		}
		return diags
	default:
		// A map's keys are the user's own — a header, a label — and
		// a scalar has nothing below it.
		return nil
	}
}

// configField is one key of the schema.
type configField struct {
	typ reflect.Type
}

// configFields returns the keys a struct type defines, and whether the
// struct takes free keys.
//
// The rules are yaml.v3's, so the check accepts exactly what the
// loader accepts: the yaml tag names the key, a field without a tag
// is keyed by its lowercased name, a field tagged `,inline` adds its
// own keys at this level, and a field tagged `,inline` that is a map
// lets any key through.
func configFields(t reflect.Type) (fields map[string]configField, free bool) {
	fields = map[string]configField{}
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if f.PkgPath != "" && !f.Anonymous {
			continue // unexported
		}
		tag := f.Tag.Get("yaml")
		if tag == "-" {
			continue
		}
		name, opts, _ := strings.Cut(tag, ",")
		if strings.Contains(opts, "inline") {
			switch f.Type.Kind() {
			case reflect.Map:
				free = true
				continue
			case reflect.Struct, reflect.Pointer:
				inner := f.Type
				for inner.Kind() == reflect.Pointer {
					inner = inner.Elem()
				}
				if inner.Kind() != reflect.Struct {
					continue
				}
				innerFields, innerFree := configFields(inner)
				maps.Copy(fields, innerFields)
				free = free || innerFree
				continue
			}
		}
		if name == "" {
			name = strings.ToLower(f.Name)
		}
		fields[name] = configField{typ: f.Type}
	}
	return fields, free
}

// yamlDiagnostic turns the error of the parser into a diagnostic.
//
// The parser says where it gave up — "yaml: line 3: ..." — and the
// line becomes the position of the problem.
func yamlDiagnostic(err error) Diagnostic {
	line, msg := splitLine(err.Error())
	return Diagnostic{Line: line, Message: msg}
}

// decodeDiagnostics turns the error of the decoder into diagnostics.
//
// The decoder reports every type error it found at once: each one
// becomes a diagnostic of its own.
func decodeDiagnostics(err error) []Diagnostic {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		line, msg := splitLine(err.Error())
		return []Diagnostic{{Line: line, Message: msg}}
	}
	out := make([]Diagnostic, 0, len(typeErr.Errors))
	for _, msg := range typeErr.Errors {
		line, text := splitLine(msg)
		out = append(out, Diagnostic{Line: line, Message: text})
	}
	return out
}

// splitLine takes the line number a yaml.v3 message starts with and
// returns the line and the message without it.
//
// The parser writes "yaml: line 3: ..." and the decoder writes
// "line 3: ...". A message that starts with neither has no line.
func splitLine(msg string) (int, string) {
	rest, ok := strings.CutPrefix(msg, "yaml: line ")
	if !ok {
		rest, ok = strings.CutPrefix(msg, "line ")
		if !ok {
			return 0, msg
		}
	}
	n, tail, ok := strings.Cut(rest, ": ")
	if !ok {
		return 0, msg
	}
	line, err := strconv.Atoi(n)
	if err != nil {
		return 0, msg
	}
	return line, tail
}

// sortDiagnostics orders the problems by their place in the file: by
// line, then by column. A problem that has no place in the file comes
// last.
func sortDiagnostics(diags []Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		li, lj := diags[i].Line, diags[j].Line
		if li == 0 {
			li = math.MaxInt
		}
		if lj == 0 {
			lj = math.MaxInt
		}
		if li != lj {
			return li < lj
		}
		return diags[i].Column < diags[j].Column
	})
}
