package gtw

import (
	"github.com/cnlangzi/nightme/internal/messages"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
)

// --- Load() ---

// withTempHome redirects $HOME to a fresh temp dir for the duration
// of the test, so Load()'s os.UserHomeDir-based discovery doesn't
// see the real user's yml. Returns the temp dir path; caller is
// responsible for the cleanup (defer os.RemoveAll).
//
// On Windows, os.UserHomeDir() reads %USERPROFILE% — not $HOME.
// We set both so Load() finds the temp dir regardless of platform.
// The unset at test teardown restores both env vars automatically
// (t.Setenv semantics).
func withTempHome(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("USERPROFILE", dir)
	return dir
}

// writeYml writes content to <home>/.nightme/gtw.yml. Panics on
// write failure (test setup error, not under test).
func writeHomeYml(t *testing.T, home, content string) {
	t.Helper()
	dir := filepath.Join(home, ".nightme")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "gtw.yml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
}

func TestLoad_MissingFile_Silent(t *testing.T) {
	withTempHome(t) // empty home, no .nightme/gtw.yml

	cfg, notes := Load()
	if !cfgIsZero(cfg) {
		t.Fatalf("expected zero Config, got %+v", cfg)
	}
	if notes.HasWarnings() {
		t.Fatalf("missing file should be silent, got warnings: %v", notes.Warnings)
	}
}

func TestLoad_ValidYml(t *testing.T) {
	home := withTempHome(t)
	writeHomeYml(t, home, `
fix:
  agent: pi
  hooks:
    before:
      - codegraph init
    after:
      - echo done
push:
  agent: claude
`)

	cfg, notes := Load()
	if notes.HasWarnings() {
		t.Fatalf("valid yml should produce no warnings, got: %v", notes.Warnings)
	}
	if cfg.Fix.Agent != "pi" {
		t.Errorf("Fix.Agent = %q, want pi", cfg.Fix.Agent)
	}
	if len(cfg.Fix.Hooks.Before) != 1 || cfg.Fix.Hooks.Before[0].Run != "codegraph init" {
		t.Errorf("Fix.Hooks.Before not parsed: %+v", cfg.Fix.Hooks.Before)
	}
	if len(cfg.Fix.Hooks.After) != 1 || cfg.Fix.Hooks.After[0].Run != "echo done" {
		t.Errorf("Fix.Hooks.After not parsed: %+v", cfg.Fix.Hooks.After)
	}
	if cfg.Push.Agent != "claude" {
		t.Errorf("Push.Agent = %q, want claude", cfg.Push.Agent)
	}
}

func TestLoad_ExplicitShellType(t *testing.T) {
	home := withTempHome(t)
	writeHomeYml(t, home, `
fix:
  hooks:
    before:
      - type: shell
        run: echo hi
      - type: shell
        run: |
          multi
          line
`)

	cfg, notes := Load()
	if notes.HasWarnings() {
		t.Fatalf("expected no warnings, got: %v", notes.Warnings)
	}
	if len(cfg.Fix.Hooks.Before) != 2 {
		t.Fatalf("len(Hooks.Before) = %d, want 2", len(cfg.Fix.Hooks.Before))
	}
	for i, h := range cfg.Fix.Hooks.Before {
		if h.Type != "shell" {
			t.Errorf("[%d] Type = %q, want shell", i, h.Type)
		}
	}
	if !strings.HasPrefix(cfg.Fix.Hooks.Before[1].Run, "multi\nline") {
		t.Errorf("[1].Run = %q, want multi-line block", cfg.Fix.Hooks.Before[1].Run)
	}
}

func TestLoad_MalformedYml_WarnOnly(t *testing.T) {
	home := withTempHome(t)
	// Tabs are illegal in YAML for indentation → parse error
	writeHomeYml(t, home, "fix:\n  agent: pi\n\tbroken: tab-indent\n")

	cfg, notes := Load()
	if !cfgIsZero(cfg) {
		t.Errorf("malformed yml should yield zero Config, got %+v", cfg)
	}
	if !notes.HasWarnings() {
		t.Errorf("malformed yml should warn, got none")
	}
}

