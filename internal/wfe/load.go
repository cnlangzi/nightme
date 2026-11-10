package wfe

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// LoadDir reads all `*.yaml` files from dir and returns the parsed
// workflows and the parsed playbooks. A file with top-level `steps`
// is a playbook; a file with `jobs` is a DAG workflow. Fails if two
// workflows share a name.
func LoadDir(dir string) ([]*Workflow, []*Playbook, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, nil, fmt.Errorf("wfe: glob %s: %w", dir, err)
	}
	var workflows []*Workflow
	var playbooks []*Playbook
	seen := map[string]string{}          // workflow name -> filename
	seenPlaybooks := map[string]string{} // playbook name -> filename
	for _, f := range files {
		raw, err := os.ReadFile(f)
		if err != nil {
			return nil, nil, fmt.Errorf("wfe: %s: %w", f, err)
		}
		wf, pb, err := parseFile(raw)
		if err != nil {
			return nil, nil, fmt.Errorf("wfe: %s: %w", f, err)
		}
		if pb != nil {
			if prev, ok := seenPlaybooks[pb.Name]; ok {
				return nil, nil, fmt.Errorf("wfe: duplicate playbook name %q in %s and %s", pb.Name, prev, f)
			}
			seenPlaybooks[pb.Name] = f
			playbooks = append(playbooks, pb)
			continue
		}
		if prev, ok := seen[wf.Name]; ok {
			return nil, nil, fmt.Errorf("wfe: duplicate workflow name %q in %s and %s", wf.Name, prev, f)
		}
		seen[wf.Name] = f
		workflows = append(workflows, wf)
	}
	return workflows, playbooks, nil
}

// LoadFile parses and validates a single workflow file.
func LoadFile(path string) (*Workflow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("wfe: read: %w", err)
	}
	return Parse(raw)
}

// Parse parses and validates raw YAML bytes as a DAG workflow.
func Parse(raw []byte) (*Workflow, error) {
	kind, err := fileKind(raw)
	if err != nil {
		return nil, err
	}
	if kind == kindPlaybook {
		return nil, fmt.Errorf("wfe: this file has top-level steps: it is a playbook, not a workflow")
	}
	return parseWorkflow(raw)
}

// parseWorkflow parses and validates raw YAML bytes as a DAG
// workflow.
func parseWorkflow(raw []byte) (*Workflow, error) {
	var wf Workflow
	if err := yaml.Unmarshal(raw, &wf); err != nil {
		return nil, fmt.Errorf("wfe: yaml: %w", err)
	}
	if err := validate(&wf); err != nil {
		return nil, err
	}
	wf.jobOrder = topoSort(wf.Jobs)
	return &wf, nil
}

// ParsePlaybook parses and validates raw YAML bytes as a playbook.
func ParsePlaybook(raw []byte) (*Playbook, error) {
	var pb Playbook
	if err := yaml.Unmarshal(raw, &pb); err != nil {
		return nil, fmt.Errorf("wfe: yaml: %w", err)
	}
	if err := validatePlaybook(&pb); err != nil {
		return nil, err
	}
	return &pb, nil
}

// parseFile parses raw YAML as a playbook or as a DAG workflow.
// Exactly one of the two results is non-nil.
func parseFile(raw []byte) (*Workflow, *Playbook, error) {
	kind, err := fileKind(raw)
	if err != nil {
		return nil, nil, err
	}
	if kind == kindPlaybook {
		pb, err := ParsePlaybook(raw)
		if err != nil {
			return nil, nil, err
		}
		return nil, pb, nil
	}
	wf, err := parseWorkflow(raw)
	if err != nil {
		return nil, nil, err
	}
	return wf, nil, nil
}

// The kinds of file the loader understands.
const (
	kindWorkflow = "workflow"
	kindPlaybook = "playbook"
)

// fileKind tells whether raw holds a playbook or a DAG workflow. A
// file with top-level `steps` is a playbook, a file with `jobs` is a
// workflow; a file with both is an error, so a workflow never
// disappears because of a stray key.
func fileKind(raw []byte) (string, error) {
	var probe struct {
		Jobs  yaml.Node `yaml:"jobs"`
		Steps yaml.Node `yaml:"steps"`
	}
	if err := yaml.Unmarshal(raw, &probe); err != nil {
		return "", fmt.Errorf("wfe: yaml: %w", err)
	}
	hasSteps := !probe.Steps.IsZero()
	hasJobs := !probe.Jobs.IsZero()
	switch {
	case hasSteps && hasJobs:
		return "", fmt.Errorf("wfe: the file has both steps and jobs: it is a playbook or a workflow, not both")
	case hasSteps:
		return kindPlaybook, nil
	default:
		return kindWorkflow, nil
	}
}

