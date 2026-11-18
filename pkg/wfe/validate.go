package wfe

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"os"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// Validate parses raw YAML bytes as a workflow and returns every
// problem it found. An empty slice means the workflow is valid.
//
// The validation surface is strict: a key the schema does not have is
// reported, because a typo changes what the workflow does without
// saying so.
func Validate(raw []byte) []Diagnostic {
	_, diags := collect(raw, true)
	return diags
}

// ValidateFile returns every problem in the workflow file at path.
//
// The diagnostics carry the path they were found in. A file that
// cannot be read is one diagnostic: the path and the reason.
func ValidateFile(path string) []Diagnostic {
	raw, err := os.ReadFile(path)
	if err != nil {
		return []Diagnostic{{Path: path, Message: err.Error()}}
	}
	_, diags := collect(raw, true)
	for i := range diags {
		diags[i].Path = path
	}
	return diags
}

// ValidateDir returns every problem in every workflow file of dir.
//
// Two files that claim the same workflow name are one problem of
// their own: the name is ambiguous.
func ValidateDir(dir string) []Diagnostic {
	paths, err := WorkflowPaths(dir)
	if err != nil {
		return []Diagnostic{{Path: dir, Message: err.Error()}}
	}
	var diags []Diagnostic
	claims := map[string][]string{} // base name -> the files that claim it
	for _, path := range paths {
		name := BaseName(path)
		if name == "" {
			continue
		}
		claims[name] = append(claims[name], path)
		diags = append(diags, ValidateFile(path)...)
	}
	for name, files := range claims {
		if len(files) < 2 {
			continue
		}
		diags = append(diags, Diagnostic{
			Path:    files[1],
			Message: fmt.Sprintf("duplicate workflow name %q in %s and %s", name, files[0], files[1]),
		})
	}
	sortByPath(diags)
	return diags
}

// collect parses raw YAML bytes as a workflow and returns the
// workflow and every problem it found.
//
// The workflow comes back only when there is no problem: a workflow
// that does not validate is not a workflow.
//
// The unknown-key check runs when unknownKeys is set. The load path
// leaves it off: it ignores the keys the schema does not have, so a
// workflow file that carries extra fields keeps running.
func collect(raw []byte, unknownKeys bool) (*Workflow, []Diagnostic) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil, []Diagnostic{{Message: "the file is empty"}}
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, []Diagnostic{yamlDiagnostic(err)}
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil, []Diagnostic{{Message: "the file is empty"}}
		}
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, []Diagnostic{{Message: "the file is not a workflow: it has no keys"}}
	}

	// A document of the wrong shape cannot be judged further: the
	// workflow it decodes into is not the workflow the file describes.
	diags := checkShape(root)
	if len(diags) > 0 {
		sortDiagnostics(diags)
		return nil, diags
	}

	var wf Workflow
	if err := root.Decode(&wf); err != nil {
		diags := decodeDiagnostics(err)
		sortDiagnostics(diags)
		return nil, diags
	}

	diags = checkSemantics(&wf, root)
	if unknownKeys {
		diags = append(diags, checkUnknownKeys(root)...)
	}
	if len(diags) > 0 {
		sortDiagnostics(diags)
		return nil, diags
	}
	return &wf, nil
}

// checkShape checks the shape of a workflow file and returns every
// problem it found.
//
// A key the schema does not have is not a shape problem: it is
// ignored here and reported by checkUnknownKeys.
func checkShape(root *yaml.Node) []Diagnostic {
	var diags []Diagnostic
	for _, e := range mappingPairs(root) {
		switch e.key {
		case "args":
			diags = append(diags, checkMappingList("`args:`", e.val)...)
		case "on":
			diags = append(diags, checkTrigger(e.val)...)
		case "jobs":
			diags = append(diags, checkJobs(e.val)...)
		}
	}
	return diags
}

// checkMappingList checks that a node holds a list of mappings.
//
// The label names the list in an error message.
func checkMappingList(label string, node *yaml.Node) []Diagnostic {
	if node.Kind != yaml.SequenceNode {
		return []Diagnostic{diagnosticAt(node, fmt.Errorf("%s must be a list (got %s)", label, nodeKind(node)))}
	}
	var diags []Diagnostic
	for _, item := range node.Content {
		if err := requireMapping(item); err != nil {
			diags = append(diags, diagnosticAt(item, fmt.Errorf("%s: %w", label, err)))
		}
	}
	return diags
}