func TestLoad_PartialYml_OK(t *testing.T) {
	home := withTempHome(t)
	writeHomeYml(t, home, `
push:
  agent: claude
`) // only push, no fix/close/sync/pr/commit

	cfg, notes := Load()
	if notes.HasWarnings() {
		t.Fatalf("partial yml should not warn, got: %v", notes.Warnings)
	}
	if cfg.Push.Agent != "claude" {
		t.Errorf("Push.Agent = %q, want claude", cfg.Push.Agent)
	}
	if !cmdCfgIsZero(cfg.Fix) {
		t.Errorf("Fix should be zero-value, got %+v", cfg.Fix)
	}
}

// TestLoad_PRFieldPresent verifies the `pr:` block round-trips
// through yaml — users who write `pr: hooks: { before: [...] }`
// in ~/.nightme/gtw.yml get cfg.PR populated, not silently
// dropped. (runPR wires through withHooks now — see cmd.go.)
func TestLoad_PRFieldPresent(t *testing.T) {
	home := withTempHome(t)
	writeHomeYml(t, home, `
pr:
  hooks:
    before:
      - run: echo pr-before
    after:
      - run: echo pr-after
`)
	cfg, notes := Load()
	if notes.HasWarnings() {
		t.Fatalf("unexpected load warnings: %v", notes.Warnings)
	}
	if len(cfg.PR.Hooks.Before) != 1 || cfg.PR.Hooks.Before[0].Run != "echo pr-before" {
		t.Errorf("PR.Hooks.Before not parsed, got %+v", cfg.PR.Hooks.Before)
	}
	if len(cfg.PR.Hooks.After) != 1 || cfg.PR.Hooks.After[0].Run != "echo pr-after" {
		t.Errorf("PR.Hooks.After not parsed, got %+v", cfg.PR.Hooks.After)
	}
}

// --- RunHooks() ---

func TestRunHooks_Empty(t *testing.T) {
	got := RunHooks(context.Background(), nil, HookContext{Command: "test"}, t.TempDir())
	if got != nil {
		t.Errorf("empty hooks should return nil, got %v", got)
	}
}

func TestRunHooks_Success(t *testing.T) {
	dir := t.TempDir()
	results := RunHooks(context.Background(),
		[]Hook{{Run: "echo hello"}}, HookContext{Command: "test"}, dir)

	if len(results) != 1 {
		t.Fatalf("len(results) = %d, want 1", len(results))
	}
	if results[0].Err != nil {
		t.Errorf("Err = %v, want nil", results[0].Err)
	}
	if !strings.Contains(results[0].Stdout, "hello") {
		t.Errorf("Stdout = %q, want contains 'hello'", results[0].Stdout)
	}
}

func TestRunHooks_ExitNonZero_DoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	results := RunHooks(context.Background(),
		[]Hook{
			{Run: "exit 7"},
			{Run: "echo still-ran"},
		}, HookContext{Command: "test"}, dir)

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Err == nil {
		t.Errorf("[0] Err = nil, want non-nil for exit 7")
	}
	if results[1].Err != nil {
		t.Errorf("[1] Err = %v, want nil (second hook must still run)", results[1].Err)
	}
	if !strings.Contains(results[1].Stdout, "still-ran") {
		t.Errorf("[1] Stdout = %q, want contains 'still-ran'", results[1].Stdout)
	}
}

func TestRunHooks_MissingBinary_DoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	results := RunHooks(context.Background(),
		[]Hook{
			{Run: "this-binary-does-not-exist-xyz-12345"},
			{Run: "echo ok"},
		}, HookContext{Command: "test"}, dir)

	if len(results) != 2 {
		t.Fatalf("len(results) = %d, want 2", len(results))
	}
	if results[0].Err == nil {
		t.Errorf("[0] Err = nil, want non-nil for missing binary")
	}
	if results[1].Err != nil {
		t.Errorf("[1] Err = %v, want nil", results[1].Err)
	}
}