func validate(wf *Workflow) error {
	if wf.Name == "" {
		return fmt.Errorf("wfe: name is required")
	}
	if len(wf.Workspaces) == 0 {
		return fmt.Errorf("wfe: %s: workspaces is required (at least one)", wf.Name)
	}
	// Dedupe workspaces.
	seen := map[string]bool{}
	deduped := wf.Workspaces[:0]
	for _, w := range wf.Workspaces {
		if !seen[w] {
			seen[w] = true
			deduped = append(deduped, w)
		}
	}
	wf.Workspaces = deduped

	if len(wf.Jobs) == 0 {
		return fmt.Errorf("wfe: %s: jobs is required (at least one)", wf.Name)
	}
	jobNames := map[string]bool{}
	for name := range wf.Jobs {
		jobNames[name] = true
	}
	for name, job := range wf.Jobs {
		if len(job.Steps) == 0 {
			return fmt.Errorf("wfe: %s: job %q has no steps", wf.Name, name)
		}
		for _, need := range job.Needs {
			if !jobNames[need] {
				return fmt.Errorf("wfe: %s: job %q needs unknown job %q", wf.Name, name, need)
			}
		}
		for i, step := range job.Steps {
			if err := validateStep(&step); err != nil {
				return fmt.Errorf("wfe: %s: job %q step %d: %w", wf.Name, name, i, err)
			}
		}
	}
	// Ensure the trigger has at least one of the five kinds.
	if wf.On.Schedule == nil && wf.On.PullRequest == nil &&
		wf.On.Branch == nil && wf.On.Issue == nil && wf.On.Mention == nil {
		return fmt.Errorf("wfe: %s: on: must specify at least one trigger (schedule / pull_request / branch / issue / mention)", wf.Name)
	}
	// If Workflow.Agent is set, it's a default for steps that don't
	// specify their own agent. (No validation needed here — step
	// resolution happens at Tick time.)

	if wf.Worker < 0 {
		return fmt.Errorf("wfe: %s: worker must be >= 0", wf.Name)
	}
	return nil
}

