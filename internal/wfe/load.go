package wfe

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Failure is one workflow file that did not load.
//
// The name is the base name the file would have provided: it is what
// the operator asks for, so the failure is reported against it.
type Failure struct {
	// Name is the base name of the file.
	Name string
	// Path is the file that did not load.
	Path string
	// Err is why the file did not load.
	Err error
}

// Error implements the error interface.
func (f Failure) Error() string {
	return f.Err.Error()
}

// LoadDir loads the workflow files of dir.
//
// Both `*.yaml` and `*.yml` are read, and a workflow's identity is the
// base name of its file. The workflows that loaded come back with one
// failure per base name that is unusable: a broken file does not take
// the workflows beside it down with it.
func LoadDir(dir string) ([]*Workflow, []Failure) {
	paths, err := WorkflowPaths(dir)
	if err != nil {
		return nil, []Failure{{Path: dir, Err: fmt.Errorf("the workflows directory %s cannot be read: %w", dir, err)}}
	}
	claims := map[string][]string{}  // base name -> the files that claim it
	loaded := map[string]*Workflow{} // base name -> the workflow that loaded
	broken := map[string]Failure{}   // base name -> why the file did not load
	for _, path := range paths {
		name := BaseName(path)
		if name == "" {
			continue
		}
		claims[name] = append(claims[name], path)
		wf, err := LoadFile(path)
		if err != nil {
			broken[name] = Failure{
				Name: name,
				Path: path,
				Err:  fmt.Errorf("%s could not be loaded: %w", filepath.Base(path), err),
			}
			continue
		}
		loaded[name] = wf
	}
	names := make([]string, 0, len(claims))
	for name := range claims {
		names = append(names, name)
	}
	sort.Strings(names)
	var workflows []*Workflow
	var failures []Failure
	for _, name := range names {
		files := claims[name]
		if len(files) > 1 {
			// Two files claim the name: it is ambiguous, and neither
			// file is offered under it. The clash is the reason the
			// name is unusable, not the state of one of the files.
			failures = append(failures, Failure{
				Name: name,
				Path: files[1],
				Err: fmt.Errorf("wfe: duplicate workflow name %q in %s and %s",
					name, files[0], files[1]),
			})
			continue
		}
		if failure, bad := broken[name]; bad {
			failures = append(failures, failure)
			continue
		}
		workflows = append(workflows, loaded[name])
	}
	return workflows, failures
}

// WorkflowPaths returns the workflow files of dir, sorted.
//
// A workflow file is a `*.yaml` or a `*.yml` file. A directory that
// cannot be read is an error: the caller says so instead of reporting
// an empty directory.
//
// The directory is checked before it is read: a path that is not a
// directory is refused on every platform, because reading it does not
// fail the same way everywhere.
func WorkflowPaths(dir string) ([]string, error) {
	if strings.TrimSpace(dir) == "" {
		return nil, nil
	}
	info, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("%s is not a directory", dir)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		switch filepath.Ext(e.Name()) {
		case ".yaml", ".yml":
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// BaseName returns the identity of a workflow file: its name without
// the extension.
func BaseName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// LoadFile parses and validates a single workflow file.
//
// The file's base name is the workflow's identity and the workflow's
// title states what it is for. The identity is set on the workflow;
// the file does not carry it.
func LoadFile(path string) (*Workflow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("wfe: %s: %w", filepath.Base(path), err)
	}
	wf, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("wfe: %s: %w", filepath.Base(path), err)
	}
	name := BaseName(path)
	if name == "" {
		return nil, fmt.Errorf("wfe: %s: the file has no name", filepath.Base(path))
	}
	wf.Name = name
	return wf, nil
}

// Parse parses and validates raw YAML bytes as a workflow.
//
// The workflow carries no identity: the identity is the name of the
// file it came from, and the loader sets it (see LoadFile).
func Parse(raw []byte) (*Workflow, error) {
	wf, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("wfe: %w", err)
	}
	return wf, nil
}

