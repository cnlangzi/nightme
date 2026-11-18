package wfe

import (
	"encoding/json"
	"strings"
	"testing"
)

// agentChainYAML is a workflow whose step names a chain of agents.
const agentChainYAML = `
title: ship
args:
  - name: agent
    default: omp
jobs:
  ship:
    steps:
      - id: review
        message: "/review"
        agents:
          - mode: use
            names: [omp, claude]
`

// TestParse_TheAgentChain — a step names the agents it runs, and the
// chain is parsed.
func TestParse_TheAgentChain(t *testing.T) {
	wf := mustParse(t, agentChainYAML)

	step := wf.Jobs["ship"].Steps[0]
	if len(step.Agents) != 1 {
		t.Fatalf("Agents = %v, want one entry", step.Agents)
	}
	entry := step.Agents[0]
	if entry.Mode != AgentModeUse {
		t.Errorf("Mode = %q, want %q", entry.Mode, AgentModeUse)
	}
	if len(entry.Names) != 2 || entry.Names[0] != "omp" || entry.Names[1] != "claude" {
		t.Errorf("Names = %v, want omp and claude", entry.Names)
	}
}

// TestParse_TheAgentChainErrors — the shape of the chain is checked
// when the file is loaded, and every problem says what is wrong.
func TestParse_TheAgentChainErrors(t *testing.T) {
	tests := []struct {
		name    string
		src     string
		wantErr string
	}{
		{
			name: "the agents of a shell step",
			src: `
title: x
jobs:
  main:
    steps:
      - shell: make build
        agents:
          - mode: use
            names: [omp]
`,
			wantErr: "the agents are for a message step",
		},
		{
			name: "the agents of a wait step",
			src: `
title: x
jobs:
  main:
    steps:
      - wait: 5m
        agents:
          - mode: use
            names: [omp]
`,
			wantErr: "the agents are for a message step",
		},
		{
			name: "a mode that names no mode",
			src: `
title: x
jobs:
  main:
    steps:
      - message: "/review"
        agents:
          - mode: switch
            names: [omp]
`,
			wantErr: "the agents entry 1 needs a mode: use or args",
		},
		{
			name: "no mode at all",
			src: `
title: x
jobs:
  main:
    steps:
      - message: "/review"
        agents:
          - names: [omp]
`,
			wantErr: "the agents entry 1 needs a mode: use or args",
		},
		{
			name: "no agent names",
			src: `
title: x
jobs:
  main:
    steps:
      - message: "/review"
        agents:
          - mode: use
            names: []
`,
			wantErr: "the agents entry 1 has no agent names",
		},
		{
			name: "an empty agent name",
			src: `
title: x
jobs:
  main:
    steps:
      - message: "/review"
        agents:
          - mode: use
            names: ["  "]
`,
			wantErr: "the agents entry 1 names an empty agent",
		},
		{
			name: "the args mode on a prose message",
			src: `
title: x
jobs:
  main:
    steps:
      - message: review the changes
        agents:
          - mode: args
            names: [omp]
`,
			wantErr: "the agents entry 1 uses the args mode: the message must be a slash command",
		},
		{
			name: "the second entry is the broken one",
			src: `
title: x
jobs:
  main:
    steps:
      - message: review the changes
        agents:
          - mode: use
            names: [omp]
          - mode: args
            names: [claude]
`,
			wantErr: "the agents entry 2 uses the args mode: the message must be a slash command",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src))
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("error = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

// TestParse_TheArgsModeAcceptsAMessageThatHoldsAnExpression — the
// message of the args mode may be an expression: what it becomes is
// checked when the step runs.
func TestParse_TheArgsModeAcceptsAMessageThatHoldsAnExpression(t *testing.T) {
	wf := mustParse(t, `
title: x
args:
  - name: cmd
    default: "/review"
jobs:
  main:
    steps:
      - message: "${{ args.cmd }}"
        agents:
          - mode: args
            names: [omp]
`)
	if len(wf.Jobs["main"].Steps[0].Agents) != 1 {
		t.Fatalf("Agents = %v, want one entry", wf.Jobs["main"].Steps[0].Agents)
	}
}

// TestBind_TheMessageOfTheArgsModeKeepsItsExpression — the message of
// the args mode is checked when the file is loaded: a message that
// holds an expression is accepted there, and the step checks what the
// expression became when it runs.
func TestBind_TheMessageOfTheArgsModeKeepsItsExpression(t *testing.T) {
	wf := mustParse(t, `
title: x
args:
  - name: cmd
    default: "review the changes"
jobs:
  main:
    steps:
      - message: "${{ args.cmd }}"
        agents:
          - mode: args
            names: [omp]
`)

	bound, err := Bind(wf, nil)
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if got := bound.Jobs["main"].Steps[0].Message; got != "review the changes" {
		t.Errorf("Message = %q, want the value of the argument", got)
	}
}

// TestParse_AnAgentNameMayComeFromAnArgument — the names of a chain
// are strings of the workflow, and an argument they name is expanded
// when the run binds it.
func TestParse_AnAgentNameMayComeFromAnArgument(t *testing.T) {
	wf := mustParse(t, `
title: x
args:
  - name: agent
    default: omp
jobs:
  main:
    steps:
      - message: "/review"
        agents:
          - mode: args
            names: ["${{ args.agent }}"]
`)

	bound, err := Bind(wf, map[string]string{"agent": "claude"})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	names := bound.Jobs["main"].Steps[0].Agents[0].Names
	if len(names) != 1 || names[0] != "claude" {
		t.Errorf("Names = %v, want claude", names)
	}
	// The source keeps its own name: the bound workflow is a copy.
	if got := wf.Jobs["main"].Steps[0].Agents[0].Names[0]; got != "${{ args.agent }}" {
		t.Errorf("the source name = %q, want the expression", got)
	}
}

// TestParse_AnUndeclaredArgumentInAnAgentNameIsRefused — the name of
// an agent is a string of the workflow: an argument it names has to be
// declared.
func TestParse_AnUndeclaredArgumentInAnAgentNameIsRefused(t *testing.T) {
	_, err := Parse([]byte(`
title: x
jobs:
  main:
    steps:
      - message: "/review"
        agents:
          - mode: use
            names: ["${{ args.nope }}"]
`))
	if err == nil {
		t.Fatal("the undeclared argument was accepted")
	}
	if !strings.Contains(err.Error(), `unknown argument "nope"`) {
		t.Errorf("error = %v, want it to name the argument", err)
	}
}

// TestTheAgentChainSurvivesTheJSONRoundTrip — the chain is part of
// the step, and a step that goes through JSON keeps it.
func TestTheAgentChainSurvivesTheJSONRoundTrip(t *testing.T) {
	wf := mustParse(t, agentChainYAML)

	raw, err := json.Marshal(wf)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var back Workflow
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	step := back.Jobs["ship"].Steps[0]
	if len(step.Agents) != 1 {
		t.Fatalf("Agents = %v, want one entry", step.Agents)
	}
	if step.Agents[0].Mode != AgentModeUse {
		t.Errorf("Mode = %q, want %q", step.Agents[0].Mode, AgentModeUse)
	}
	if len(step.Agents[0].Names) != 2 {
		t.Errorf("Names = %v, want two names", step.Agents[0].Names)
	}
}

// TestValidate_TheUnknownKeyOfAnAgentEntryIsReported — the known keys
// of an entry are read from the schema, the way every other key is.
func TestValidate_TheUnknownKeyOfAnAgentEntryIsReported(t *testing.T) {
	diags := Validate([]byte(`
title: x
jobs:
  main:
    steps:
      - message: "/review"
        agents:
          - mode: use
            agent: omp
`))
	if len(diags) == 0 {
		t.Fatal("the unknown key of the entry was accepted")
	}
	found := false
	for _, d := range diags {
		if strings.Contains(d.Message, `unknown key "agent"`) {
			found = true
		}
	}
	if !found {
		t.Errorf("diagnostics = %v, want the unknown key of the entry", diags)
	}
}

// TestValidate_TheKnownKeysOfAnAgentEntryAreAccepted — the entry
// carries a mode and names, and nothing else.
func TestValidate_TheKnownKeysOfAnAgentEntryAreAccepted(t *testing.T) {
	diags := Validate([]byte(agentChainYAML))
	if len(diags) != 0 {
		t.Fatalf("diagnostics = %v, want none", diags)
	}
}