func TestRunHooks_EmptyCwd(t *testing.T) {
	results := RunHooks(context.Background(),
		[]Hook{{Run: "echo x"}}, HookContext{Command: "test"}, "")

	if len(results) != 1 {
		t.Fatalf("len = %d, want 1", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("empty cwd should produce error")
	}
	if !strings.Contains(results[0].Err.Error(), "no active workspace") {
		t.Errorf("Err = %v, want 'no active workspace'", results[0].Err)
	}
}

func TestRunHooks_EmptyRun(t *testing.T) {
	results := RunHooks(context.Background(),
		[]Hook{{Run: "  "}}, HookContext{Command: "test"}, t.TempDir())

	if results[0].Err == nil {
		t.Fatal("empty run should produce error")
	}
	if !strings.Contains(results[0].Err.Error(), "no run field") {
		t.Errorf("Err = %v, want 'no run field'", results[0].Err)
	}
}

func TestRunHooks_UnknownType_DoesNotBlock(t *testing.T) {
	dir := t.TempDir()
	results := RunHooks(context.Background(),
		[]Hook{
			{Type: "agent", Run: "do something"},
			{Run: "echo ok"},
		}, HookContext{Command: "test"}, dir)

	if len(results) != 2 {
		t.Fatalf("len = %d, want 2", len(results))
	}
	if results[0].Err == nil {
		t.Fatal("[0] unknown type should produce error")
	}
	if !strings.Contains(results[0].Err.Error(), "unsupported hook type") {
		t.Errorf("[0] Err = %v, want 'unsupported hook type'", results[0].Err)
	}
	if results[1].Err != nil {
		t.Errorf("[1] Err = %v, want nil", results[1].Err)
	}
}

func TestRunHooks_RespectsCwd(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "marker.txt")
	if err := os.WriteFile(marker, []byte("present"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Use a subshell that reads the marker from cwd.
	results := RunHooks(context.Background(),
		[]Hook{{Run: "ls marker.txt"}}, HookContext{Command: "test"}, dir)
	if results[0].Err != nil {
		t.Errorf("expected ls to succeed in cwd %s, got err: %v", dir, results[0].Err)
	}
}

// --- FormatResults() ---

func TestFormatResults_Empty(t *testing.T) {
	if got := FormatResults("before", nil); got != "" {
		t.Errorf("empty results = %q, want empty", got)
	}
}

// --- HookContext.ToEnv() ---

func TestHookContext_ToEnv_AllFields(t *testing.T) {
	hc := HookContext{
		RepoRoot:      "/r",
		Worktree:      "/w",
		Branch:        "feat-x",
		DefaultBranch: "main",
		Command:       "fix", // not exported
	}
	got := hc.ToEnv()
	want := []string{
		"GTW_REPO_ROOT=/r",
		"GTW_WORKTREE=/w",
		"GTW_BRANCH=feat-x",
		"GTW_DEFAULT_BRANCH=main",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ToEnv() = %v, want %v", got, want)
	}
}

func TestHookContext_ToEnv_EmptyFieldsSkipped(t *testing.T) {
	// DefaultBranch not discoverable (no origin) → not exported.
	hc := HookContext{
		RepoRoot: "/r",
		Worktree: "/w",
		Branch:   "feat-x",
	}
	got := hc.ToEnv()
	for _, line := range got {
		if strings.HasPrefix(line, "GTW_DEFAULT_BRANCH=") {
			t.Errorf("GTW_DEFAULT_BRANCH should be skipped when empty, got %q", line)
		}
	}
	if len(got) != 3 {
		t.Errorf("expected 3 env vars, got %d: %v", len(got), got)
	}
}

// --- Env injection (real subprocess) ---

// TestRunHooks_InjectsEnvVars runs a hook that prints the GTW_*
// vars it sees, and verifies the 4 mandatory vars are set with
// the expected values. Skipped on Windows because the test uses
// `sh`-specific syntax (`echo $VAR`).
func TestRunHooks_InjectsEnvVars(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh-only syntax")
	}
	dir := t.TempDir()
	hc := HookContext{
		RepoRoot:      "/tmp/repo",
		Worktree:      "/tmp/wt",
		Branch:        "feat-inject",
		DefaultBranch: "main",
		Command:       "push",
	}
	// dump all GTW_* vars + HOME (sanity check that os.Environ
	// is still inherited alongside our additions).
	results := RunHooks(context.Background(),
		[]Hook{{Run: "env | grep '^GTW_\\|^HOME=' | sort"}},
		hc, dir)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("hook failed: %+v", results)
	}
	want := []string{
		"GTW_BRANCH=feat-inject",
		"GTW_DEFAULT_BRANCH=main",
		"GTW_REPO_ROOT=/tmp/repo",
		"GTW_WORKTREE=/tmp/wt",
		// HOME — only present if os.Environ() is still inherited.
	}
	for _, line := range want[:4] {
		if !strings.Contains(results[0].Stdout, line) {
			t.Errorf("stdout missing %q\n--- got ---\n%s", line, results[0].Stdout)
		}
	}
	if !strings.Contains(results[0].Stdout, "HOME=") {
		t.Errorf("HOME not inherited from os.Environ(); env inheritance regressed\n%s",
			results[0].Stdout)
	}
}

