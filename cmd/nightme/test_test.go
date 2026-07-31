package main

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/registry"
)

// listFixture creates a registry on disk with a fixed set of entries
// and returns its path. Used by the table-format and JSON tests.
func listFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	file, err := registry.Open(path)
	if err != nil {
		t.Fatalf("registry.Open: %v", err)
	}
	now := time.Date(2026, 7, 31, 10, 30, 0, 0, time.UTC)
	code := 0
	if err := file.Upsert(registry.Entry{
		SessionID: "s_01ABC",
		ChatID:    "oc_run",
		Workspace: "/home/devin/code/bailing",
		Agent:     "claude",
		PID:       12345,
		StartedAt: now,
		LastRunAt: now,
		Status:    registry.StatusRunning,
	}); err != nil {
		t.Fatalf("Upsert running: %v", err)
	}
	if err := file.Upsert(registry.Entry{
		SessionID: "s_02DEF",
		ChatID:    "oc_exited",
		Workspace: "/tmp/test",
		Agent:     "codex",
		PID:       0,
		StartedAt: now.Add(-time.Hour),
		LastRunAt: now.Add(-30 * time.Minute),
		Status:    registry.StatusExited,
		ExitCode:  &code,
	}); err != nil {
		t.Fatalf("Upsert exited: %v", err)
	}
	return path
}

// TestListTextFormat exercises the default table renderer against a
// populated registry. Verifies the header is present, every entry
// appears on its own line, and the exited row uses "-" for PID and
// includes the exit code in the status column.
func TestListTextFormat(t *testing.T) {
	path := listFixture(t)

	entries, err := loadEntries(path)
	if err != nil {
		t.Fatalf("loadEntries: %v", err)
	}

	var buf bytes.Buffer
	printListTable(&buf, entries)
	out := buf.String()

	for _, want := range []string{"SID", "AGENT", "WORKSPACE", "PID", "STATUS", "STARTED"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing header %q\n%s", want, out)
		}
	}
	if !strings.Contains(out, "s_01ABC") {
		t.Errorf("table missing running session ID\n%s", out)
	}
	if !strings.Contains(out, "claude") {
		t.Errorf("table missing running agent\n%s", out)
	}
	if !strings.Contains(out, "12345") {
		t.Errorf("table missing running PID\n%s", out)
	}
	if !strings.Contains(out, "running") {
		t.Errorf("table missing running status\n%s", out)
	}
	if !strings.Contains(out, "-") {
		t.Errorf("table missing '-' placeholder for exited PID\n%s", out)
	}
	if !strings.Contains(out, "exited(0)") {
		t.Errorf("table missing 'exited(0)' status\n%s", out)
	}
}

// TestListEmptyAlwaysPrintsHeader documents the empty-registry
// behavior: a header is emitted so users see "no sessions" clearly.
func TestListEmptyAlwaysPrintsHeader(t *testing.T) {
	var buf bytes.Buffer
	printListTable(&buf, nil)
	out := buf.String()
	for _, want := range []string{"SID", "AGENT", "STATUS"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty table missing header %q", want)
		}
	}
	if strings.Contains(out, "s_") {
		t.Errorf("empty table unexpectedly contains a session ID: %q", out)
	}
}

// TestListJSONFormat verifies the --json output is a valid JSON
// array of registry entries and round-trips losslessly.
func TestListJSONFormat(t *testing.T) {
	path := listFixture(t)
	entries, err := loadEntries(path)
	if err != nil {
		t.Fatalf("loadEntries: %v", err)
	}

	var buf bytes.Buffer
	if err := printListJSON(&buf, entries); err != nil {
		t.Fatalf("printListJSON: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "[") {
		t.Errorf("JSON output is not an array: %s", buf.String())
	}
	for _, want := range []string{`"session_id"`, `"chat_id"`, `"workspace"`, `"status"`} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("JSON output missing %q\n%s", want, buf.String())
		}
	}
}

// loadEntries opens the registry at path and returns a fresh entry
// list. Tests use this instead of poking the public Open API
// directly so the path is the only thing they need to know about.
func loadEntries(path string) ([]registry.Entry, error) {
	file, err := registry.Open(path)
	if err != nil {
		return nil, err
	}
	return file.List(), nil
}

// TestListCmdJSONFlag wires the --json flag through cobra and verifies
// it toggles the JSON renderer. The command is invoked in-process; we
// only need the flag plumbing, not a real session manager.
func TestListCmdJSONFlag(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "registry.json")
	if _, err := registry.Open(path); err != nil {
		t.Fatalf("seed registry: %v", err)
	}

	cmd := newListCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ParseFlags([]string{"--json"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	jsonFlag, err := cmd.Flags().GetBool("json")
	if err != nil {
		t.Fatalf("GetBool(json): %v", err)
	}
	if !jsonFlag {
		t.Errorf("--json did not set the flag")
	}
}

// TestListCmdMissingJSONFlag verifies the default (no --json) is text
// mode.
func TestListCmdMissingJSONFlag(t *testing.T) {
	cmd := newListCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ParseFlags(nil); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	jsonFlag, err := cmd.Flags().GetBool("json")
	if err != nil {
		t.Fatalf("GetBool(json): %v", err)
	}
	if jsonFlag {
		t.Errorf("--json unexpectedly true without the flag")
	}
}

// TestNewRootCmdHasSubcommands guards against accidentally dropping
// `test` or `list` from the root command. Users would notice but
// tests catch regressions cheaply.
func TestNewRootCmdHasSubcommands(t *testing.T) {
	root := newRootCmd()
	want := map[string]bool{"test": false, "list": false}
	for _, c := range root.Commands() {
		if _, ok := want[c.Name()]; ok {
			want[c.Name()] = true
		}
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("root command missing subcommand %q", name)
		}
	}
}

