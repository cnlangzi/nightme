package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeInspectConfig writes a config file and returns its path.
func writeInspectConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestInspect_MissingFileIsNotAProblem asserts the legal state: a
// user who has not set nightme up yet has no config file, and that is
// not a failure.
func TestInspect_MissingFileIsNotAProblem(t *testing.T) {
	report := Inspect(filepath.Join(t.TempDir(), "absent.yaml"))
	if report.Exists {
		t.Error("Exists = true for a missing file")
	}
	if len(report.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v, want none", report.Diagnostics)
	}
}

// TestInspect_EmptyFileIsValid asserts a zero-byte file is a config
// file with nothing in it — not a broken one.
func TestInspect_EmptyFileIsValid(t *testing.T) {
	report := Inspect(writeInspectConfig(t, ""))
	if !report.Exists {
		t.Error("Exists = false for an empty file")
	}
	if len(report.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v, want none", report.Diagnostics)
	}
}

// TestInspect_SyntaxError pins the parser's message format: the line
// the parser gave up on, and the text without the line prefix. The
// format is part of the yaml.v3 library — the test fails when the
// library changes it instead of the diagnostic quietly pointing at
// the wrong line.
func TestInspect_SyntaxError(t *testing.T) {
	path := writeInspectConfig(t, "primary: claude\nfeishu:\n  app_id: x\n   app_secret: y\n")
	report := Inspect(path)
	if len(report.Diagnostics) == 0 {
		t.Fatal("a syntax error produced no diagnostic")
	}
	d := report.Diagnostics[0]
	if d.Line != 4 {
		t.Errorf("line = %d, want 4 (%s)", d.Line, d.Message)
	}
	if strings.Contains(d.Message, "line ") {
		t.Errorf("message still carries the line prefix: %q", d.Message)
	}
	if !strings.Contains(d.Message, "mapping values are not allowed") {
		t.Errorf("message = %q, want the parser's text", d.Message)
	}
}

// TestInspect_TypeError pins the decoder's message format: one
// diagnostic per wrong value, with the line it is on.
func TestInspect_TypeError(t *testing.T) {
	path := writeInspectConfig(t, "telegram:\n  polling_timeout: \"abc\"\n")
	report := Inspect(path)
	if len(report.Diagnostics) == 0 {
		t.Fatal("a type error produced no diagnostic")
	}
	d := report.Diagnostics[0]
	if d.Line != 2 {
		t.Errorf("line = %d, want 2 (%s)", d.Line, d.Message)
	}
	want := "cannot unmarshal !!str `abc` into int"
	if d.Message != want {
		t.Errorf("message = %q, want %q", d.Message, want)
	}
}

// TestInspect_UnknownKey asserts the typo a user actually makes is
// reported with the place it was made.
func TestInspect_UnknownKey(t *testing.T) {
	path := writeInspectConfig(t, "feishu:\n  app_id: x\n  app_secrett: y\n")
	report := Inspect(path)
	if len(report.Diagnostics) != 1 {
		t.Fatalf("diagnostics = %v, want exactly one", report.Diagnostics)
	}
	d := report.Diagnostics[0]
	if d.Message != `unknown key "feishu.app_secrett"` {
		t.Errorf("message = %q", d.Message)
	}
	if d.Line != 3 {
		t.Errorf("line = %d, want 3", d.Line)
	}
	if d.Column != 3 {
		t.Errorf("column = %d, want 3", d.Column)
	}
}

