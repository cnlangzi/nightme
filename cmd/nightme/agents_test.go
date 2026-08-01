package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// TestPrintAgentsTable_Empty confirms the empty-registry case prints
// just the header line (no footer), so the user knows the command
// ran instead of guessing.
func TestPrintAgentsTable_Empty(t *testing.T) {
	var buf bytes.Buffer
	printAgentsTable(&buf, nil, "claude")

	out := buf.String()
	for _, want := range []string{"NAME", "COMMAND", "ARGS"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing header %q: %q", want, out)
		}
	}
	if strings.Contains(out, "(default:") {
		t.Errorf("empty table should not print a default footer: %q", out)
	}
}

// TestPrintAgentsTable_Basic covers the happy path: three rows
// aligned under the header, footer reports the default agent.
func TestPrintAgentsTable_Basic(t *testing.T) {
	rows := []agentRow{
		{Name: "claude", Command: "claude"},
		{Name: "codex", Command: "codex-acp"},
		{Name: "opencode", Command: "opencode", Args: []string{"acp"}},
	}

	var buf bytes.Buffer
	printAgentsTable(&buf, rows, "claude")

	out := buf.String()
	for _, want := range []string{"claude", "codex", "opencode", "codex-acp", "acp", "(default: claude)"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing %q\n%s", want, out)
		}
	}
}

// TestPrintAgentsTable_LongArgsTruncated verifies that an absurdly
// long argv gets ellipsized to keep the right-hand columns aligned.
func TestPrintAgentsTable_LongArgsTruncated(t *testing.T) {
	rows := []agentRow{
		{
			Name:    "verbose",
			Command: "vc",
			Args:    []string{"--long-flag-with-many-dashes", "--another", "--third"},
		},
	}

	var buf bytes.Buffer
	printAgentsTable(&buf, rows, "")

	out := buf.String()
	if !strings.Contains(out, "…") {
		t.Errorf("long args should be truncated with ellipsis: %q", out)
	}
	// Footer must be suppressed when defaultName is empty.
	if strings.Contains(out, "(default:") {
		t.Errorf("empty defaultName should suppress footer: %q", out)
	}
}

// TestPrintAgentsJSON verifies the JSON wire format matches the
// documented field order and omits a wrapper envelope.
func TestPrintAgentsJSON(t *testing.T) {
	rows := []agentRow{
		{Name: "claude", Command: "claude"},
		{Name: "opencode", Command: "opencode", Args: []string{"acp"}},
	}

	var buf bytes.Buffer
	if err := printAgentsJSON(&buf, rows); err != nil {
		t.Fatalf("printAgentsJSON: %v", err)
	}

	var got []agentRow
	if err := json.Unmarshal(buf.Bytes(), &got); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if len(got) != 2 {
		t.Fatalf("decoded %d rows, want 2", len(got))
	}
	if got[1].Name != "opencode" || len(got[1].Args) != 1 || got[1].Args[0] != "acp" {
		t.Errorf("second row mismatch: %+v", got[1])
	}
}

// TestQuoteArgs verifies the helper that flattens argv for the table
// column. Empty input returns "" so the cell stays blank instead of
// printing "[]".
func TestQuoteArgs(t *testing.T) {
	cases := []struct {
		in   []string
		want string
	}{
		{nil, ""},
		{[]string{}, ""},
		{[]string{"acp"}, "acp"},
		{[]string{"--foo", "bar"}, "--foo bar"},
	}
	for _, c := range cases {
		if got := quoteArgs(c.in); got != c.want {
			t.Errorf("quoteArgs(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestAgentsCmd_Help verifies the cobra wiring exposes the subcommand
// under the root with --json flag.
func TestAgentsCmd_Help(t *testing.T) {
	cmd := newAgentsCmd()
	if cmd.Use != "agents" {
		t.Errorf("Use = %q, want agents", cmd.Use)
	}
	if cmd.RunE == nil {
		t.Errorf("RunE is nil — cobra will no-op on the command")
	}
	if f := cmd.Flags().Lookup("json"); f == nil {
		t.Errorf("--json flag missing")
	}
}