// checkTrigger validates the `on:` block of a workflow.
//
// The block is parsed and displayed; nothing fires it yet.
func checkTrigger(node *yaml.Node) []Diagnostic {
	if node.Kind != yaml.MappingNode {
		return []Diagnostic{diagnosticAt(node, fmt.Errorf("`on:` must be a mapping (got %s)", nodeKind(node)))}
	}
	return nil
}

// checkJobs validates the jobs of a workflow and their steps.
func checkJobs(node *yaml.Node) []Diagnostic {
	if node.Kind != yaml.MappingNode {
		return []Diagnostic{diagnosticAt(node, fmt.Errorf("`jobs:` must be a mapping (got %s)", nodeKind(node)))}
	}
	var diags []Diagnostic
	for _, e := range mappingPairs(node) {
		jobEntries, err := mappingEntries(e.val)
		if err != nil {
			diags = append(diags, diagnosticAt(e.val, fmt.Errorf("job %q: %w", e.key, err)))
			continue
		}
		for _, je := range jobEntries {
			if je.key == "iterations" {
				if err := requireWholeNumber(je.key, je.val); err != nil {
					diags = append(diags, diagnosticAt(je.val, fmt.Errorf("job %q: %w", e.key, err)))
				}
			}
			if je.key == "steps" {
				diags = append(diags, checkMappingList(fmt.Sprintf("job %q: `steps:`", e.key), je.val)...)
			}
		}
	}
	return diags
}

// checkSemantics checks the semantics of a workflow and gives every
// step an id.
//
// The root node is the parsed document: it says which keys the file
// actually wrote, so a missing key can be told apart from a key that
// was written with a zero value — and it says where every part of the
// file is.
func checkSemantics(wf *Workflow, root *yaml.Node) []Diagnostic {
	var diags []Diagnostic
	if strings.TrimSpace(wf.Title) == "" {
		diags = append(diags, diagnosticAt(root, errors.New("title is required")))
	}
	if !hasKey(root, "instruct") {
		wf.Instruct = true
	}

	seenArgs := map[string]bool{}
	for i, arg := range wf.Args {
		node := argNode(root, i)
		if strings.TrimSpace(arg.Name) == "" {
			diags = append(diags, diagnosticAt(node, fmt.Errorf("argument %d has no name", i+1)))
			continue
		}
		if seenArgs[arg.Name] {
			diags = append(diags, diagnosticAt(node, fmt.Errorf("duplicate argument %q", arg.Name)))
		}
		seenArgs[arg.Name] = true
	}

	if len(wf.Jobs) == 0 {
		diags = append(diags, diagnosticAt(root, errors.New("jobs is required (at least one)")))
	}
	jobNames := map[string]bool{}
	for name := range wf.Jobs {
		jobNames[name] = true
	}
	for name, job := range wf.Jobs {
		jobMapping := jobNode(root, name)
		for _, need := range job.Needs {
			if !jobNames[need] {
				diags = append(diags, diagnosticAt(jobMapping, fmt.Errorf("job %q needs unknown job %q", name, need)))
			}
		}
		if job.Iterations != nil && *job.Iterations < 0 {
			diags = append(diags, diagnosticAt(mappingValue(jobMapping, "iterations"),
				fmt.Errorf("job %q: iterations must be >= 0", name)))
		}
	}
	if len(wf.JobOrder()) != len(wf.Jobs) {
		diags = append(diags, diagnosticAt(root, errors.New("the jobs need each other in a circle")))
	}
	seenSteps := map[string]string{} // step id -> the job it belongs to
	for _, name := range sortedJobNames(wf.Jobs) {
		job := wf.Jobs[name]
		jobMapping := jobNode(root, name)
		if len(job.Steps) == 0 {
			diags = append(diags, diagnosticAt(jobMapping, fmt.Errorf("job %q has no steps", name)))
			continue
		}
		for i := range job.Steps {
			step := &job.Steps[i]
			stepMapping := stepNode(root, name, i)
			if err := validateStep(step); err != nil {
				diags = append(diags, diagnosticAt(stepMapping, fmt.Errorf("job %q step %d: %w", name, i+1, err)))
			}
			if step.ID == "" {
				step.ID = syntheticStepID(name, i+1)
			}
			// The run state is keyed by step id, so two steps may
			// not share one.
			if prev, ok := seenSteps[step.ID]; ok {
				diags = append(diags, diagnosticAt(stepMapping,
					fmt.Errorf("the step id %q is used by job %q and job %q", step.ID, prev, name)))
			}
			seenSteps[step.ID] = name
		}
	}
	diags = append(diags, checkArgExpressions(wf, root)...)
	return diags
}