// parse parses and validates raw YAML bytes as a workflow.
//
// Its errors carry no prefix: the caller names the file they come
// from.
func parse(raw []byte) (*Workflow, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil, fmt.Errorf("the file is empty")
		}
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("the file is not a workflow: it has no keys")
	}
	if err := checkDocument(root); err != nil {
		return nil, err
	}

	var wf Workflow
	if err := root.Decode(&wf); err != nil {
		return nil, err
	}
	if err := validate(&wf, root); err != nil {
		return nil, err
	}
	return &wf, nil
}

// The keys each level of the schema accepts.
var (
	workflowKeys = keySet("title", "args", "instruct", "workspaces", "on", "jobs")
	argKeys      = keySet("name", "default")
	jobKeys      = keySet("needs", "if", "iterations", "steps")
	stepKeys     = keySet("id", "if", "env", "continue-on-error", "note", "allow", "attempts", "message", "shell", "use", "with", "wait")
	triggerKeys  = keySet("schedule", "pull_request", "branch", "issue", "mention")
)

// keySet builds a set of accepted keys.
func keySet(keys ...string) map[string]bool {
	out := make(map[string]bool, len(keys))
	for _, k := range keys {
		out[k] = true
	}
	return out
}

// entry is one key/value pair of a YAML mapping.
type entry struct {
	key string
	// line is where the key is written.
	line int
	// val is the value of the key.
	val *yaml.Node
}

// mappingEntries returns the key/value pairs of a YAML mapping.
func mappingEntries(node *yaml.Node) ([]entry, error) {
	if node == nil {
		return nil, nil
	}
	if node.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("expected a mapping, got %s", nodeKind(node))
	}
	out := make([]entry, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		out = append(out, entry{
			key:  node.Content[i].Value,
			line: node.Content[i].Line,
			val:  node.Content[i+1],
		})
	}
	return out, nil
}

// nodeKind names the kind of a YAML node, for an error message.
func nodeKind(node *yaml.Node) string {
	switch node.Kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.ScalarNode:
		return "a value"
	case yaml.AliasNode:
		return "an alias"
	}
	return "something else"
}

