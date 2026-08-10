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

// listFixture creates a v1.2 pair of on-disk stores (chat_sessions.json
// + agent_sessions.json) and seeds them with a running Claude session
// and a detached Codex session. Exited rows are added by callers via
// addExitedToFixture. The returned *registry.AgentSessionFile lets
// callers assert on-disk side effects after the loader runs.
func listFixture(t *testing.T) (*registry.ChatSessionFile, *registry.AgentSessionFile, *registry.AgentSessionEntry, *registry.AgentSessionEntry) {
	t.Helper()
	dir := t.TempDir()
	csFile, err := registry.OpenChatSessionFile(filepath.Join(dir, "chat_sessions.json"))
	if err != nil {
		t.Fatalf("OpenChatSessionFile: %v", err)
	}
	asFile, err := registry.OpenAgentSessionFile(filepath.Join(dir, "agent_sessions.json"))
	if err != nil {
		t.Fatalf("OpenAgentSessionFile: %v", err)
	}

	now := time.Date(2026, 7, 31, 10, 30, 0, 0, time.UTC)

	// ChatSession #1 (Claude, running).
	cs1 := &registry.ChatSessionEntry{
		ID:            "cs_oc_run",
		ChatID:        "oc_run",
		SelectedCwd:     "/home/devin/code/bailing",
		SelectedAgent:   "claude",
		PrimaryAgent:  "claude",
		AgentSessionIDs: []string{"as_run_1"},
		CreatedAt:     now,
		LastInteractionAt: now,
	}
	if err := csFile.Upsert(cs1); err != nil {
		t.Fatalf("Upsert cs1: %v", err)
	}

	// AgentSession #1 (running, with a SessionID).
	asRun := &registry.AgentSessionEntry{
		ID:            "as_run_1",
		ChatSessionID: cs1.ID,
		Agent:         "claude",
		Cwd:           cs1.SelectedCwd,
		PID:           12345,
		Status:        registry.StatusRunning,
		SessionID:      "sess-claude-abc",
		CreatedAt:     now,
		LastRunAt:     now,
	}
	if err := asFile.Upsert(asRun); err != nil {
		t.Fatalf("Upsert asRun: %v", err)
	}

	// ChatSession #2 (Codex, detached).
	cs2 := &registry.ChatSessionEntry{
		ID:            "cs_oc_det",
		ChatID:        "oc_det",
		SelectedCwd:     "/home/devin/code/nightme",
		SelectedAgent:   "codex",
		PrimaryAgent:  "codex",
		AgentSessionIDs: []string{"as_det_1"},
		CreatedAt:     now.Add(-time.Hour),
		LastInteractionAt: now.Add(-30 * time.Minute),
	}
	if err := csFile.Upsert(cs2); err != nil {
		t.Fatalf("Upsert cs2: %v", err)
	}

	// AgentSession #2 (detached, no SessionID).
	asDet := &registry.AgentSessionEntry{
		ID:            "as_det_1",
		ChatSessionID: cs2.ID,
		Agent:         "codex",
		Cwd:           cs2.SelectedCwd,
		Status:        registry.StatusDetached,
		CreatedAt:     now.Add(-time.Hour),
		LastRunAt:     now.Add(-30 * time.Minute),
	}
	if err := asFile.Upsert(asDet); err != nil {
		t.Fatalf("Upsert asDet: %v", err)
	}

	return csFile, asFile, asRun, asDet
}

// addExitedToFixture adds an exited AgentSession to the fixture so
// callers can verify GC behaviour. chatSessionID is the parent
// ChatSession's ID (not the ChatID), e.g. "cs_oc_run".
func addExitedToFixture(t *testing.T, asFile *registry.AgentSessionFile, id, chatSessionID, cwd string) {
	t.Helper()
	now := time.Now()
	code := 0
	if err := asFile.Upsert(&registry.AgentSessionEntry{
		ID:            id,
		ChatSessionID: chatSessionID,
		Agent:         "codex",
		Cwd:           cwd,
		Status:        registry.StatusExited,
		ExitCode:      &code,
		CreatedAt:     now.Add(-2 * time.Hour),
		LastRunAt:     now.Add(-time.Hour),
	}); err != nil {
		t.Fatalf("Upsert exited: %v", err)
	}
}

