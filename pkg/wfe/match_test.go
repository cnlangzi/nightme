package wfe

import "testing"

// The trigger machinery belongs to the `on:` block: it is parsed and
// displayed today, and it is what a scheduler starts a workflow from.

func TestMatch(t *testing.T) {
	wf, err := Parse([]byte(`
title: x
on:
  pull_request:
    branches: [main]
    events: [opened]
  branch:
    patterns: [release/*]
    events: [pushed]
  issue:
    events: [opened, commented]
  mention:
    commands: [review, fix]
  schedule:
    - cron: '0 9 * * *'
jobs:
  main:
    steps: [{id: s, message: x}]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}

	tests := []struct {
		name string
		ev   Event
		want bool
	}{
		{"schedule match", Event{Kind: "schedule", Data: map[string]any{"cron": "0 9 * * *"}}, true},
		{"schedule no match", Event{Kind: "schedule", Data: map[string]any{"cron": "0 0 * * *"}}, false},
		{"PR match", Event{Kind: "pull_request", Data: map[string]any{"branch": "main", "action": "opened"}}, true},
		{"PR wrong branch", Event{Kind: "pull_request", Data: map[string]any{"branch": "dev", "action": "opened"}}, false},
		{"branch glob match", Event{Kind: "branch", Data: map[string]any{"name": "release/v1.0", "action": "pushed"}}, true},
		{"branch no match", Event{Kind: "branch", Data: map[string]any{"name": "feature/x", "action": "pushed"}}, false},
		{"issue match", Event{Kind: "issue", Data: map[string]any{"action": "commented"}}, true},
		{"issue wrong action", Event{Kind: "issue", Data: map[string]any{"action": "closed"}}, false},
		{"mention match", Event{Kind: "mention", Data: map[string]any{"command": "review"}}, true},
		{"mention wrong cmd", Event{Kind: "mention", Data: map[string]any{"command": "deploy"}}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Match(wf, tt.ev); got != tt.want {
				t.Errorf("Match = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestMatchMentionNoCommands(t *testing.T) {
	wf, err := Parse([]byte(`
title: x
on:
  mention: {}
jobs:
  main:
    steps: [{id: s, message: x}]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	// No commands whitelist = any mention fires
	if !Match(wf, Event{Kind: "mention", Data: map[string]any{"command": "anything"}}) {
		t.Error("any mention should fire when no commands whitelist")
	}
}

func TestMatchSchedule(t *testing.T) {
	wf, err := Parse([]byte(`
title: x
on:
  schedule:
    - cron: '0 9 * * *'
    - cron: '0 18 * * 5'
jobs:
  main:
    steps: [{id: s, message: x}]
`))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !Match(wf, Event{Kind: "schedule", Data: map[string]any{"cron": "0 9 * * *"}}) {
		t.Error("first cron should match")
	}
	if !Match(wf, Event{Kind: "schedule", Data: map[string]any{"cron": "0 18 * * 5"}}) {
		t.Error("second cron should match")
	}
	if Match(wf, Event{Kind: "schedule", Data: map[string]any{"cron": "0 0 * * *"}}) {
		t.Error("non-listed cron should not match")
	}
	// PR event should not match when only schedule is configured
	if Match(wf, Event{Kind: "pull_request"}) {
		t.Error("PR event should not match a schedule-only workflow")
	}
}

func TestMatchGlob(t *testing.T) {
	tests := []struct {
		pattern, s string
		want       bool
	}{
		{"*", "anything", true},
		{"release/*", "release/v1.0", true},
		{"release/*", "feature/x", false},
		{"foo", "foo", true},
		{"foo", "bar", false},
		{"*foo", "xfoo", true},
		{"*foo", "xfo", false},
		{"foo*", "foobar", true},
		{"foo*", "bar", false},
		{"a*b", "axxxb", true},
		{"a*b", "axb", true},
		{"a*b", "ab", true},
		{"a*b", "ax", false},
	}
	for _, tt := range tests {
		t.Run(tt.pattern+"/"+tt.s, func(t *testing.T) {
			if got := matchGlob(tt.pattern, tt.s); got != tt.want {
				t.Errorf("matchGlob(%q, %q) = %v, want %v", tt.pattern, tt.s, got, tt.want)
			}
		})
	}
}