// checkDocument validates the keys of a workflow file.
//
// A key the schema does not know is refused by name. The retired keys
// get their own message: a file that still uses one of them has to be
// rewritten, and the error says what to write instead.
func checkDocument(root *yaml.Node) error {
	entries, err := mappingEntries(root)
	if err != nil {
		return err
	}
	for _, e := range entries {
		switch e.key {
		case "steps":
			return fmt.Errorf("`steps:` is retired (line %d): a workflow keeps its steps in `jobs:`", e.line)
		case "agent":
			return fmt.Errorf("`agent:` is retired (line %d): a run uses the agent of the chat that started it", e.line)
		case "name":
			return fmt.Errorf("unknown key %q: the workflow's name is its file name; use %q for a description (line %d)",
				e.key, "title", e.line)
		case "goal":
			return fmt.Errorf("unknown key %q: use %q for a description (line %d)", e.key, "title", e.line)
		}
		if !workflowKeys[e.key] {
			return fmt.Errorf("unknown key %q (line %d)", e.key, e.line)
		}
		switch e.key {
		case "args":
			if err := checkArgs(e.val); err != nil {
				return err
			}
		case "on":
			if err := checkTrigger(e.val); err != nil {
				return err
			}
		case "jobs":
			if err := checkJobs(e.val); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkArgs validates the argument specs of a workflow.
func checkArgs(node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("`args:` must be a list (got %s)", nodeKind(node))
	}
	for _, item := range node.Content {
		entries, err := mappingEntries(item)
		if err != nil {
			return fmt.Errorf("`args:`: %w", err)
		}
		for _, e := range entries {
			if !argKeys[e.key] {
				return fmt.Errorf("unknown key %q in an argument (line %d)", e.key, e.line)
			}
		}
	}
	return nil
}

// checkTrigger validates the `on:` block of a workflow.
//
// The block is parsed and displayed; nothing fires it yet.
func checkTrigger(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("`on:` must be a mapping (got %s)", nodeKind(node))
	}
	entries, err := mappingEntries(node)
	if err != nil {
		return fmt.Errorf("`on:`: %w", err)
	}
	for _, e := range entries {
		if !triggerKeys[e.key] {
			return fmt.Errorf("unknown key %q in `on:` (line %d)", e.key, e.line)
		}
	}
	return nil
}

// checkJobs validates the jobs of a workflow and their steps.
func checkJobs(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("`jobs:` must be a mapping (got %s)", nodeKind(node))
	}
	entries, err := mappingEntries(node)
	if err != nil {
		return fmt.Errorf("`jobs:`: %w", err)
	}
	for _, e := range entries {
		jobEntries, err := mappingEntries(e.val)
		if err != nil {
			return fmt.Errorf("job %q: %w", e.key, err)
		}
		for _, je := range jobEntries {
			if !jobKeys[je.key] {
				return fmt.Errorf("unknown key %q in job %q (line %d)", je.key, e.key, je.line)
			}
			if je.key == "iterations" {
				if err := requireWholeNumber(je.key, je.val); err != nil {
					return fmt.Errorf("job %q: %w", e.key, err)
				}
			}
			if je.key == "steps" {
				if err := checkSteps(e.key, je.val); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// checkSteps validates the steps of a job.
func checkSteps(job string, node *yaml.Node) error {
	if node.Kind != yaml.SequenceNode {
		return fmt.Errorf("job %q: `steps:` must be a list (got %s)", job, nodeKind(node))
	}
	for _, item := range node.Content {
		entries, err := mappingEntries(item)
		if err != nil {
			return fmt.Errorf("job %q: `steps:`: %w", job, err)
		}
		for _, e := range entries {
			switch e.key {
			case "run":
				return fmt.Errorf("`run:` is retired (line %d): use `shell:`", e.line)
			case "prompt":
				return fmt.Errorf("`prompt:` is retired (line %d): use `message:`", e.line)
			case "agent":
				return fmt.Errorf("`agent:` is retired (line %d): a run uses the agent of the chat that started it", e.line)
			}
			if !stepKeys[e.key] {
				return fmt.Errorf("unknown key %q in a step of job %q (line %d)", e.key, job, e.line)
			}
			if e.key == "attempts" {
				if err := requireWholeNumber(e.key, e.val); err != nil {
					return fmt.Errorf("job %q: %w", job, err)
				}
			}
		}
	}
	return nil
}

// requireWholeNumber checks that a scalar node holds a whole number.
//
// yaml.v3 decodes a float into an int field by truncating it, so a
// negative fraction would slip past a check on the decoded value.
func requireWholeNumber(key string, node *yaml.Node) error {
	if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
		return fmt.Errorf("%s must be a whole number, got %q", key, node.Value)
	}
	return nil
}

// validate checks the semantics of a workflow and gives every step an
// id.
//
// The root node is the parsed document: it says which keys the file
// actually wrote, so a missing key can be told apart from a key that
// was written with a zero value.
func validate(wf *Workflow, root *yaml.Node) error {
	if strings.TrimSpace(wf.Title) == "" {
		return fmt.Errorf("title is required")
	}
	if !hasKey(root, "instruct") {
		wf.Instruct = true
	}

	seenArgs := map[string]bool{}
	for i, arg := range wf.Args {
		if strings.TrimSpace(arg.Name) == "" {
			return fmt.Errorf("argument %d has no name", i+1)
		}
		if seenArgs[arg.Name] {
			return fmt.Errorf("duplicate argument %q", arg.Name)
		}
		seenArgs[arg.Name] = true
	}

	if len(wf.Jobs) == 0 {
		return fmt.Errorf("jobs is required (at least one)")
	}
	jobNames := map[string]bool{}
	for name := range wf.Jobs {
		jobNames[name] = true
	}
	for name, job := range wf.Jobs {
		for _, need := range job.Needs {
			if !jobNames[need] {
				return fmt.Errorf("job %q needs unknown job %q", name, need)
			}
		}
		if job.Iterations != nil && *job.Iterations < 0 {
			return fmt.Errorf("job %q: iterations must be >= 0", name)
		}
	}
	if len(wf.JobOrder()) != len(wf.Jobs) {
		return fmt.Errorf("the jobs need each other in a circle")
	}
	seenSteps := map[string]string{} // step id -> the job it belongs to
	for _, name := range sortedJobNames(wf.Jobs) {
		job := wf.Jobs[name]
		if len(job.Steps) == 0 {
			return fmt.Errorf("job %q has no steps", name)
		}
		for i := range job.Steps {
			step := &job.Steps[i]
			if err := validateStep(step); err != nil {
				return fmt.Errorf("job %q step %d: %w", name, i+1, err)
			}
			if step.ID == "" {
				step.ID = syntheticStepID(name, i+1)
			}
			// The run state is keyed by step id, so two steps may
			// not share one.
			if prev, ok := seenSteps[step.ID]; ok {
				return fmt.Errorf("the step id %q is used by job %q and job %q", step.ID, prev, name)
			}
			seenSteps[step.ID] = name
		}
	}
	if err := checkArgExpressions(wf); err != nil {
		return err
	}
	return nil
}

// validateStep checks that a step names exactly one kind.
func validateStep(s *Step) error {
	n := 0
	if s.Message != "" {
		n++
	}
	if s.Shell != "" {
		n++
	}
	if s.Use != "" {
		n++
	}
	if s.Wait != "" {
		n++
	}
	if n != 1 {
		return fmt.Errorf("exactly one of message / shell / use / wait must be set (got %d)", n)
	}
	if s.Attempts < 0 {
		return fmt.Errorf("attempts must be >= 0")
	}
	if s.Wait != "" {
		if _, err := time.ParseDuration(s.Wait); err != nil {
			return fmt.Errorf("wait: %q is not a duration", s.Wait)
		}
	}
	return nil
}

// syntheticStepID builds the id of a step that declares none: the job
// name and the step's 1-based position in that job.
func syntheticStepID(job string, position int) string {
	return fmt.Sprintf("%s#%d", job, position)
}

// hasKey reports whether the mapping holds the key.
func hasKey(root *yaml.Node, key string) bool {
	if root == nil || root.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(root.Content); i += 2 {
		if root.Content[i].Value == key {
			return true
		}
	}
	return false
}

// JobOrder returns the workflow's jobs in topological order: no job
// appears before the jobs it needs.
//
// The order is stable: jobs that are ready at the same time come back
// sorted by name.
func (w *Workflow) JobOrder() []string {
	return topoSort(w.Jobs)
}

// sortedJobNames returns the workflow's job names, sorted.
func sortedJobNames(jobs map[string]Job) []string {
	out := make([]string, 0, len(jobs))
	for name := range jobs {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// Steps returns every step of every job in topological order.
func (w *Workflow) Steps() []Step {
	out := make([]Step, 0, len(w.Jobs))
	for _, name := range w.JobOrder() {
		out = append(out, w.Jobs[name].Steps...)
	}
	return out
}

// ArgNames returns the workflow's declared arguments, in declaration
// order.
func (w *Workflow) ArgNames() []string {
	out := make([]string, 0, len(w.Args))
	for _, a := range w.Args {
		out = append(out, a.Name)
	}
	return out
}

// topoSort returns jobs in topological order (no job appears
// before its dependencies). Returns stable order on ties (sorted by
// name). Falls back to a simple greedy order; no cycle detection
// beyond what validate() catches (unknown need is an error there).
func topoSort(jobs map[string]Job) []string {
	order := make([]string, 0, len(jobs))
	placed := map[string]bool{}
	for len(order) < len(jobs) {
		// Find a job whose deps are all placed; pick the one with
		// the smallest name (stable).
		var pick string
		var picked []string
		for name, job := range jobs {
			if placed[name] {
				continue
			}
			ready := true
			for _, need := range job.Needs {
				if !placed[need] {
					ready = false
					break
				}
			}
			if ready {
				picked = append(picked, name)
			}
		}
		if len(picked) == 0 {
			// Cycle (validate() should have caught this; defensive)
			break
		}
		sort.Strings(picked)
		pick = picked[0]
		order = append(order, pick)
		placed[pick] = true
	}
	return order
}

// exprRef matches a `${{ ... }}` expression.
var exprRef = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

// argExpr matches an `${{ args.NAME }}` reference.
var argExpr = regexp.MustCompile(`^\s*args\.([A-Za-z0-9_-]+)\s*$`)

// Bind expands the `${{ args.NAME }}` references of a workflow with
// the given values and returns the bound workflow; the source workflow
// is left untouched.
//
// An argument the workflow does not declare is an error. An argument
// with no value falls back to its default; an argument with neither a
// value nor a default is an error.
//
// Every other expression is left alone: it can only be resolved once
// the run knows what the earlier steps produced.
func Bind(wf *Workflow, args map[string]string) (*Workflow, error) {
	if wf == nil {
		return nil, fmt.Errorf("wfe: workflow is nil")
	}
	declared := map[string]bool{}
	for _, a := range wf.Args {
		declared[a.Name] = true
	}
	given := make([]string, 0, len(args))
	for name := range args {
		given = append(given, name)
	}
	sort.Strings(given)
	for _, name := range given {
		if !declared[name] {
			return nil, fmt.Errorf("wfe: %s: unknown argument %q (declared: %s)", wf.Name, name, describeArgs(wf.ArgNames()))
		}
	}
	values := make(map[string]string, len(args))
	for _, a := range wf.Args {
		if v, ok := args[a.Name]; ok {
			values[a.Name] = v
			continue
		}
		if a.Default == nil {
			return nil, fmt.Errorf("wfe: %s: missing argument %q", wf.Name, a.Name)
		}
		values[a.Name] = *a.Default
	}

	out := cloneWorkflow(wf)
	expandWorkflowArgs(out, values)

	// A bound step may have lost its kind to an empty argument.
	for _, name := range out.JobOrder() {
		job := out.Jobs[name]
		for i := range job.Steps {
			if err := validateStep(&job.Steps[i]); err != nil {
				return nil, fmt.Errorf("wfe: %s: job %q step %d: %w", out.Name, name, i+1, err)
			}
		}
	}
	return out, nil
}

// checkArgExpressions reports the first expression that names an
// argument the workflow does not declare.
func checkArgExpressions(wf *Workflow) error {
	declared := map[string]bool{}
	for _, a := range wf.Args {
		declared[a.Name] = true
	}
	for _, text := range workflowTexts(wf) {
		for _, m := range exprRef.FindAllStringSubmatch(text, -1) {
			expr := strings.TrimSpace(m[1])
			arg := argExpr.FindStringSubmatch(expr)
			if arg == nil {
				continue
			}
			if !declared[arg[1]] {
				return fmt.Errorf("unknown argument %q (declared: %s)", arg[1], describeArgs(wf.ArgNames()))
			}
		}
	}
	return nil
}

// workflowTexts returns every string of a workflow that may hold an
// expression.
func workflowTexts(wf *Workflow) []string {
	texts := []string{wf.Title}
	for _, name := range wf.JobOrder() {
		job := wf.Jobs[name]
		texts = append(texts, job.If)
		for _, step := range job.Steps {
			texts = append(texts, step.If, step.Message, step.Shell, step.Note, step.Use)
			texts = append(texts, step.Allow...)
			for _, v := range step.Env {
				texts = append(texts, v)
			}
			texts = append(texts, mapValues(step.With)...)
		}
	}
	return texts
}

// mapValues returns the string leaves of a map.
func mapValues(m map[string]any) []string {
	var out []string
	for _, v := range m {
		out = append(out, valueStrings(v)...)
	}
	return out
}

// valueStrings returns the string leaves of a value.
func valueStrings(v any) []string {
	switch x := v.(type) {
	case string:
		return []string{x}
	case map[string]any:
		return mapValues(x)
	case []any:
		var out []string
		for _, item := range x {
			out = append(out, valueStrings(item)...)
		}
		return out
	}
	return nil
}

// cloneWorkflow copies a workflow so a bound one never touches the
// source.
func cloneWorkflow(wf *Workflow) *Workflow {
	out := &Workflow{
		Title:      wf.Title,
		Name:       wf.Name,
		Instruct:   wf.Instruct,
		Workspaces: append([]string(nil), wf.Workspaces...),
		On:         wf.On,
		Jobs:       make(map[string]Job, len(wf.Jobs)),
	}
	for _, a := range wf.Args {
		arg := Arg{Name: a.Name}
		if a.Default != nil {
			def := *a.Default
			arg.Default = &def
		}
		out.Args = append(out.Args, arg)
	}
	for name, job := range wf.Jobs {
		steps := make([]Step, len(job.Steps))
		for i, step := range job.Steps {
			steps[i] = step
			if len(step.Allow) > 0 {
				steps[i].Allow = append([]string(nil), step.Allow...)
			}
			if len(step.Env) > 0 {
				env := make(map[string]string, len(step.Env))
				for k, v := range step.Env {
					env[k] = v
				}
				steps[i].Env = env
			}
			if len(step.With) > 0 {
				steps[i].With = cloneAnyMap(step.With)
			}
		}
		cloned := Job{
			Needs: append([]string(nil), job.Needs...),
			If:    job.If,
			Steps: steps,
		}
		if job.Iterations != nil {
			rounds := *job.Iterations
			cloned.Iterations = &rounds
		}
		out.Jobs[name] = cloned
	}
	return out
}

// cloneAnyMap copies a map of arbitrary values.
func cloneAnyMap(m map[string]any) map[string]any {
	out := make(map[string]any, len(m))
	for k, v := range m {
		switch x := v.(type) {
		case map[string]any:
			out[k] = cloneAnyMap(x)
		case []any:
			items := make([]any, len(x))
			copy(items, x)
			out[k] = items
		default:
			out[k] = v
		}
	}
	return out
}

// expandWorkflowArgs replaces the `${{ args.NAME }}` references of a
// workflow with the given values.
//
// Every other expression is left as it is: it can only be resolved
// once the run knows what the earlier steps produced.
func expandWorkflowArgs(wf *Workflow, args map[string]string) {
	wf.Title = expandArgs(wf.Title, args)
	for name := range wf.Jobs {
		job := wf.Jobs[name]
		job.If = expandArgs(job.If, args)
		for i := range job.Steps {
			step := &job.Steps[i]
			step.If = expandArgs(step.If, args)
			step.Message = expandArgs(step.Message, args)
			step.Shell = expandArgs(step.Shell, args)
			step.Note = expandArgs(step.Note, args)
			step.Use = expandArgs(step.Use, args)
			for j := range step.Allow {
				step.Allow[j] = expandArgs(step.Allow[j], args)
			}
			for k, v := range step.Env {
				step.Env[k] = expandArgs(v, args)
			}
			step.With = expandAnyMap(step.With, args)
		}
		wf.Jobs[name] = job
	}
}

// expandAnyMap replaces the `${{ args.NAME }}` references of every
// string leaf of a map.
func expandAnyMap(m map[string]any, args map[string]string) map[string]any {
	if m == nil {
		return nil
	}
	out := make(map[string]any, len(m))
	for k, v := range m {
		out[k] = expandValue(v, args)
	}
	return out
}

// expandValue replaces the `${{ args.NAME }}` references of a value.
//
// Every shape the runtime understands is walked, so a reference hides
// nowhere.
func expandValue(v any, args map[string]string) any {
	switch x := v.(type) {
	case string:
		return expandArgs(x, args)
	case map[string]any:
		return expandAnyMap(x, args)
	case []any:
		items := make([]any, len(x))
		for i, item := range x {
			items[i] = expandValue(item, args)
		}
		return items
	}
	return v
}

// expandArgs replaces the `${{ args.NAME }}` references in s.
//
// Every other expression is left as it is.
func expandArgs(s string, args map[string]string) string {
	if !strings.Contains(s, "${{") {
		return s
	}
	return exprRef.ReplaceAllStringFunc(s, func(m string) string {
		expr := strings.TrimSpace(m[3 : len(m)-2])
		arg := argExpr.FindStringSubmatch(expr)
		if arg == nil {
			return m
		}
		return args[arg[1]]
	})
}

// describeArgs renders the declared arguments for an error message.
func describeArgs(args []string) string {
	if len(args) == 0 {
		return "none"
	}
	return strings.Join(args, ", ")
}
