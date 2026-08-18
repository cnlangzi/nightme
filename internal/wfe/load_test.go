package wfe

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParse_Valid(t *testing.T) {
	yaml := `
name: pr-reviewer
workspaces: [~/work/nightme]
agent: codex
on:
  mention:
    commands: [review]
jobs:
  review:
    steps:
      - id: review
        prompt: "Review ${{ event.title }}"
        if: ${{ success() }}
        env:
          GH_TOKEN: secret
`
	wf, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if wf.Name != "pr-reviewer" {
		t.Errorf("Name = %q, want pr-reviewer", wf.Name)
	}
	if len(wf.Workspaces) != 1 {
		t.Errorf("Workspaces = %v, want 1", wf.Workspaces)
	}
	if wf.Agent != "codex" {
		t.Errorf("Agent = %q, want codex", wf.Agent)
	}
	if len(wf.On.Mention.Commands) != 1 || wf.On.Mention.Commands[0] != "review" {
		t.Errorf("Mention.Commands = %v, want [review]", wf.On.Mention.Commands)
	}
	if len(wf.Jobs) != 1 {
		t.Errorf("Jobs = %v, want 1 job", wf.Jobs)
	}
	step := wf.Jobs["review"].Steps[0]
	if step.Prompt == "" {
		t.Error("step.Prompt empty")
	}
	if step.ID != "review" {
		t.Errorf("step.ID = %q, want review", step.ID)
	}
	if step.Env["GH_TOKEN"] != "secret" {
		t.Errorf("step.Env[GH_TOKEN] = %q, want secret", step.Env["GH_TOKEN"])
	}
}

func TestParse_MissingName(t *testing.T) {
	yaml := `
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps: [{run: echo}]
`
	_, err := Parse([]byte(yaml))
	if err == nil {
		t.Error("expected error for missing name")
	}
}

func TestParse_MissingWorkspaces(t *testing.T) {
	yaml := `
name: x
on: { mention: {} }
jobs:
  main:
    steps: [{run: echo}]
`
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "workspaces") {
		t.Errorf("expected workspaces error, got %v", err)
	}
}

func TestParse_NoTriggers(t *testing.T) {
	yaml := `
name: x
workspaces: [a]
jobs:
  main:
    steps: [{run: echo}]
`
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "on:") {
		t.Errorf("expected on: error, got %v", err)
	}
}

func TestParse_StepKindExclusive(t *testing.T) {
	yaml := `
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps:
      - id: s
        run: echo
        prompt: hi
`
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "exactly one") {
		t.Errorf("expected exactly-one error, got %v", err)
	}
}

func TestParse_AgentOnlyForPrompt(t *testing.T) {
	yaml := `
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps:
      - id: s
        run: echo
        agent: codex
`
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "agent") {
		t.Errorf("expected agent error, got %v", err)
	}
}

func TestParse_UnknownNeed(t *testing.T) {
	yaml := `
name: x
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    needs: [nonexistent]
    steps: [{run: echo}]
`
	_, err := Parse([]byte(yaml))
	if err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Errorf("expected unknown-need error, got %v", err)
	}
}

func TestParse_DedupWorkspaces(t *testing.T) {
	yaml := `
name: x
workspaces: [a, b, a, c, b]
on: { mention: {} }
jobs:
  main:
    steps: [{run: echo}]
`
	wf, err := Parse([]byte(yaml))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if len(wf.Workspaces) != 3 {
		t.Errorf("Workspaces = %v, want 3 (deduped)", wf.Workspaces)
	}
}

func TestLoadDir_DuplicateName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"a.yaml", "b.yaml"} {
		os.WriteFile(filepath.Join(dir, name), []byte(`
name: dup
workspaces: [a]
on: { mention: {} }
jobs:
  main:
    steps: [{run: echo}]
`), 0644)
	}
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("expected duplicate-name error, got %v", err)
	}
}

func TestTopoSort(t *testing.T) {
	wf := &Workflow{
		Jobs: map[string]Job{
			"a": {Steps: []Step{{ID: "a1", Run: "x"}}},
			"b": {Needs: []string{"a"}, Steps: []Step{{ID: "b1", Run: "x"}}},
			"c": {Needs: []string{"b"}, Steps: []Step{{ID: "c1", Run: "x"}}},
		},
	}
	order := topoSort(wf.Jobs)
	if len(order) != 3 {
		t.Fatalf("order = %v, want 3 entries", order)
	}
	posA, posB, posC := -1, -1, -1
	for i, n := range order {
		switch n {
		case "a":
			posA = i
		case "b":
			posB = i
		case "c":
			posC = i
		}
	}
	if !(posA < posB && posB < posC) {
		t.Errorf("order %v does not respect deps a < b < c", order)
	}
}
