package wfe

import (
	"fmt"
	"io/fs"
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
// The name is the name the file would have provided: it is what the
// operator asks for, so the failure is reported against it.
type Failure struct {
	// Name is the name of the workflow the file would have provided.
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
// Both `*.yaml` and `*.yml` are read, at any depth, and a workflow's
// identity is its path relative to dir without the extension. The
// workflows that loaded come back with one failure per name that is
// unusable: a broken file does not take the workflows beside it down
// with it.
func LoadDir(dir string) ([]*Workflow, []Failure) {
	paths, err := WorkflowPaths(dir)
	if err != nil {
		return nil, []Failure{{Path: dir, Err: fmt.Errorf("the workflows directory %s cannot be read: %w", dir, err)}}
	}
	claims := map[string][]string{}  // name -> the files that claim it
	loaded := map[string]*Workflow{} // name -> the workflow that loaded
	broken := map[string]Failure{}   // name -> why the file did not load
	for _, path := range paths {
		name := WorkflowName(dir, path)
		if name == "" {
			continue
		}
		claims[name] = append(claims[name], path)
		wf, err := LoadFile(dir, path)
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
// The directory is read at any depth: a workflow file is a `*.yaml` or
// a `*.yml` file, wherever it sits under dir. The reserved directories
// — the workflows directory's own `actions` and `runs` children — are
// never descended into.
//
// A directory that cannot be read is an error: the caller says so
// instead of reporting an empty directory.
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
	var out []string
	walkErr := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			switch filepath.Ext(entry.Name()) {
			case ".yaml", ".yml":
				out = append(out, path)
			}
			return nil
		}
		if path == dir || !isReservedChild(dir, path) {
			return nil
		}
		return fs.SkipDir
	})
	if walkErr != nil {
		return nil, walkErr
	}
	sort.Strings(out)
	return out, nil
}

// IsReservedDir reports whether name is a reserved directory of the
// workflows directory.
//
// The reserved directories hold nightme's own state: `actions` holds
// the user's action scripts and `runs` holds the run state. Neither is
// a workflow namespace, so a file under them is never a workflow.
func IsReservedDir(name string) bool {
	switch name {
	case "actions", "runs":
		return true
	}
	return false
}

// isReservedChild reports whether path is one of the workflows
// directory's own children whose name is reserved.
//
// The reserved directories are the workflows directory's own children:
// `actions` holds the user's action scripts and `runs` holds the run
// state. A directory of the same name deeper down is an ordinary
// namespace.
func isReservedChild(dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	if strings.Contains(rel, string(filepath.Separator)) {
		return false
	}
	return IsReservedDir(filepath.Base(rel))
}

// WorkflowPathsByName returns the workflow files of dir whose name is
// name.
//
// The name is the path of a workflow under dir, without the extension
// (see WorkflowName). A name two files claim is ambiguous: both files
// come back, so the caller can report the clash.
func WorkflowPathsByName(dir, name string) ([]string, error) {
	paths, err := WorkflowPaths(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, path := range paths {
		if WorkflowName(dir, path) == name {
			out = append(out, path)
		}
	}
	return out, nil
}

// BaseName returns the name of a file without its extension.
//
// It names the file, not the workflow: the workflow's name is its path
// under the workflows directory (see WorkflowName).
func BaseName(path string) string {
	base := filepath.Base(path)
	return strings.TrimSuffix(base, filepath.Ext(base))
}

// WorkflowName returns the name of the workflow file at path: its path
// relative to root, without the extension.
//
// "nightme/coder.yml" under root is the workflow "nightme/coder". The
// separator is always "/", so a workflow's name is the same on every
// platform. An empty root means the file's own directory.
func WorkflowName(root, path string) string {
	if strings.TrimSpace(root) == "" {
		root = filepath.Dir(path)
	}
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = filepath.Base(path)
	}
	rel = filepath.ToSlash(rel)
	if rel == ".." || strings.HasPrefix(rel, "../") {
		// The file is not under the root: it has no path-relative
		// name, so it keeps the name of its file.
		return BaseName(path)
	}
	name := strings.TrimSuffix(rel, filepath.Ext(rel))
	if name == "" || name == "." {
		return ""
	}
	return name
}

// LoadFile parses and validates a single workflow file.
//
// The file's path under root is the workflow's identity and the
// workflow's title states what it is for. The identity is set on the
// workflow; the file does not carry it.
func LoadFile(root, path string) (*Workflow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("wfe: %s: %w", filepath.Base(path), err)
	}
	wf, err := parse(raw)
	if err != nil {
		return nil, fmt.Errorf("wfe: %s: %w", filepath.Base(path), err)
	}
	name := WorkflowName(root, path)
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
// from. A workflow that does not validate is a *ValidationError: it
// carries every problem that was found.
func parse(raw []byte) (*Workflow, error) {
	wf, diags := collect(raw, false)
	if len(diags) > 0 {
		return nil, &ValidationError{Diagnostics: diags}
	}
	return wf, nil
}

// entry is one key/value pair of a YAML mapping.
type entry struct {
	key string
	// keyNode is the node of the key.
	keyNode *yaml.Node
	// val is the value of the key.
	val *yaml.Node
}

// requireMapping checks that a node holds a mapping.
func requireMapping(node *yaml.Node) error {
	if node == nil {
		return fmt.Errorf("expected a mapping, got nothing")
	}
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("expected a mapping, got %s", nodeKind(node))
	}
	return nil
}