func validateStep(s *Step) error {
	n := 0
	if s.Run != "" {
		n++
	}
	if s.Prompt != "" {
		n++
	}
	if s.Use != "" {
		n++
	}
	if n != 1 {
		return fmt.Errorf("exactly one of run / prompt / use must be set (got %d)", n)
	}
	if s.Prompt != "" && s.Agent == "" {
		// OK — agent can come from workflow-level default at Tick time
	}
	if s.Agent != "" && s.Prompt == "" {
		return fmt.Errorf("agent is only valid for prompt steps")
	}
	if s.Shell != "" && s.Run == "" {
		return fmt.Errorf("shell is only valid for run steps")
	}
	return nil
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

// UnmarshalYAML implements yaml.Unmarshaler. It fills in the two
// defaults a playbook may leave out: max_actions and instruct.
func (p *Playbook) UnmarshalYAML(value *yaml.Node) error {
	var raw struct {
		Name       string         `yaml:"name"`
		Args       []string       `yaml:"args"`
		Goal       string         `yaml:"goal"`
		MaxActions *int           `yaml:"max_actions"`
		Instruct   *bool          `yaml:"instruct"`
		Steps      []PlaybookStep `yaml:"steps"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	p.Name = raw.Name
	p.Args = raw.Args
	p.Goal = raw.Goal
	p.MaxActions = DefaultMaxActions
	if raw.MaxActions != nil {
		if *raw.MaxActions < 1 {
			return fmt.Errorf("max_actions must be >= 1")
		}
		p.MaxActions = *raw.MaxActions
	}
	p.Instruct = true
	if raw.Instruct != nil {
		p.Instruct = *raw.Instruct
	}
	p.Steps = raw.Steps
	return nil
}

// UnmarshalYAML implements yaml.Unmarshaler. A step is either a plain
// string — shorthand for `run` — or a mapping.
func (s *PlaybookStep) UnmarshalYAML(value *yaml.Node) error {
	if value.Kind == yaml.ScalarNode {
		var run string
		if err := value.Decode(&run); err != nil {
			return err
		}
		s.Run = run
		return nil
	}
	var raw struct {
		Run   string   `yaml:"run"`
		Note  string   `yaml:"note"`
		Allow []string `yaml:"allow"`
		Max   *int     `yaml:"max"`
	}
	if err := value.Decode(&raw); err != nil {
		return err
	}
	s.Run = raw.Run
	s.Note = raw.Note
	s.Allow = raw.Allow
	if raw.Max != nil {
		if *raw.Max < 1 {
			return fmt.Errorf("max must be >= 1")
		}
		s.Max = *raw.Max
	}
	return nil
}

func validatePlaybook(pb *Playbook) error {
	if pb.Name == "" {
		return fmt.Errorf("wfe: playbook name is required")
	}
	if len(pb.Steps) == 0 {
		return fmt.Errorf("wfe: %s: steps is required (at least one)", pb.Name)
	}
	for i, step := range pb.Steps {
		if strings.TrimSpace(step.Run) == "" {
			return fmt.Errorf("wfe: %s: step %d: run is required", pb.Name, i+1)
		}
	}
	return nil
}

// exprRef matches a `${{ ... }}` expression.
var exprRef = regexp.MustCompile(`\$\{\{(.*?)\}\}`)

// argExpr matches an `${{ args.NAME }}` reference.
var argExpr = regexp.MustCompile(`^\s*args\.([A-Za-z0-9_-]+)\s*$`)

// Bind expands `${{ args.x }}` in the playbook with the given values
// and returns the bound playbook; the source playbook is left
// untouched. An unknown argument — one the playbook does not declare,
// or one no value was given for — is an error.
//
// A playbook is a static document: the arguments are its only
// variable part. Any other expression is rejected instead of being
// resolved to a constant or to nothing, so a command never silently
// turns into a different one.
func Bind(pb *Playbook, args map[string]string) (*Playbook, error) {
	if pb == nil {
		return nil, fmt.Errorf("wfe: playbook is nil")
	}
	declared := make(map[string]bool, len(pb.Args))
	for _, a := range pb.Args {
		declared[a] = true
	}
	given := make([]string, 0, len(args))
	for name := range args {
		given = append(given, name)
	}
	sort.Strings(given)
	for _, name := range given {
		if !declared[name] {
			return nil, fmt.Errorf("wfe: %s: unknown argument %q (declared: %s)", pb.Name, name, describeArgs(pb.Args))
		}
	}
	for _, a := range pb.Args {
		if _, ok := args[a]; !ok {
			return nil, fmt.Errorf("wfe: %s: missing argument %q", pb.Name, a)
		}
	}

	out := &Playbook{
		Name:       pb.Name,
		Args:       append([]string(nil), pb.Args...),
		Goal:       pb.Goal,
		MaxActions: pb.MaxActions,
		Instruct:   pb.Instruct,
		Steps:      make([]PlaybookStep, len(pb.Steps)),
	}
	for i, step := range pb.Steps {
		out.Steps[i] = PlaybookStep{
			Run:  step.Run,
			Note: step.Note,
			Max:  step.Max,
		}
		if len(step.Allow) > 0 {
			out.Steps[i].Allow = make([]string, len(step.Allow))
			for j, a := range step.Allow {
				out.Steps[i].Allow[j] = a
			}
		}
	}
	if err := checkExpressions(pb, declared, playbookTexts(out)); err != nil {
		return nil, err
	}

	// EvalString resolves plain substitutions only; it never reaches
	// for the runtime.
	ec := ExprCtx{Args: args}
	out.Goal = EvalString(out.Goal, ec, nil)
	for i := range out.Steps {
		out.Steps[i].Run = EvalString(out.Steps[i].Run, ec, nil)
		out.Steps[i].Note = EvalString(out.Steps[i].Note, ec, nil)
		for j, a := range out.Steps[i].Allow {
			out.Steps[i].Allow[j] = EvalString(a, ec, nil)
		}
	}
	// A bound step may have lost its command to an empty argument.
	if err := validatePlaybook(out); err != nil {
		return nil, err
	}
	return out, nil
}

// checkExpressions reports the first expression that is not a
// reference to a declared argument.
func checkExpressions(pb *Playbook, declared map[string]bool, texts []string) error {
	for _, text := range texts {
		for _, m := range exprRef.FindAllStringSubmatch(text, -1) {
			expr := strings.TrimSpace(m[1])
			arg := argExpr.FindStringSubmatch(expr)
			if arg == nil {
				return fmt.Errorf("wfe: %s: unsupported expression %q: only ${{ args.NAME }} is supported", pb.Name, "${{ "+expr+" }}")
			}
			if !declared[arg[1]] {
				return fmt.Errorf("wfe: %s: unknown argument %q (declared: %s)", pb.Name, arg[1], describeArgs(pb.Args))
			}
		}
	}
	return nil
}

// playbookTexts returns every string of a playbook that may hold an
// expression.
func playbookTexts(pb *Playbook) []string {
	texts := []string{pb.Goal}
	for _, step := range pb.Steps {
		texts = append(texts, step.Run, step.Note)
		texts = append(texts, step.Allow...)
	}
	return texts
}

// describeArgs renders the declared arguments for an error message.
func describeArgs(args []string) string {
	if len(args) == 0 {
		return "none"
	}
	return strings.Join(args, ", ")
}
