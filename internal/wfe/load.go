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

// LoadDir reads all `*.yaml` files from dir and returns the parsed
// workflows. Fails if two workflows share a name.
func LoadDir(dir string) ([]*Workflow, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("wfe: glob %s: %w", dir, err)
	}
	var workflows []*Workflow
	seen := map[string]string{} // workflow name -> filename
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, fmt.Errorf("wfe: %s: %w", f, err)
		}
		wf, err := Parse(raw)
		if err != nil {
			return nil, fmt.Errorf("wfe: %s: %w", f, err)
		}
		if prev, ok := seen[wf.Name]; ok {
			return nil, fmt.Errorf("wfe: duplicate workflow name %q in %s and %s", wf.Name, prev, f)
		}
		seen[wf.Name] = f
		workflows = append(workflows, wf)
	}
	return workflows, nil
}

// LoadFile parses and validates a single workflow file.
func LoadFile(path string) (*Workflow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("wfe: read: %w", err)
	}
	return Parse(raw)
}

// Parse parses and validates raw YAML bytes as a workflow.
func Parse(raw []byte) (*Workflow, error) {
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("wfe: yaml: %w", err)
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode {
		if len(doc.Content) == 0 {
			return nil, fmt.Errorf("wfe: the file is empty")
		}
		root = doc.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("wfe: the file is not a workflow: it has no keys")
	}
	if err := checkDocument(root); err != nil {
		return nil, err
	}

	var wf Workflow
	if err := root.Decode(&wf); err != nil {
		return nil, fmt.Errorf("wfe: yaml: %w", err)
	}
	if err := validate(&wf, root); err != nil {
		return nil, err
	}
	return &wf, nil
}

