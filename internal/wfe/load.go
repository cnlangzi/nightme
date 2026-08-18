package wfe

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"gopkg.in/yaml.v3"
)

// LoadDir reads all `*.yaml` files from dir and returns parsed
// workflows. Fails if two workflows share a name.
func LoadDir(dir string) ([]*Workflow, error) {
	files, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return nil, fmt.Errorf("wfe: glob %s: %w", dir, err)
	}
	var out []*Workflow
	seen := map[string]string{} // name -> filename
	for _, f := range files {
		wf, err := LoadFile(f)
		if err != nil {
			return nil, fmt.Errorf("wfe: %s: %w", f, err)
		}
		if prev, ok := seen[wf.Name]; ok {
			return nil, fmt.Errorf("wfe: duplicate workflow name %q in %s and %s", wf.Name, prev, f)
		}
		seen[wf.Name] = f
		out = append(out, wf)
	}
	return out, nil
}

// LoadFile parses and validates a single workflow file.
func LoadFile(path string) (*Workflow, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("wfe: read: %w", err)
	}
	return Parse(raw)
}

// Parse parses and validates raw YAML bytes.
func Parse(raw []byte) (*Workflow, error) {
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