// checkArgExpressions reports every expression that names an
// argument the workflow does not declare.
//
// An argument the workflow does not declare is reported once, at the
// place the expression stands.
func checkArgExpressions(wf *Workflow, root *yaml.Node) []Diagnostic {
	declared := map[string]bool{}
	for _, a := range wf.Args {
		declared[a.Name] = true
	}
	var diags []Diagnostic
	reported := map[string]bool{}
	for _, text := range workflowTexts(wf) {
		for _, m := range exprRef.FindAllStringSubmatch(text, -1) {
			expr := strings.TrimSpace(m[1])
			arg := argExpr.FindStringSubmatch(expr)
			if arg == nil {
				continue
			}
			if declared[arg[1]] || reported[arg[1]] {
				continue
			}
			reported[arg[1]] = true
			diags = append(diags, diagnosticAt(findTextNode(root, m[0]),
				fmt.Errorf("unknown argument %q (declared: %s)", arg[1], describeArgs(wf.ArgNames()))))
		}
	}
	return diags
}

// checkUnknownKeys reports the keys the schema does not have.
//
// The known keys are read from the yaml tags of the workflow structs:
// the schema lives in one place, and a field that is added to a struct
// is a key the validator accepts.
func checkUnknownKeys(root *yaml.Node) []Diagnostic {
	var diags []Diagnostic
	checkRootKeys(root, &diags)
	return diags
}

// checkRootKeys reports the unknown keys of the root mapping and of
// the blocks below it.
func checkRootKeys(node *yaml.Node, diags *[]Diagnostic) {
	checkKeys(node, reflect.TypeOf(Workflow{}), "", diags)
	for _, e := range mappingPairs(node) {
		switch e.key {
		case "args":
			for i, item := range sequenceItems(e.val) {
				checkKeys(item, reflect.TypeOf(Arg{}), fmt.Sprintf("argument %d", i+1), diags)
			}
		case "on":
			checkTriggerKeys(e.val, diags)
		case "jobs":
			for _, je := range mappingPairs(e.val) {
				checkJobKeys(je.val, fmt.Sprintf("job %q", je.key), diags)
			}
		}
	}
}

// checkJobKeys reports the unknown keys of a job and of its steps.
func checkJobKeys(node *yaml.Node, ctx string, diags *[]Diagnostic) {
	checkKeys(node, reflect.TypeOf(Job{}), ctx, diags)
	for _, e := range mappingPairs(node) {
		if e.key != "steps" {
			continue
		}
		for i, item := range sequenceItems(e.val) {
			checkKeys(item, reflect.TypeOf(Step{}), fmt.Sprintf("%s step %d", ctx, i+1), diags)
		}
	}
}

// checkTriggerKeys reports the unknown keys of the `on:` block and of
// the trigger blocks below it.
func checkTriggerKeys(node *yaml.Node, diags *[]Diagnostic) {
	checkKeys(node, reflect.TypeOf(Trigger{}), "on", diags)
	for _, e := range mappingPairs(node) {
		switch e.key {
		case "schedule":
			for i, item := range sequenceItems(e.val) {
				checkKeys(item, reflect.TypeOf(ScheduleEntry{}), fmt.Sprintf("schedule entry %d", i+1), diags)
			}
		case "pull_request":
			checkKeys(e.val, reflect.TypeOf(PRTrigger{}), "pull_request", diags)
		case "branch":
			checkKeys(e.val, reflect.TypeOf(BranchTrigger{}), "branch", diags)
		case "issue":
			checkKeys(e.val, reflect.TypeOf(EventFilter{}), "issue", diags)
		case "mention":
			checkKeys(e.val, reflect.TypeOf(MentionTrigger{}), "mention", diags)
		}
	}
}

