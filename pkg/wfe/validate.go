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
	"time"

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
// A workflow file is the `workflow.yml` of a bundle — a directory
// under dir — so the files that are not workflows are not checked.
//
// Two files that claim the same workflow name are one problem of
// their own: the name is ambiguous.
func ValidateDir(dir string) []Diagnostic {
	paths, err := WorkflowPaths(dir)
	if err != nil {
		return []Diagnostic{{Path: dir, Message: err.Error()}}
	}
	var diags []Diagnostic
	claims := map[string][]string{} // name -> the files that claim it
	for _, path := range paths {
		name := WorkflowName(dir, path)
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

	var diags []Diagnostic
	// The unknown-key check reads the tree, so it says what it found
	// whatever the shape of the document is.
	if unknownKeys {
		diags = append(diags, checkRootKeys(root)...)
	}
	// A document of the wrong shape cannot be judged further: the
	// workflow it decodes into is not the workflow the file describes.
	shape := checkShape(root)
	if len(shape) > 0 {
		diags = append(diags, shape...)
		sortByPath(diags)
		return nil, diags
	}

	var wf Workflow
	if err := root.Decode(&wf); err != nil {
		decodeDiags := decodeDiagnostics(err)
		sortByPath(decodeDiags)
		return nil, decodeDiags
	}

	diags = append(diags, checkSemantics(&wf, root)...)
	if len(diags) > 0 {
		sortByPath(diags)
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
		case "requires":
			diags = append(diags, checkRequiresShape(e.val)...)
		case "on":
			diags = append(diags, checkTrigger(e.val)...)
		case "jobs":
			diags = append(diags, checkJobs(e.val)...)
		}
	}
	return diags
}