// TestRunHooks_NoDefaultBranchOmitsVar verifies that when the
// derived HookContext has an empty DefaultBranch, the env var
// is NOT set (vs. set to empty string). Hooks can use
// `[[ -n "$GTW_DEFAULT_BRANCH" ]]` to detect "not discoverable".
func TestRunHooks_NoDefaultBranchOmitsVar(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh-only syntax")
	}
	dir := t.TempDir()
	hc := HookContext{
		RepoRoot: "/tmp/repo",
		Worktree: "/tmp/wt",
		Branch:   "feat-x",
		// DefaultBranch intentionally empty.
	}
	results := RunHooks(context.Background(),
		[]Hook{{Run: "if [ -n \"${GTW_DEFAULT_BRANCH+x}\" ]; then echo SET; else echo UNSET; fi"}},
		hc, dir)
	if len(results) != 1 || results[0].Err != nil {
		t.Fatalf("hook failed: %+v", results)
	}
	if !strings.Contains(results[0].Stdout, "UNSET") {
		t.Errorf("empty DefaultBranch should omit the env var, got %q", results[0].Stdout)
	}
}

func TestFormatResults_ShowsAlways(t *testing.T) {
	// Per the always-echo decision in wip/gtw-hooks.md, every
	// hook gets a `> <run>` line regardless of stdout/stderr.
	results := RunHooks(context.Background(),
		[]Hook{{Run: "true"}}, HookContext{Command: "test"}, t.TempDir())
	out := FormatResults("before", results)
	if !strings.Contains(out, ">") {
		t.Errorf("expected `>` command prefix, got: %q", out)
	}
	if !strings.Contains(out, "true") {
		t.Errorf("expected run command in output, got: %q", out)
	}
	if !strings.Contains(out, "✅ hooks: before") {
		t.Errorf("expected standard gtw title `✅ hooks: before`, got: %q", out)
	}
}

func TestFormatResults_ShowsFailure(t *testing.T) {
	// `exit 5` produces an *exec.ExitError with code 5; the
	// Format 3 rule (gtw/README.md §2.3) prepends `  ❌ exit N`
	// to the raw block for non-zero exits so the user sees the
	// cause-of-failure first.
	results := RunHooks(context.Background(),
		[]Hook{{Run: "echo oops 1>&2; exit 5"}}, HookContext{Command: "test"}, t.TempDir())
	out := FormatResults("after", results)
	if !strings.Contains(out, "oops") {
		t.Errorf("stderr should appear, got: %q", out)
	}
	if !strings.Contains(out, "❌ exit 5") {
		t.Errorf("expected `❌ exit 5` failure indicator, got: %q", out)
	}
}

func TestFormatResults_NonExitError_UsesWarning(t *testing.T) {
	// Errors that don't carry an exit code (timeout, unsupported
	// type, no workspace, empty run) keep the `⚠️ <msg>` shape —
	// only `*exec.ExitError` triggers the `❌ exit N` injection.
	results := RunHooks(context.Background(),
		[]Hook{{Run: ""}}, HookContext{Command: "test"}, t.TempDir())
	out := FormatResults("after", results)
	if !strings.Contains(out, "⚠️") {
		t.Errorf("expected `⚠️ <msg>` for non-exit error, got: %q", out)
	}
	if strings.Contains(out, "❌ exit") {
		t.Errorf("non-exit error should NOT trigger `❌ exit N`, got: %q", out)
	}
}

// --- ResolveAgent() ---

func TestResolveAgent_CLIWinsOverYmlAndSession(t *testing.T) {
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("session-default")

	got, notes := ResolveAgent("cli-wins", "yml-agent", cs)
	if got != "cli-wins" {
		t.Errorf("got = %q, want cli-wins", got)
	}
	if len(notes) > 0 {
		t.Errorf("cli-wins should produce no notes, got: %v", notes)
	}
}