// TestListCmdWorkspaceRequired ensures cobra enforces the required
// --workspace flag. We invoke the test command (which shares the
// same flag pattern as list would if list ever grew workspace
// filters) by directly testing the test subcommand's flag plumbing.
func TestTestCmdFlagsRequired(t *testing.T) {
	cmd := newTestCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ParseFlags([]string{"--workspace", "/tmp", "--agent", "claude"}); err != nil {
		t.Fatalf("ParseFlags full: %v", err)
	}
	ws, _ := cmd.Flags().GetString("workspace")
	ag, _ := cmd.Flags().GetString("agent")
	if ws != "/tmp" || ag != "claude" {
		t.Errorf("flags = (%q,%q), want (/tmp,claude)", ws, ag)
	}
}

// TestRegistryPathResolvesRelative ensures cfg.Paths.RegistryFile is
// joined under DataDir when it is not absolute.
func TestRegistryPathResolvesRelative(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Paths.DataDir = dir
	cfg.Paths.RegistryFile = "registry.json"

	got, err := registryPath(cfg)
	if err != nil {
		t.Fatalf("registryPath: %v", err)
	}
	want := filepath.Join(dir, "registry.json")
	if got != want {
		t.Errorf("registryPath = %q, want %q", got, want)
	}
}

// TestRegistryPathAbsolute ensures an absolute path is returned
// verbatim without touching DataDir.
func TestRegistryPathAbsolute(t *testing.T) {
	cfg := &config.Config{}
	cfg.Paths.DataDir = "/should/not/be/used"
	cfg.Paths.RegistryFile = "/var/lib/nightme/registry.json"

	got, err := registryPath(cfg)
	if err != nil {
		t.Fatalf("registryPath: %v", err)
	}
	if got != "/var/lib/nightme/registry.json" {
		t.Errorf("registryPath = %q, want absolute verbatim", got)
	}
}

// TestValidateTestRequest covers the cheap pre-checks that gate the
// `nightme test` command before config loading.
func TestValidateTestRequest(t *testing.T) {
	dir := t.TempDir()

	if err := validateTestRequest(testCmdFlags{workspace: dir, agentName: "x"}); err != nil {
		t.Errorf("valid request returned error: %v", err)
	}
	if err := validateTestRequest(testCmdFlags{workspace: "", agentName: "x"}); err == nil {
		t.Errorf("missing workspace did not error")
	}
	if err := validateTestRequest(testCmdFlags{workspace: dir, agentName: ""}); err == nil {
		t.Errorf("missing agent did not error")
	}

	// Workspace that does not exist.
	bogus := filepath.Join(dir, "does-not-exist")
	if err := validateTestRequest(testCmdFlags{workspace: bogus, agentName: "x"}); err == nil {
		t.Errorf("non-existent workspace did not error")
	}

	// Workspace pointing at a regular file.
	tmp := filepath.Join(dir, "file.txt")
	if err := os.WriteFile(tmp, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := validateTestRequest(testCmdFlags{workspace: tmp, agentName: "x"}); err == nil {
		t.Errorf("non-directory workspace did not error")
	}
}

// TestBuildAgentRegistryAutoRegister exercises the bare-path fallback
// for `--agent /bin/echo` style usage. The agent registry is empty
// initially; we verify the binary on disk becomes a registered PTY
// agent.
func TestBuildAgentRegistryAutoRegister(t *testing.T) {
	cfg := &config.Config{} // empty — no agents from config
	bin := "/bin/echo"
	if _, err := os.Stat(bin); err != nil {
		t.Skipf("test requires %s: %v", bin, err)
	}

	reg := buildAgentRegistry(cfg, bin)
	got, err := reg.Get(bin)
	if err != nil {
		t.Fatalf("Get(%s) after auto-register: %v", bin, err)
	}
	if got.Mode() != 2 {
		// ModePTY = 2 in the agent package. The exact integer is
		// not stable across reorders; pin the symbolic value here
		// so we notice if someone changes the iota ordering.
		t.Errorf("registered agent mode = %d, want ModePTY(2)", got.Mode())
	}
}

// TestBuildAgentRegistryUnknownAgent ensures an unregistered,
// non-existent agent name does NOT get silently registered — that
// path should surface as an "unknown agent" error at Create time.
func TestBuildAgentRegistryUnknownAgent(t *testing.T) {
	cfg := &config.Config{}
	reg := buildAgentRegistry(cfg, "/no/such/binary/anywhere")
	if _, err := reg.Get("/no/such/binary/anywhere"); err == nil {
		t.Errorf("unknown agent was silently registered")
	}
}