// The keys each level of the schema accepts.
var (
	workflowKeys = keySet("name", "args", "goal", "instruct", "workspaces", "on", "jobs")
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
		return fmt.Errorf("wfe: %w", err)
	}
	for _, e := range entries {
		switch e.key {
		case "steps":
			return fmt.Errorf("wfe: `steps:` is retired (line %d): a workflow keeps its steps in `jobs:`", e.line)
		case "agent":
			return fmt.Errorf("wfe: `agent:` is retired (line %d): a run uses the agent of the chat that started it", e.line)
		}
		if !workflowKeys[e.key] {
			return fmt.Errorf("wfe: unknown key %q (line %d)", e.key, e.line)
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
		return fmt.Errorf("wfe: `args:` must be a list (got %s)", nodeKind(node))
	}
	for _, item := range node.Content {
		entries, err := mappingEntries(item)
		if err != nil {
			return fmt.Errorf("wfe: `args:`: %w", err)
		}
		for _, e := range entries {
			if !argKeys[e.key] {
				return fmt.Errorf("wfe: unknown key %q in an argument (line %d)", e.key, e.line)
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
		return fmt.Errorf("wfe: `on:` must be a mapping (got %s)", nodeKind(node))
	}
	entries, err := mappingEntries(node)
	if err != nil {
		return fmt.Errorf("wfe: `on:`: %w", err)
	}
	for _, e := range entries {
		if !triggerKeys[e.key] {
			return fmt.Errorf("wfe: unknown key %q in `on:` (line %d)", e.key, e.line)
		}
	}
	return nil
}

// checkJobs validates the jobs of a workflow and their steps.
func checkJobs(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("wfe: `jobs:` must be a mapping (got %s)", nodeKind(node))
	}
	entries, err := mappingEntries(node)
	if err != nil {
		return fmt.Errorf("wfe: `jobs:`: %w", err)
	}
	for _, e := range entries {
		jobEntries, err := mappingEntries(e.val)
		if err != nil {
			return fmt.Errorf("wfe: job %q: %w", e.key, err)
		}
		for _, je := range jobEntries {
			if !jobKeys[je.key] {
				return fmt.Errorf("wfe: unknown key %q in job %q (line %d)", je.key, e.key, je.line)
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
		return fmt.Errorf("wfe: job %q: `steps:` must be a list (got %s)", job, nodeKind(node))
	}
	for _, item := range node.Content {
		entries, err := mappingEntries(item)
		if err != nil {
			return fmt.Errorf("wfe: job %q: `steps:`: %w", job, err)
		}
		for _, e := range entries {
			switch e.key {
			case "run":
				return fmt.Errorf("wfe: `run:` is retired (line %d): use `shell:`", e.line)
			case "prompt":
				return fmt.Errorf("wfe: `prompt:` is retired (line %d): use `message:`", e.line)
			case "agent":
				return fmt.Errorf("wfe: `agent:` is retired (line %d): a run uses the agent of the chat that started it", e.line)
			}
			if !stepKeys[e.key] {
				return fmt.Errorf("wfe: unknown key %q in a step of job %q (line %d)", e.key, job, e.line)
			}
		}
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
	if wf.Name == "" {
		return fmt.Errorf("wfe: name is required")
	}
	if !hasKey(root, "instruct") {
		wf.Instruct = true
	}

	seenArgs := map[string]bool{}
	for i, arg := range wf.Args {
		if strings.TrimSpace(arg.Name) == "" {
			return fmt.Errorf("wfe: %s: argument %d has no name", wf.Name, i+1)
		}
		if seenArgs[arg.Name] {
			return fmt.Errorf("wfe: %s: duplicate argument %q", wf.Name, arg.Name)
		}
		seenArgs[arg.Name] = true
	}

	if len(wf.Jobs) == 0 {
		return fmt.Errorf("wfe: %s: jobs is required (at least one)", wf.Name)
	}
	jobNames := map[string]bool{}
	for name := range wf.Jobs {
		jobNames[name] = true
	}
	for name, job := range wf.Jobs {
		for _, need := range job.Needs {
			if !jobNames[need] {
				return fmt.Errorf("wfe: %s: job %q needs unknown job %q", wf.Name, name, need)
			}
		}
		if job.Iterations != nil && *job.Iterations < 0 {
			return fmt.Errorf("wfe: %s: job %q: iterations must be >= 0", wf.Name, name)
		}
	}
	if len(wf.JobOrder()) != len(wf.Jobs) {
		return fmt.Errorf("wfe: %s: the jobs need each other in a circle", wf.Name)
	}
	seenSteps := map[string]string{} // step id -> the job it belongs to
	for _, name := range sortedJobNames(wf.Jobs) {
		job := wf.Jobs[name]
		if len(job.Steps) == 0 {
			return fmt.Errorf("wfe: %s: job %q has no steps", wf.Name, name)
		}
		for i := range job.Steps {
			step := &job.Steps[i]
			if err := validateStep(step); err != nil {
				return fmt.Errorf("wfe: %s: job %q step %d: %w", wf.Name, name, i+1, err)
			}
			if step.ID == "" {
				step.ID = syntheticStepID(name, i+1)
			}
			// The run state is keyed by step id, so two steps may
			// not share one.
			if prev, ok := seenSteps[step.ID]; ok {
				return fmt.Errorf("wfe: %s: the step id %q is used by job %q and job %q", wf.Name, step.ID, prev, name)
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
				return fmt.Errorf("wfe: %s: unknown argument %q (declared: %s)", wf.Name, arg[1], describeArgs(wf.ArgNames()))
			}
		}
	}
	return nil
}

// workflowTexts returns every string of a workflow that may hold an
// expression.
func workflowTexts(wf *Workflow) []string {
	texts := []string{wf.Goal}
	for _, name := range wf.JobOrder() {
		job := wf.Jobs[name]
		texts = append(texts, job.If)
		for _, step := range job.Steps {
			texts = append(texts, step.If, step.Message, step.Shell, step.Note)
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
		Name:       wf.Name,
		Goal:       wf.Goal,
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
	wf.Goal = expandArgs(wf.Goal, args)
	for name := range wf.Jobs {
		job := wf.Jobs[name]
		job.If = expandArgs(job.If, args)
		for i := range job.Steps {
			step := &job.Steps[i]
			step.If = expandArgs(step.If, args)
			step.Message = expandArgs(step.Message, args)
			step.Shell = expandArgs(step.Shell, args)
			step.Note = expandArgs(step.Note, args)
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