func TestResolveAgent_YmlWinsOverSession(t *testing.T) {
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("session-default")

	// Register a test agent so yml can reference a name that
	// agent.Builtins recognises. Builtins may be empty in tests
	// because bridge init() funcs aren't transitively imported
	// from this package.
	prev, _ := agent.Builtins.Get("test-agent-yml")
	t.Cleanup(func() {
		if prev != nil {
			_ = agent.Builtins.Register(prev)
		}
	})
	_ = agent.Builtins.Register(&testStarter{name: "test-agent-yml"})

	got, notes := ResolveAgent("", "test-agent-yml", cs)
	if got != "test-agent-yml" {
		t.Errorf("got = %q, want test-agent-yml (yml wins over session)", got)
	}
	if len(notes) > 0 {
		t.Errorf("known yml agent should produce no notes, got: %v", notes)
	}
}

func TestResolveAgent_FallbackToSessionWhenYmlUnknown(t *testing.T) {
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("session-default")

	got, notes := ResolveAgent("", "nonexistent-agent-zzz", cs)
	if got != "session-default" {
		t.Errorf("got = %q, want session-default (yml unknown → fall through)", got)
	}
	if len(notes) != 1 {
		t.Fatalf("notes len = %d, want 1", len(notes))
	}
	if !strings.Contains(notes[0], "nonexistent-agent-zzz") {
		t.Errorf("note should name the bad agent, got: %v", notes[0])
	}
}

func TestResolveAgent_FallbackToSessionWhenYmlUnknown_NoSessionEither(t *testing.T) {
	// yml says "pi" but pi doesn't exist AND session has nothing
	// either. We should still get a note (so the user knows why)
	// but the returned name is empty so the caller can hit the
	// existing "no agent selected" path.
	got, notes := ResolveAgent("", "nonexistent-agent-zzz", &chatsession.ChatSession{})
	if got != "" {
		t.Errorf("got = %q, want empty", got)
	}
	if len(notes) != 1 {
		t.Errorf("notes len = %d, want 1", len(notes))
	}
}

func TestResolveAgent_AllEmpty(t *testing.T) {
	got, notes := ResolveAgent("", "", &chatsession.ChatSession{})
	if got != "" {
		t.Errorf("got = %q, want empty", got)
	}
	if len(notes) != 0 {
		t.Errorf("all-empty should produce no notes, got: %v", notes)
	}
}

// --- withHooks() (factory wrapper) ---

// fakeChannel records every Send call so the test can assert
// what the wrapper emitted.
type fakeChannel struct {
	sent []string
}

func (c *fakeChannel) Send(_ context.Context, msg messages.OutboundMessage) error {
	c.sent = append(c.sent, msg.Text)
	return nil
}
func (c *fakeChannel) SendCard(_ context.Context, _ messages.OutboundMessage) (string, error) {
	return "", nil
}
func (c *fakeChannel) Patch(_ context.Context, _ messages.OutboundMessage) error {
	return nil
}

func TestWithHooks_BeforeAndAfterFire(t *testing.T) {
	ch := &fakeChannel{}
	cs := (&chatsession.ChatSession{}).WithEmitter(ch)
	_ = cs.SetSelectedCwd(t.TempDir())

	f := &Factory{} // HandlerDeps nil is fine; withHooks doesn't touch it
	mainCalled := false

	err := f.withHooks(context.Background(), cs, "chat-1", "msg-1", LoadNotes{}, func() HookContext { return HookContext{Command: "test"} },
		[]Hook{{Run: "echo before"}},
		[]Hook{{Run: "echo after"}},
		func() error {
			mainCalled = true
			return nil
		})
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if !mainCalled {
		t.Fatal("main was not called")
	}
	// U1: 2 follow-up replies (before + after) so chat order
	// matches execution order. First must be before, second after.
	if len(ch.sent) != 2 {
		t.Fatalf("expected 2 follow-up Sends (before, after), got %d: %v", len(ch.sent), ch.sent)
	}
	if !strings.Contains(ch.sent[0], "hooks: before") {
		t.Errorf("reply[0] missing 'hooks: before': %q", ch.sent[0])
	}
	if strings.Contains(ch.sent[0], "hooks: after") {
		t.Errorf("reply[0] should not contain 'hooks: after' (must come later): %q", ch.sent[0])
	}
	if !strings.Contains(ch.sent[1], "hooks: after") {
		t.Errorf("reply[1] missing 'hooks: after': %q", ch.sent[1])
	}
	if strings.Contains(ch.sent[1], "hooks: before") {
		t.Errorf("reply[1] should not contain 'hooks: before' (must come earlier): %q", ch.sent[1])
	}
}