// checkRequiresShape validates the `requires:` block of a workflow.
//
// An empty block names no requirement: it is the same as not writing
// it.
func checkRequiresShape(node *yaml.Node) []Diagnostic {
	if node.Kind == yaml.ScalarNode && node.Tag == "!!null" {
		return nil
	}
	if node.Kind != yaml.MappingNode {
		return []Diagnostic{diagnosticAt(node, fmt.Errorf("`requires:` must be a mapping (got %s)", nodeKind(node)))}
	}
	return nil
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
	diags = append(diags, checkRequires(wf, root)...)

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
	needsKnown := true
	for name, job := range wf.Jobs {
		jobMapping := jobNode(root, name)
		for _, need := range job.Needs {
			if !jobNames[need] {
				needsKnown = false
				diags = append(diags, diagnosticAt(jobMapping, fmt.Errorf("job %q needs unknown job %q", name, need)))
			}
		}
		if job.Iterations != nil && *job.Iterations < 0 {
			diags = append(diags, diagnosticAt(mappingValue(jobMapping, "iterations"),
				fmt.Errorf("job %q: iterations must be >= 0", name)))
		}
		if job.Sleep != "" {
			// The gap is a duration, the way the wait of a step is one.
			d, err := time.ParseDuration(job.Sleep)
			switch {
			case err != nil:
				diags = append(diags, diagnosticAt(mappingValue(jobMapping, "sleep"),
					fmt.Errorf("job %q: sleep: %q is not a duration", name, job.Sleep)))
			case d < 0:
				diags = append(diags, diagnosticAt(mappingValue(jobMapping, "sleep"),
					fmt.Errorf("job %q: sleep must be >= 0", name)))
			}
		}
	}
	// The circle is only worth reporting when every job can be placed:
	// an unknown need is a different problem, and it keeps a job from
	// being placed as well.
	if needsKnown && len(wf.JobOrder()) != len(wf.Jobs) {
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
			if err := validateStepMessage(step); err != nil {
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
	diags = append(diags, checkConditions(wf, root)...)
	return diags
}

// checkConditions parses the conditions of the jobs and the steps.
//
// A condition is an expression, and the plan cannot decide on a
// condition it cannot read: a condition that does not parse is a
// problem of the file, and the diagnostic points at the condition.
func checkConditions(wf *Workflow, root *yaml.Node) []Diagnostic {
	declared := map[string]bool{}
	for _, a := range wf.Args {
		declared[a.Name] = true
	}
	names := wf.ArgNames()
	var diags []Diagnostic
	for _, name := range sortedJobNames(wf.Jobs) {
		job := wf.Jobs[name]
		jobMapping := jobNode(root, name)
		if job.If != "" {
			diags = append(diags, conditionDiagnostics(mappingValue(jobMapping, "if"), job.If,
				fmt.Sprintf("job %q", name), declared, names)...)
		}
		for i := range job.Steps {
			step := &job.Steps[i]
			if step.If == "" {
				continue
			}
			stepMapping := stepNode(root, name, i)
			diags = append(diags, conditionDiagnostics(mappingValue(stepMapping, "if"), step.If,
				fmt.Sprintf("job %q step %d", name, i+1), declared, names)...)
		}
	}
	return diags
}

// conditionDiagnostics reports the problems of one condition.
//
// The condition is parsed and the arguments it names are checked: an
// argument the workflow does not declare is a problem of the file.
//
// The context says where the condition stands, the way the other
// diagnostics of a job and its steps do.
func conditionDiagnostics(node *yaml.Node, text, ctx string, declared map[string]bool, names []string) []Diagnostic {
	parsed, err := parseCondition(text)
	if err != nil {
		return []Diagnostic{diagnosticAt(node, fmt.Errorf("%s: the condition does not parse: %w", ctx, err))}
	}
	if parsed == nil {
		return nil
	}
	var diags []Diagnostic
	reported := map[string]bool{}
	for _, arg := range conditionArgs(parsed) {
		if declared[arg] || reported[arg] {
			continue
		}
		reported[arg] = true
		diags = append(diags, diagnosticAt(node,
			fmt.Errorf("%s: unknown argument %q (declared: %s)", ctx, arg, describeArgs(names))))
	}
	return diags
}

// checkRequires checks the version requirements of a workflow.
//
// A constraint that does not parse is a problem of the file: the
// workflow asks for a nightme that cannot be read.
func checkRequires(wf *Workflow, root *yaml.Node) []Diagnostic {
	requirement := strings.TrimSpace(wf.Requires.Nightme)
	if requirement == "" {
		return nil
	}
	if _, err := ParseConstraint(requirement); err != nil {
		node := mappingValue(mappingValue(root, "requires"), "nightme")
		return []Diagnostic{diagnosticAt(node, fmt.Errorf("requires nightme: %w", err))}
	}
	return nil
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

// checkRootKeys reports the keys the schema does not have.
//
// The known keys are read from the yaml tags of the workflow structs:
// the schema lives in one place, and a field that is added to a struct
// is a key the validator accepts.
func checkRootKeys(node *yaml.Node) []Diagnostic {
	diags := checkKeys(node, reflect.TypeOf(Workflow{}), "")
	for _, e := range mappingPairs(node) {
		switch e.key {
		case "args":
			for i, item := range sequenceItems(e.val) {
				diags = append(diags, checkKeys(item, reflect.TypeOf(Arg{}), fmt.Sprintf("argument %d", i+1))...)
			}
		case "requires":
			diags = append(diags, checkKeys(e.val, reflect.TypeOf(Requires{}), "requires")...)
		case "on":
			diags = append(diags, checkTriggerKeys(e.val)...)
		case "jobs":
			for _, je := range mappingPairs(e.val) {
				diags = append(diags, checkJobKeys(je.val, fmt.Sprintf("job %q", je.key))...)
			}
		}
	}
	return diags
}

// checkJobKeys reports the unknown keys of a job and of its steps.
func checkJobKeys(node *yaml.Node, ctx string) []Diagnostic {
	diags := checkKeys(node, reflect.TypeOf(Job{}), ctx)
	for _, e := range mappingPairs(node) {
		if e.key != "steps" {
			continue
		}
		for i, item := range sequenceItems(e.val) {
			stepCtx := fmt.Sprintf("%s step %d", ctx, i+1)
			diags = append(diags, checkKeys(item, reflect.TypeOf(Step{}), stepCtx)...)
			diags = append(diags, checkAgentChainKeys(item, stepCtx)...)
		}
	}
	return diags
}

// checkAgentChainKeys reports the unknown keys of the chain entries of
// a step.
func checkAgentChainKeys(node *yaml.Node, ctx string) []Diagnostic {
	var diags []Diagnostic
	for i, entry := range sequenceItems(mappingValue(node, "agents")) {
		diags = append(diags, checkKeys(entry, reflect.TypeOf(AgentChain{}), fmt.Sprintf("%s agents entry %d", ctx, i+1))...)
	}
	return diags
}

// checkTriggerKeys reports the unknown keys of the `on:` block and of
// the trigger blocks below it.
func checkTriggerKeys(node *yaml.Node) []Diagnostic {
	diags := checkKeys(node, reflect.TypeOf(Trigger{}), "on")
	for _, e := range mappingPairs(node) {
		switch e.key {
		case "schedule":
			for i, item := range sequenceItems(e.val) {
				diags = append(diags, checkKeys(item, reflect.TypeOf(ScheduleEntry{}), fmt.Sprintf("schedule entry %d", i+1))...)
			}
		case "pull_request":
			diags = append(diags, checkKeys(e.val, reflect.TypeOf(PRTrigger{}), "pull_request")...)
		case "branch":
			diags = append(diags, checkKeys(e.val, reflect.TypeOf(BranchTrigger{}), "branch")...)
		case "issue":
			diags = append(diags, checkKeys(e.val, reflect.TypeOf(EventFilter{}), "issue")...)
		case "mention":
			diags = append(diags, checkKeys(e.val, reflect.TypeOf(MentionTrigger{}), "mention")...)
		}
	}
	return diags
}

// checkKeys reports the keys of a mapping node the schema does not
// have.
//
// The type says which keys are known. The context says where the
// mapping is, so a message read on its own still says what it is
// about.
func checkKeys(node *yaml.Node, t reflect.Type, ctx string) []Diagnostic {
	known := knownKeys(t)
	var diags []Diagnostic
	for _, e := range mappingPairs(node) {
		if known[e.key] {
			continue
		}
		diags = append(diags, diagnosticAt(e.keyNode,
			fmt.Errorf("%sunknown key %q", contextPrefix(ctx), e.key)))
	}
	return diags
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
		return []Diagnostic{{Message: err.Error()}}
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

// sortByPath orders the problems by path, then by position within the
// file: by line, then by column. A problem that has no place in the
// file comes last.
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