// TestListTextFormat exercises the default table renderer against a
// populated v1.2 store. Verifies the header is present, every alive
// row appears, and the resume column shows the captured id.
func TestListTextFormat(t *testing.T) {
	csFile, asFile, asRun, asDet := listFixture(t)

	rows, _, err := loadListRows(csFile, asFile, false, false)
	if err != nil {
		t.Fatalf("loadListRows: %v", err)
	}

	var buf bytes.Buffer
	printListTable(&buf, rows)
	out := buf.String()

	for _, want := range []string{"CHAT", "AGENT", "PID", "STATUS", "WORKSPACE", "STARTED", "SID", "RESUME"} {
		if !strings.Contains(out, want) {
			t.Errorf("table missing header %q\n%s", want, out)
		}
	}
	if !strings.Contains(out, asRun.ID) {
		t.Errorf("table missing running session ID\n%s", out)
	}
	if !strings.Contains(out, "oc_run") {
		t.Errorf("table missing chat ID\n%s", out)
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
	if !strings.Contains(out, asRun.SessionID) {
		t.Errorf("table missing resume id for Claude session\n%s", out)
	}
	if !strings.Contains(out, asDet.ID) {
		t.Errorf("table missing detached session ID\n%s", out)
	}
	if !strings.Contains(out, "detached") {
		t.Errorf("table missing detached status\n%s", out)
	}
	// Detached row has no resume id → "-" placeholder.
	if !strings.Contains(out, "-") {
		t.Errorf("table missing '-' placeholder for empty resume id\n%s", out)
	}
}

// TestListEmptyAlwaysPrintsHeader documents the empty-registry
// behavior: a header is emitted so users see "no sessions" clearly.
func TestListEmptyAlwaysPrintsHeader(t *testing.T) {
	var buf bytes.Buffer
	printListTable(&buf, nil)
	out := buf.String()
	for _, want := range []string{"CHAT", "AGENT", "STATUS"} {
		if !strings.Contains(out, want) {
			t.Errorf("empty table missing header %q", want)
		}
	}
	if strings.Contains(out, "as_") {
		t.Errorf("empty table unexpectedly contains a session ID: %q", out)
	}
}

// TestListJSONFormat verifies the --json output is a valid JSON
// array of joined rows and contains the resume id payload.
func TestListJSONFormat(t *testing.T) {
	csFile, asFile, asRun, _ := listFixture(t)

	rows, _, err := loadListRows(csFile, asFile, false, false)
	if err != nil {
		t.Fatalf("loadListRows: %v", err)
	}

	var buf bytes.Buffer
	if err := printListJSON(&buf, rows); err != nil {
		t.Fatalf("printListJSON: %v", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(buf.String()), "[") {
		t.Errorf("JSON output is not an array: %s", buf.String())
	}
	for _, want := range []string{
		`"agentSessionId"`, `"chatId"`, `"cwd"`, `"status"`, `"resumeId"`,
	} {
		if !strings.Contains(buf.String(), want) {
			t.Errorf("JSON output missing %q\n%s", want, buf.String())
		}
	}
	if !strings.Contains(buf.String(), asRun.SessionID) {
		t.Errorf("JSON output missing resume id value\n%s", buf.String())
	}
}

// TestListCmdJSONFlag wires the --json flag through cobra and verifies
// it toggles the JSON renderer. The command is invoked in-process; we
// only need the flag plumbing, not a real session manager.
func TestListCmdJSONFlag(t *testing.T) {
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

// TestListCmdAllFlag covers the --all flag and exercises the "show
// exited + skip GC" path.
func TestListCmdAllFlag(t *testing.T) {
	cmd := newListCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ParseFlags([]string{"--all"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	f, err := cmd.Flags().GetBool("all")
	if err != nil {
		t.Fatalf("GetBool(all): %v", err)
	}
	if !f {
		t.Errorf("--all did not set the flag")
	}
}

// TestListCmdKeepExitedFlag covers --keep-exited.
func TestListCmdKeepExitedFlag(t *testing.T) {
	cmd := newListCmd()
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	if err := cmd.ParseFlags([]string{"--keep-exited"}); err != nil {
		t.Fatalf("ParseFlags: %v", err)
	}
	f, err := cmd.Flags().GetBool("keep-exited")
	if err != nil {
		t.Fatalf("GetBool(keep-exited): %v", err)
	}
	if !f {
		t.Errorf("--keep-exited did not set the flag")
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

// TestChatSessionsPathResolvesRelative ensures chatSessionsPath is
// joined under DataDir when it is not absolute.
func TestChatSessionsPathResolvesRelative(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Paths.DataDir = dir

	got, err := chatSessionsPath(cfg)
	if err != nil {
		t.Fatalf("chatSessionsPath: %v", err)
	}
	want := filepath.Join(dir, "chat_sessions.json")
	if got != want {
		t.Errorf("chatSessionsPath = %q, want %q", got, want)
	}
}

// TestAgentSessionsPathResolvesRelative ensures agentSessionsPath is
// joined under DataDir when it is not absolute.
func TestAgentSessionsPathResolvesRelative(t *testing.T) {
	dir := t.TempDir()
	cfg := &config.Config{}
	cfg.Paths.DataDir = dir

	got, err := agentSessionsPath(cfg)
	if err != nil {
		t.Fatalf("agentSessionsPath: %v", err)
	}
	want := filepath.Join(dir, "agent_sessions.json")
	if got != want {
		t.Errorf("agentSessionsPath = %q, want %q", got, want)
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
	if got.Info().Mode != 2 {
		// ModePTY = 2 in the agent package. The exact integer is
		// not stable across reorders; pin the symbolic value here
		// so we notice if someone changes the iota ordering.
		t.Errorf("registered agent mode = %d, want ModePTY(2)", got.Info().Mode)
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