func TestWithHooks_BeforeFailureDoesNotBlockMain(t *testing.T) {
	ch := &fakeChannel{}
	cs := (&chatsession.ChatSession{}).WithEmitter(ch)
	_ = cs.SetSelectedCwd(t.TempDir())
	f := &Factory{}

	mainCalled := false
	err := f.withHooks(context.Background(), cs, "chat-1", "msg-1", LoadNotes{}, func() HookContext { return HookContext{Command: "test"} },
		[]Hook{{Run: "exit 1"}},
		nil,
		func() error {
			mainCalled = true
			return nil
		})
	if err != nil {
		t.Fatalf("main err should be nil (before failure isolated), got %v", err)
	}
	if !mainCalled {
		t.Fatal("main not called despite before-hook failure")
	}
	// Format 3 failure indicator: `false; exit 1` produces an
	// *exec.ExitError with code 1, which FormatResults renders
	// as `❌ exit 1` (gtw/README.md §2.3). The previous `⚠️`
	// marker is now reserved for non-exit-code errors only.
	if len(ch.sent) != 1 || !strings.Contains(ch.sent[0], "❌ exit 1") {
		t.Errorf("expected follow-up with `❌ exit 1` marker, got: %v", ch.sent)
	}
}

func TestWithHooks_AfterFiresEvenWhenMainFails(t *testing.T) {
	ch := &fakeChannel{}
	cs := (&chatsession.ChatSession{}).WithEmitter(ch)
	_ = cs.SetSelectedCwd(t.TempDir())
	f := &Factory{}

	err := f.withHooks(context.Background(), cs, "chat-1", "msg-1", LoadNotes{}, func() HookContext { return HookContext{Command: "test"} },
		nil,
		[]Hook{{Run: "echo cleaned-up"}},
		func() error { return errBoom })

	if err != errBoom {
		t.Fatalf("err = %v, want main err to pass through", err)
	}
	if len(ch.sent) != 1 {
		t.Fatalf("after hooks should fire even on main failure; sent = %v", ch.sent)
	}
	if !strings.Contains(ch.sent[0], "cleaned-up") {
		t.Errorf("after output missing: %q", ch.sent[0])
	}
}

func TestWithHooks_NoHooksNoReply(t *testing.T) {
	ch := &fakeChannel{}
	cs := (&chatsession.ChatSession{}).WithEmitter(ch)
	_ = cs.SetSelectedCwd(t.TempDir())
	f := &Factory{}

	_ = f.withHooks(context.Background(), cs, "chat-1", "msg-1", LoadNotes{}, func() HookContext { return HookContext{Command: "test"} },
		nil, nil, func() error { return nil })

	if len(ch.sent) != 0 {
		t.Errorf("no hooks + no notes should produce no follow-up, got: %v", ch.sent)
	}
}

// TestWithHooks_NoHooks_SkipsHCFn locks in the fast-path: when
// neither before nor after hooks are configured, withHooks
// must NOT invoke hcFn() (which would trigger git rev-parse +
// DefaultBranch — the cost we're trying to avoid on the no-
// hooks hot path). The test would panic if hcFn is called.
func TestWithHooks_NoHooks_SkipsHCFn(t *testing.T) {
	ch := &fakeChannel{}
	cs := (&chatsession.ChatSession{}).WithEmitter(ch)
	_ = cs.SetSelectedCwd(t.TempDir())
	f := &Factory{}

	called := false
	hcFn := func() HookContext {
		called = true
		return HookContext{Command: "test"}
	}
	_ = f.withHooks(context.Background(), cs, "chat-1", "msg-1", LoadNotes{}, hcFn,
		nil, nil, func() error { return nil })

	if called {
		t.Errorf("hcFn must not be invoked when both before+after are empty (no-hook fast-path)")
	}
}

func TestWithHooks_NilChannel_NoPanic(t *testing.T) {
	// ChatSession with no Channel set (e.g. test harness) — the
	// wrapper must not panic when sending the hook block.
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedCwd(t.TempDir())
	f := &Factory{}

	_ = f.withHooks(context.Background(), cs, "chat-1", "msg-1", LoadNotes{}, func() HookContext { return HookContext{Command: "test"} },
		[]Hook{{Run: "echo x"}}, nil, func() error { return nil })
	// No assertion needed; success = no panic.
}