// TestInspect_UnknownKeyAtEveryLevel asserts the path of an unknown
// key says where it is: at the root, below a section, below a nested
// section and inside a list item.
func TestInspect_UnknownKeyAtEveryLevel(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    string
	}{
		{
			name:    "root",
			content: "foo: bar\n",
			want:    `unknown key "foo"`,
		},
		{
			name:    "section",
			content: "feishu:\n  foo: bar\n",
			want:    `unknown key "feishu.foo"`,
		},
		{
			name:    "nested section",
			content: "me:\n  llm:\n    foo: bar\n",
			want:    `unknown key "me.llm.foo"`,
		},
		{
			name:    "list item",
			content: "agents:\n  - name: cc\n    foo: bar\n",
			want:    `unknown key "agents[0].foo"`,
		},
		{
			name:    "second list item",
			content: "agents:\n  - name: cc\n  - name: codex\n    foo: bar\n",
			want:    `unknown key "agents[1].foo"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			report := Inspect(writeInspectConfig(t, tc.content))
			if len(report.Diagnostics) != 1 {
				t.Fatalf("diagnostics = %v, want exactly one", report.Diagnostics)
			}
			if got := report.Diagnostics[0].Message; got != tc.want {
				t.Errorf("message = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestInspect_KnownKeysAreAccepted asserts every key of the schema is
// a key the check accepts — a config file that names them all has no
// problem to report.
func TestInspect_KnownKeysAreAccepted(t *testing.T) {
	content := `name: macbook
primary: claude
agent_telemetry: false
feishu:
  app_id: x
  app_secret: y
  verification_token: z
  encrypt_key: k
  rate_limit:
    rate_per_sec: 5
    burst: 1
telegram:
  bot_token: t
  polling_timeout: 30
slack:
  bot_token: b
  app_token: a
  stream_throttle_ms: 3000
  rate_limit:
    rate_per_sec: 5
    burst: 1
discord:
  bot_token: b
  application_id: a
  intents: 1
  reconnect_max_backoff_sec: 30
session:
  default_pty_cols: 80
  default_pty_rows: 24
  output_chunk_size: 4096
  output_flush_interval_ms: 200
logging:
  level: info
  file: /tmp/nightme.log
paths:
  data_dir: ~/.nightme
review:
  agent: cc
  timeout: 15m
me:
  llm:
    protocol: messages
    base_url: https://example.com
    model: m
    api_key: k
    api_key_env: E
    max_tokens: 100
    timeout: 30s
    headers:
      X-Anything: v
  decider:
    protocol: decisions
    base_url: https://example.com
    model: d
    api_key: k
    api_key_env: E
    timeout: 30s
    min_confidence: 0.5
    headers:
      X-Anything: v
  agent: cc
  max_judgments: 3
agents:
  - name: cc
    bridge: claude
    command: /usr/local/bin/claude
`
	report := Inspect(writeInspectConfig(t, content))
	if len(report.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v, want none", report.Diagnostics)
	}
}

// TestInspect_FreeKeyMapIsNotChecked asserts a map's keys are the
// user's own — a header name is not a schema key.
func TestInspect_FreeKeyMapIsNotChecked(t *testing.T) {
	path := writeInspectConfig(t, "me:\n  llm:\n    headers:\n      X-Whatever: v\n      Authorization: Bearer x\n")
	report := Inspect(path)
	if len(report.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v, want none", report.Diagnostics)
	}
}

// TestInspect_NullOptionalBlock asserts an absent optional block has
// no keys to check.
func TestInspect_NullOptionalBlock(t *testing.T) {
	path := writeInspectConfig(t, "feishu:\n  rate_limit: null\nslack:\n  rate_limit:\n")
	report := Inspect(path)
	if len(report.Diagnostics) != 0 {
		t.Errorf("diagnostics = %v, want none", report.Diagnostics)
	}
}

// TestInspect_DoesNotWriteTheFile asserts the check leaves the file
// alone: a diagnostic command has no business changing the thing it
// diagnoses.
func TestInspect_DoesNotWriteTheFile(t *testing.T) {
	content := "primary: claude\n"
	path := writeInspectConfig(t, content)
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	Inspect(path)
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("mtime changed: %s -> %s", before.ModTime(), after.ModTime())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(got) != content {
		t.Errorf("content changed: %q", got)
	}
}

// TestInspect_DuplicateKey asserts a key written twice is reported —
// the second one silently wins otherwise, and the file says something
// the daemon does not do.
func TestInspect_DuplicateKey(t *testing.T) {
	path := writeInspectConfig(t, "primary: a\nprimary: b\n")
	report := Inspect(path)
	if len(report.Diagnostics) == 0 {
		t.Fatal("a duplicate key produced no diagnostic")
	}
	d := report.Diagnostics[0]
	if d.Line != 2 {
		t.Errorf("line = %d, want 2 (%s)", d.Line, d.Message)
	}
	if !strings.Contains(d.Message, "already defined") {
		t.Errorf("message = %q, want the decoder's text", d.Message)
	}
}

// TestInspect_ReportsEveryProblem asserts the report carries all of
// them — a user fixes the file in one pass.
func TestInspect_ReportsEveryProblem(t *testing.T) {
	path := writeInspectConfig(t, "foo: bar\nfeishu:\n  app_secrett: y\n")
	report := Inspect(path)
	if len(report.Diagnostics) != 2 {
		t.Fatalf("diagnostics = %v, want two", report.Diagnostics)
	}
	// The problems are ordered by their place in the file.
	if report.Diagnostics[0].Message != `unknown key "foo"` {
		t.Errorf("first diagnostic = %q", report.Diagnostics[0].Message)
	}
	if report.Diagnostics[1].Message != `unknown key "feishu.app_secrett"` {
		t.Errorf("second diagnostic = %q", report.Diagnostics[1].Message)
	}
}