// checkKeys reports the keys of a mapping node the schema does not
// have.
//
// The type says which keys are known. The context says where the
// mapping is, so a message read on its own still says what it is
// about.
func checkKeys(node *yaml.Node, t reflect.Type, ctx string, diags *[]Diagnostic) {
	known := knownKeys(t)
	for _, e := range mappingPairs(node) {
		if known[e.key] {
			continue
		}
		*diags = append(*diags, diagnosticAt(e.keyNode,
			fmt.Errorf("%sunknown key %q", contextPrefix(ctx), e.key)))
	}
}

// knownKeys returns the keys the schema defines for a struct type.
//
// The keys are the yaml tags of its fields: a field with no tag or a
// tag of "-" is not a key.
func knownKeys(t reflect.Type) map[string]bool {
	out := map[string]bool{}
	for i := 0; i < t.NumField(); i++ {
		tag := t.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if name == "" {
			continue
		}
		out[name] = true
	}
	return out
}

// contextPrefix returns the prefix a message of that context carries.
func contextPrefix(ctx string) string {
	if ctx == "" {
		return ""
	}
	return ctx + ": "
}

// diagnosticAt returns the diagnostic of a problem at a node.
//
// A node that is not there — a problem that belongs to no place in
// the file — gives a diagnostic without a position.
func diagnosticAt(node *yaml.Node, err error) Diagnostic {
	d := Diagnostic{Message: err.Error()}
	if node != nil {
		d.Line = node.Line
		d.Column = node.Column
	}
	return d
}

// yamlDiagnostic turns an error of the parser into a diagnostic.
//
// The parser says where it gave up — "yaml: line 3: ..." — and the
// line becomes the position of the problem.
func yamlDiagnostic(err error) Diagnostic {
	msg := err.Error()
	line := 0
	if rest, ok := strings.CutPrefix(msg, "yaml: line "); ok {
		if n, tail, ok := strings.Cut(rest, ": "); ok {
			if parsed, convErr := strconv.Atoi(n); convErr == nil {
				line = parsed
				msg = tail
			}
		}
	}
	return Diagnostic{Line: line, Message: msg}
}

// decodeDiagnostics turns the error of the decoder into diagnostics.
//
// The decoder reports every type error it found at once: each one
// becomes a diagnostic of its own.
func decodeDiagnostics(err error) []Diagnostic {
	var typeErr *yaml.TypeError
	if !errors.As(err, &typeErr) {
		return []Diagnostic{{Message: err.Error()}}
	}
	out := make([]Diagnostic, 0, len(typeErr.Errors))
	for _, msg := range typeErr.Errors {
		line := 0
		if n, tail, ok := strings.Cut(msg, ": "); ok {
			if parsed, convErr := strconv.Atoi(n); convErr == nil {
				line = parsed
				msg = tail
			}
		}
		out = append(out, Diagnostic{Line: line, Message: msg})
	}
	return out
}

// findTextNode returns the first node whose text holds the needle.
//
// It is how a problem in a text finds its place in the file.
func findTextNode(node *yaml.Node, needle string) *yaml.Node {
	if node == nil {
		return nil
	}
	if node.Kind == yaml.ScalarNode && strings.Contains(node.Value, needle) {
		return node
	}
	for _, child := range node.Content {
		if found := findTextNode(child, needle); found != nil {
			return found
		}
	}
	return nil
}

// sortDiagnostics orders the problems of one file by position: by
// line, then by column. A problem that has no place in the file
// comes last.
func sortDiagnostics(diags []Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		return lessPosition(diags[i], diags[j])
	})
}

// sortByPath orders the problems of many files by path, then by
// position within the file.
func sortByPath(diags []Diagnostic) {
	sort.SliceStable(diags, func(i, j int) bool {
		if diags[i].Path != diags[j].Path {
			return diags[i].Path < diags[j].Path
		}
		return lessPosition(diags[i], diags[j])
	})
}

// lessPosition tells which of two problems comes first in the file.
func lessPosition(a, b Diagnostic) bool {
	al, bl := a.Line, b.Line
	if al == 0 {
		al = math.MaxInt
	}
	if bl == 0 {
		bl = math.MaxInt
	}
	if al != bl {
		return al < bl
	}
	return a.Column < b.Column
}