func TestWithHooks_LoadNotesInReply(t *testing.T) {
	ch := &fakeChannel{}
	cs := (&chatsession.ChatSession{}).WithEmitter(ch)
	_ = cs.SetSelectedCwd(t.TempDir())
	f := &Factory{}

	notes := LoadNotes{Warnings: []string{"⚠️ simulated warning"}}

	_ = f.withHooks(context.Background(), cs, "chat-1", "msg-1", notes, func() HookContext { return HookContext{Command: "test"} },
		nil, nil, func() error { return nil })

	if len(ch.sent) != 1 {
		t.Fatalf("expected 1 follow-up, got %d", len(ch.sent))
	}
	if !strings.Contains(ch.sent[0], "hooks config") {
		t.Errorf("expected 'hooks config' header, got: %q", ch.sent[0])
	}
	if !strings.Contains(ch.sent[0], "simulated warning") {
		t.Errorf("warning body missing, got: %q", ch.sent[0])
	}
}

// --- withHooks closure: hc mutated inside main() ---

// TestWithHooks_ClosureReadsMutatedHC verifies that withHooks
// re-invokes hcFn() AFTER main() returns, so post-hook sees the
// mutated HookContext — this is the fix for the post-fix-after
// bug where GTW_WORKTREE / GTW_BRANCH were stale because RunFix
// created the worktree inside main(). The /gtw fix caller
// mutates the captured `hc` variable on success; the closure
// captures the variable (not the value), so the next hcFn()
// call returns the new state.
func TestWithHooks_ClosureReadsMutatedHC(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses sh-only syntax")
	}
	ch := &fakeChannel{}
	cs := (&chatsession.ChatSession{}).WithEmitter(ch)
	_ = cs.SetSelectedCwd(t.TempDir())
	f := &Factory{}

	hc := HookContext{Command: "fix", Branch: "fix/predicted"}
	hcFn := func() HookContext { return hc }

	_ = f.withHooks(context.Background(), cs, "chat-1", "msg-1", LoadNotes{}, hcFn,
		[]Hook{{Run: `echo "BRANCH=$GTW_BRANCH"`}},
		[]Hook{{Run: `echo "BRANCH=$GTW_BRANCH"`}},
		func() error {
			// Simulate /gtw fix resolving the predicted branch
			// to the actual one once the worktree exists.
			hc.Branch = "fix/resolved"
			return nil
		})

	if len(ch.sent) < 2 {
		t.Fatalf("expected 2 follow-ups (before+after), got %d: %v", len(ch.sent), ch.sent)
	}
	if !strings.Contains(ch.sent[0], "BRANCH=fix/predicted") {
		t.Errorf("pre-hook should see predicted Branch=fix/predicted, got: %q", ch.sent[0])
	}
	if !strings.Contains(ch.sent[1], "BRANCH=fix/resolved") {
		t.Errorf("post-hook should see mutated Branch=fix/resolved, got: %q", ch.sent[1])
	}
}

// --- ResolveAgent note text (B2 fix) ---

// B2 fix: when ymlAgent is unknown AND session has no default,
// the diagnostic note must NOT claim "using session default"
// because there's nothing to fall back to. Otherwise the user
// gets a misleading message that hides the real cause.
func TestResolveAgent_NoteDoesNotLieWhenNoFallback(t *testing.T) {
	got, notes := ResolveAgent("", "nonexistent-agent-zzz", &chatsession.ChatSession{})
	if got != "" {
		t.Errorf("got = %q, want empty", got)
	}
	if len(notes) != 1 {
		t.Fatalf("notes len = %d, want 1", len(notes))
	}
	if strings.Contains(notes[0], "using session default") {
		t.Errorf("note falsely promises a session default: %q", notes[0])
	}
	if strings.Contains(notes[0], "falling back") {
		t.Errorf("note falsely claims a fallback exists: %q", notes[0])
	}
	if !strings.Contains(notes[0], "not found") {
		t.Errorf("note should mention 'not found', got: %q", notes[0])
	}
}

// B2 fix: when ymlAgent is unknown BUT session has a default,
// the note should explicitly mention the fallback so the user
// sees what was actually used.
func TestResolveAgent_NoteMentionsFallbackWhenAvailable(t *testing.T) {
	cs := &chatsession.ChatSession{}
	_ = cs.SetSelectedAgent("claude-fallback")

	got, notes := ResolveAgent("", "nonexistent-agent-zzz", cs)
	if got != "claude-fallback" {
		t.Errorf("got = %q, want claude-fallback", got)
	}
	if len(notes) != 1 {
		t.Fatalf("notes len = %d, want 1", len(notes))
	}
	if !strings.Contains(notes[0], "falling back") {
		t.Errorf("note should mention the fallback, got: %q", notes[0])
	}
}