// mappingEntries returns the key/value pairs of a YAML mapping.
//
// The node must be a mapping: requireMapping says so.
func mappingEntries(node *yaml.Node) ([]entry, error) {
	if err := requireMapping(node); err != nil {
		return nil, err
	}
	return mappingPairs(node), nil
}

// mappingPairs returns the key/value pairs of a YAML mapping.
//
// A node that is not a mapping has no pairs.
func mappingPairs(node *yaml.Node) []entry {
	if node == nil || node.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]entry, 0, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		out = append(out, entry{
			key:     node.Content[i].Value,
			keyNode: node.Content[i],
			val:     node.Content[i+1],
		})
	}
	return out
}

// sequenceItems returns the items of a YAML sequence.
//
// A node that is not a sequence has no items.
func sequenceItems(node *yaml.Node) []*yaml.Node {
	if node == nil || node.Kind != yaml.SequenceNode {
		return nil
	}
	return node.Content
}

// mappingValue returns the value node of a key in a mapping node.
func mappingValue(node *yaml.Node, key string) *yaml.Node {
	for _, e := range mappingPairs(node) {
		if e.key == key {
			return e.val
		}
	}
	return nil
}

// jobNode returns the mapping node of a job.
func jobNode(root *yaml.Node, name string) *yaml.Node {
	return mappingValue(mappingValue(root, "jobs"), name)
}

// stepNode returns the mapping node of a step of a job.
func stepNode(root *yaml.Node, job string, index int) *yaml.Node {
	steps := mappingValue(jobNode(root, job), "steps")
	items := sequenceItems(steps)
	if index < 0 || index >= len(items) {
		return nil
	}
	return items[index]
}

// argNode returns the mapping node of an argument.
func argNode(root *yaml.Node, index int) *yaml.Node {
	items := sequenceItems(mappingValue(root, "args"))
	if index < 0 || index >= len(items) {
		return nil
	}
	return items[index]
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
	if s.Wait != "" {
		if _, err := time.ParseDuration(s.Wait); err != nil {
			return fmt.Errorf("wait: %q is not a duration", s.Wait)
		}
	}
	return validateStepAgents(s)
}

// validateStepAgents checks the chain of agents a step declares.
//
// The agents are for a message step: an agent is what does the work,
// and only a message reaches an agent.
func validateStepAgents(s *Step) error {
	if len(s.Agents) == 0 {
		return nil
	}
	if s.Kind() != StepKindMessage {
		return fmt.Errorf("the agents are for a message step")
	}
	for i, entry := range s.Agents {
		if err := validateAgentChain(i+1, entry); err != nil {
			return err
		}
	}
	return nil
}

// validateAgentChain checks one entry of a step's chain of agents.
//
// The position of the entry names it: the diagnostic says which entry
// it is about.
func validateAgentChain(position int, entry AgentChain) error {
	switch entry.Mode {
	case AgentModeUse, AgentModeArgs:
	default:
		return fmt.Errorf("the agents entry %d needs a mode: use or args", position)
	}
	if len(entry.Names) == 0 {
		return fmt.Errorf("the agents entry %d has no agent names", position)
	}
	for _, name := range entry.Names {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("the agents entry %d names an empty agent", position)
		}
	}
	return nil
}

// validateStepMessage checks the message of a step against its chain.
//
// The message of the args mode has to be a command: the agent goes on
// the command line, and only a command takes one.
//
// The check belongs to the load path: a message that holds an
// expression is accepted there, and the step checks what the
// expression became when it runs.
func validateStepMessage(s *Step) error {
	for i, entry := range s.Agents {
		if entry.Mode != AgentModeArgs {
			continue
		}
		if !commandMessage(s.Message) {
			return fmt.Errorf("the agents entry %d uses the args mode: the message must be a slash command", i+1)
		}
	}
	return nil
}

// commandMessage reports whether a message is a slash command.
//
// A message that starts with an expression is one: the expression is
// what the message becomes when the step runs, and the command
// dispatcher sees that.
func commandMessage(message string) bool {
	text := strings.TrimSpace(message)
	return strings.HasPrefix(text, "/") || strings.HasPrefix(text, "${{")
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

// workflowTexts returns every string of a workflow that may hold an
// expression.
func workflowTexts(wf *Workflow) []string {
	texts := []string{wf.Title}
	for _, name := range wf.JobOrder() {
		job := wf.Jobs[name]
		texts = append(texts, job.If)
		for _, step := range job.Steps {
			texts = append(texts, step.If, step.Message, step.Shell, step.Note, step.Use)
			for _, v := range step.Env {
				texts = append(texts, v)
			}
			texts = append(texts, mapValues(step.With)...)
			for _, entry := range step.Agents {
				texts = append(texts, entry.Names...)
			}
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
			if len(step.Agents) > 0 {
				steps[i].Agents = cloneAgentChains(step.Agents)
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

// cloneAgentChains copies the chain of agents of a step.
func cloneAgentChains(chains []AgentChain) []AgentChain {
	out := make([]AgentChain, len(chains))
	for i, chain := range chains {
		out[i] = AgentChain{
			Mode:  chain.Mode,
			Names: append([]string(nil), chain.Names...),
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
			for k, v := range step.Env {
				step.Env[k] = expandArgs(v, args)
			}
			step.With = expandAnyMap(step.With, args)
			for i := range step.Agents {
				for j, name := range step.Agents[i].Names {
					step.Agents[i].Names[j] = expandArgs(name, args)
				}
			}
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