// --- withHooks ordering (U1 fix) ---

// U1 fix: when only after-hooks fire (e.g. main failed BEFORE
// before-hooks were scheduled, or only after is configured),
// only ONE follow-up reply is sent — never two empty ones.
func TestWithHooks_OnlyAfterHooksFires_OneReply(t *testing.T) {
	ch := &fakeChannel{}
	cs := (&chatsession.ChatSession{}).WithEmitter(ch)
	_ = cs.SetSelectedCwd(t.TempDir())
	f := &Factory{}

	_ = f.withHooks(context.Background(), cs, "chat-1", "msg-1", LoadNotes{}, func() HookContext { return HookContext{Command: "test"} },
		nil, // no before hooks
		[]Hook{{Run: "echo post-cleanup"}},
		func() error { return nil })

	if len(ch.sent) != 1 {
		t.Fatalf("expected 1 follow-up (after only), got %d: %v", len(ch.sent), ch.sent)
	}
	if !strings.Contains(ch.sent[0], "hooks: after") {
		t.Errorf("reply should be the after block, got: %q", ch.sent[0])
	}
}

// U1 fix: loadNotes ride along with the before-hooks reply (the
// first follow-up). They should NOT appear in the after-hooks
// reply — that would be a second-card duplicate.
func TestWithHooks_LoadNotesNotDuplicatedInAfter(t *testing.T) {
	ch := &fakeChannel{}
	cs := (&chatsession.ChatSession{}).WithEmitter(ch)
	_ = cs.SetSelectedCwd(t.TempDir())
	f := &Factory{}

	notes := LoadNotes{Warnings: []string{"⚠️ simulated warning"}}

	_ = f.withHooks(context.Background(), cs, "chat-1", "msg-1", notes, func() HookContext { return HookContext{Command: "test"} },
		[]Hook{{Run: "true"}},
		[]Hook{{Run: "true"}},
		func() error { return nil })

	if len(ch.sent) != 2 {
		t.Fatalf("expected 2 follow-ups, got %d", len(ch.sent))
	}
	if !strings.Contains(ch.sent[0], "simulated warning") {
		t.Errorf("notes should ride with before-reply[0], got: %q", ch.sent[0])
	}
	if strings.Contains(ch.sent[1], "simulated warning") {
		t.Errorf("notes should NOT repeat in after-reply[1], got: %q", ch.sent[1])
	}
}

// --- helpers ---

var errBoom = errors.New("boom")

// testStarter is a minimal agent.Starter for testing ResolveAgent.
// We only care about Info() — Detect/Start/RunOnce aren't invoked
// by the priority resolver. Pattern mirrors internal/agent's own
// fakeAgent (registry_test.go).
type testStarter struct{ name string }

func (s *testStarter) Info() agent.Info              { return agent.NewInfo(s.name, agent.ModePTY, "", nil, nil) }
func (s *testStarter) Detect() error                  { return nil }
func (s *testStarter) Start(context.Context, agent.StartConfig) (*agent.Agent, error) {
	return nil, errors.New("testStarter: Start not implemented")
}
func (s *testStarter) RunOnce(context.Context, agent.StartConfig, []agent.ContentBlock) (agent.RunResult, error) {
	return agent.RunResult{}, errors.New("testStarter: RunOnce not implemented")
}

// cfgIsZero reports whether cfg has every field at its zero value.
// Config contains slices (which aren't ==-comparable), so we check
// field-by-field.
func cfgIsZero(cfg Config) bool {
	return cmdCfgIsZero(cfg.Fix) &&
		cmdCfgIsZero(cfg.Push) &&
		cmdCfgIsZero(cfg.Commit) &&
		cmdCfgIsZero(cfg.Close) &&
		cmdCfgIsZero(cfg.Sync)
}

func cmdCfgIsZero(c CmdConfig) bool {
	return c.Agent == "" &&
		len(c.Hooks.Before) == 0 &&
		len(c.Hooks.After) == 0
}

// Ensure timeout is reasonable (sanity check the const).
func TestHookTimeout_Reasonable(t *testing.T) {
	if hookTimeout <= 0 || hookTimeout > 5*time.Minute {
		t.Errorf("hookTimeout = %v, want > 0 and <= 5m", hookTimeout)
	}
}