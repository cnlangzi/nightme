package gtw

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// --- EnsureGitignore ---

// TestEnsureGitignore_CreatesWhenMissing verifies a missing
// .gitignore is created with just the .nightme/ entry. This is
// the cold-start case — worktree just initialized by git, no
// .gitignore yet.
func TestEnsureGitignore_CreatesWhenMissing(t *testing.T) {
	wt := t.TempDir()

	if err := EnsureGitignore(wt); err != nil {
		t.Fatalf("EnsureGitignore: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(wt, ".gitignore"))
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if want := ".nightme/\n"; string(got) != want {
		t.Fatalf(".gitignore content = %q, want %q", got, want)
	}
}

// TestEnsureGitignore_AppendsWhenMissingEntry verifies an
// existing .gitignore without .nightme/ gets the entry appended
// without disturbing its original lines.
func TestEnsureGitignore_AppendsWhenMissingEntry(t *testing.T) {
	wt := t.TempDir()
	giPath := filepath.Join(wt, ".gitignore")

	original := "node_modules/\n*.log\n"
	if err := os.WriteFile(giPath, []byte(original), 0o644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	if err := EnsureGitignore(wt); err != nil {
		t.Fatalf("EnsureGitignore: %v", err)
	}

	got, err := os.ReadFile(giPath)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	want := original + ".nightme/\n"
	if string(got) != want {
		t.Fatalf(".gitignore content = %q, want %q", got, want)
	}
}

// TestEnsureGitignore_AppendsWithoutTrailingNewline verifies the
// case where the existing file does not end in \n — we add one
// first so our entry lands on its own line, not glued onto the
// previous pattern.
func TestEnsureGitignore_AppendsWithoutTrailingNewline(t *testing.T) {
	wt := t.TempDir()
	giPath := filepath.Join(wt, ".gitignore")

	original := "node_modules/" // no trailing newline
	if err := os.WriteFile(giPath, []byte(original), 0o644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	if err := EnsureGitignore(wt); err != nil {
		t.Fatalf("EnsureGitignore: %v", err)
	}

	got, err := os.ReadFile(giPath)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	want := "node_modules/\n.nightme/\n"
	if string(got) != want {
		t.Fatalf(".gitignore content = %q, want %q", got, want)
	}
}

// TestEnsureGitignore_IdempotentWhenPresent verifies repeated
// EnsureGitignore calls don't duplicate the entry. Matches both
// .nightme/ (directory form) and .nightme/gtw.yml (file form,
// in case a user wrote it that way).
func TestEnsureGitignore_IdempotentWhenPresent(t *testing.T) {
	wt := t.TempDir()
	giPath := filepath.Join(wt, ".gitignore")
	if err := os.WriteFile(giPath, []byte("foo/\n.nightme/\nbar/\n"), 0o644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	// Run twice to be extra sure.
	if err := EnsureGitignore(wt); err != nil {
		t.Fatalf("EnsureGitignore first call: %v", err)
	}
	if err := EnsureGitignore(wt); err != nil {
		t.Fatalf("EnsureGitignore second call: %v", err)
	}

	got, err := os.ReadFile(giPath)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if strings.Count(string(got), ".nightme/") != 1 {
		t.Fatalf(".gitignore has duplicate .nightme/ entry:\n%s", got)
	}
}

// TestEnsureGitignore_AcceptsExplicitFileForm verifies a
// hand-edited ".nightme/gtw.yml" line also satisfies the check
// (no need to rewrite to .nightme/).
func TestEnsureGitignore_AcceptsExplicitFileForm(t *testing.T) {
	wt := t.TempDir()
	giPath := filepath.Join(wt, ".gitignore")
	if err := os.WriteFile(giPath, []byte(".nightme/gtw.yml\n"), 0o644); err != nil {
		t.Fatalf("seed .gitignore: %v", err)
	}

	if err := EnsureGitignore(wt); err != nil {
		t.Fatalf("EnsureGitignore: %v", err)
	}

	got, err := os.ReadFile(giPath)
	if err != nil {
		t.Fatalf("read .gitignore: %v", err)
	}
	if string(got) != ".nightme/gtw.yml\n" {
		t.Fatalf(".gitignore was modified:\n%s", got)
	}
}

// --- WriteGTWYml / ReadGTWYml round-trip ---

// fixedNow returns a deterministic time.Now for round-trip tests.
func fixedNow() time.Time {
	return time.Date(2026, 8, 8, 10, 30, 0, 0, time.UTC)
}

// TestGTWYml_RoundTrip_AllFields covers the ModeRemote case where
// every field is populated. Write + Read should yield an
// equivalent Context (modulo State/UpdatedAt which are not
// serialized).
func TestGTWYml_RoundTrip_AllFields(t *testing.T) {
	wt := t.TempDir()

	in := Context{
		Mode:      ModeRemote,
		Issue:     42,
		Branch:    "fix/42-foo",
		Worktree:  wt,
		RepoRoot:  "/some/main/repo",
		Repo:      "cnlangzi/nightme",
		Provider:  "github",
		State:     StateFixing,
		UpdatedAt: fixedNow(),
	}
	if err := WriteGTWYml(wt, in, fixedNow); err != nil {
		t.Fatalf("WriteGTWYml: %v", err)
	}

	out, err := ReadGTWYml(wt)
	if err != nil {
		t.Fatalf("ReadGTWYml: %v", err)
	}
	if out.Mode != in.Mode {
		t.Errorf("Mode = %q, want %q", out.Mode, in.Mode)
	}
	if out.Issue != in.Issue {
		t.Errorf("Issue = %d, want %d", out.Issue, in.Issue)
	}
	if out.Branch != in.Branch {
		t.Errorf("Branch = %q, want %q", out.Branch, in.Branch)
	}
	if out.Worktree != in.Worktree {
		t.Errorf("Worktree = %q, want %q", out.Worktree, in.Worktree)
	}
	if out.RepoRoot != in.RepoRoot {
		t.Errorf("RepoRoot = %q, want %q", out.RepoRoot, in.RepoRoot)
	}
	if out.Repo != in.Repo {
		t.Errorf("Repo = %q, want %q", out.Repo, in.Repo)
	}
	if out.Provider != in.Provider {
		t.Errorf("Provider = %q, want %q", out.Provider, in.Provider)
	}
	if out.State != StateFixing {
		t.Errorf("State = %q, want StateFixing (yml always reads as in-flight)", out.State)
	}
}

// TestGTWYml_RoundTrip_LocalMode verifies ModeLocal with empty
// Repo/Provider round-trips cleanly. This is the common case
// after `/gtw fix --name foo` in a no-origin repo.
func TestGTWYml_RoundTrip_LocalMode(t *testing.T) {
	wt := t.TempDir()

	in := Context{
		Mode:     ModeLocal,
		Issue:    -1,
		Branch:   "login-bug",
		Worktree: wt,
		RepoRoot: "/some/main/repo",
		// Repo / Provider deliberately empty for local mode.
		State:     StateFixing,
		UpdatedAt: fixedNow(),
	}
	if err := WriteGTWYml(wt, in, fixedNow); err != nil {
		t.Fatalf("WriteGTWYml: %v", err)
	}

	out, err := ReadGTWYml(wt)
	if err != nil {
		t.Fatalf("ReadGTWYml: %v", err)
	}
	if out.Mode != ModeLocal {
		t.Errorf("Mode = %q, want ModeLocal", out.Mode)
	}
	if out.Issue != -1 {
		t.Errorf("Issue = %d, want -1", out.Issue)
	}
	if out.Repo != "" {
		t.Errorf("Repo = %q, want empty", out.Repo)
	}
	if out.Provider != "" {
		t.Errorf("Provider = %q, want empty", out.Provider)
	}
	if out.Branch != "login-bug" {
		t.Errorf("Branch = %q, want login-bug", out.Branch)
	}
}

// TestWriteGTWYml_RefusesWhenExists verifies the single-fix-
// per-repo invariant. Pre-existing yml → ErrGtwYmlExists so the
// caller can surface "first run /gtw close" instead of silently
// overwriting.
func TestWriteGTWYml_RefusesWhenExists(t *testing.T) {
	wt := t.TempDir()

	first := Context{
		Mode: ModeRemote, Issue: 1, Branch: "fix/1",
		Worktree: wt, RepoRoot: "/repo", Repo: "o/r", Provider: "github",
		State: StateFixing, UpdatedAt: fixedNow(),
	}
	if err := WriteGTWYml(wt, first, fixedNow); err != nil {
		t.Fatalf("first WriteGTWYml: %v", err)
	}

	second := Context{
		Mode: ModeRemote, Issue: 2, Branch: "fix/2",
		Worktree: wt, RepoRoot: "/repo", Repo: "o/r", Provider: "github",
		State: StateFixing, UpdatedAt: fixedNow(),
	}
	err := WriteGTWYml(wt, second, fixedNow)
	if !errors.Is(err, ErrGtwYmlExists) {
		t.Fatalf("second WriteGTWYml err = %v, want ErrGtwYmlExists", err)
	}

	// First write must be intact.
	got, err := ReadGTWYml(wt)
	if err != nil {
		t.Fatalf("ReadGTWYml: %v", err)
	}
	if got.Branch != "fix/1" {
		t.Errorf("Branch after overwrite attempt = %q, want fix/1 (first write must survive)", got.Branch)
	}
}

// TestReadGTWYml_RejectsRelativeRepoRoot verifies the safety
// check on read — a relative repoRoot would lead to a broken
// `git -C <repoRoot> worktree remove`, so we refuse early.
func TestReadGTWYml_RejectsRelativeRepoRoot(t *testing.T) {
	wt := t.TempDir()

	doc := gtwYmlDoc{
		Mode: "remote", Issue: 1, Branch: "fix/1",
		Worktree: wt, RepoRoot: "relative/path", // not absolute
		Repo: "o/r", Provider: "github",
		CreatedAt: fixedNow(),
	}
	if err := writeYmlRaw(wt, doc); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := ReadGTWYml(wt); err == nil {
		t.Fatal("ReadGTWYml succeeded with relative repoRoot, want error")
	}
}

// TestReadGTWYml_RejectsEmptyRepoRoot verifies the
// RepoRoot-must-be-set invariant.
func TestReadGTWYml_RejectsEmptyRepoRoot(t *testing.T) {
	wt := t.TempDir()

	doc := gtwYmlDoc{
		Mode: "remote", Issue: 1, Branch: "fix/1",
		Worktree: wt, RepoRoot: "", // empty
		Repo: "o/r", Provider: "github",
		CreatedAt: fixedNow(),
	}
	if err := writeYmlRaw(wt, doc); err != nil {
		t.Fatalf("seed: %v", err)
	}

	if _, err := ReadGTWYml(wt); err == nil {
		t.Fatal("ReadGTWYml succeeded with empty repoRoot, want error")
	}
}

// TestReadGTWYml_NotExist verifies the missing-file case bubbles
// up os.ErrNotExist (callers turn this into "no active fix").
func TestReadGTWYml_NotExist(t *testing.T) {
	wt := t.TempDir()
	_, err := ReadGTWYml(wt)
	if err == nil {
		t.Fatal("ReadGTWYml on missing file returned nil err")
	}
	if !errors.Is(err, os.ErrNotExist) && !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("err = %v, want os.ErrNotExist-derived", err)
	}
}

// TestRemoveGTWYml_Idempotent verifies RemoveGTWYml is a no-op
// when the file is already missing (so RunClose can call it
// unconditionally as a belt-and-braces cleanup).
func TestRemoveGTWYml_Idempotent(t *testing.T) {
	wt := t.TempDir()

	// Missing → no-op success.
	if err := RemoveGTWYml(wt); err != nil {
		t.Fatalf("RemoveGTWYml on missing: %v", err)
	}

	// Then seed and remove.
	if err := WriteGTWYml(wt, Context{
		Mode: ModeLocal, Issue: -1, Branch: "b", Worktree: wt, RepoRoot: "/r",
		State: StateFixing, UpdatedAt: fixedNow(),
	}, fixedNow); err != nil {
		t.Fatalf("WriteGTWYml: %v", err)
	}
	if err := RemoveGTWYml(wt); err != nil {
		t.Fatalf("RemoveGTWYml: %v", err)
	}
	if _, err := os.Stat(gtwYmlPath(wt)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("gtw.yml still present after RemoveGTWYml: err=%v", err)
	}
}

// writeYmlRaw seeds a gtw.yml directly from a gtwYmlDoc literal
// (bypassing Context) so tests can construct intentionally
// broken files (empty repoRoot, relative repoRoot, etc.).
func writeYmlRaw(wt string, doc gtwYmlDoc) error {
	if err := os.MkdirAll(filepath.Join(wt, nightmeDirName), 0o755); err != nil {
		return err
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(gtwYmlPath(wt), out, 0o600)
}

// --- CommitGitignoreIfDirty ---
//
// These tests need a real (or fake) git binary because
// CommitGitignoreIfDirty shells out to `git status` /
// `git add` / `git commit`. The unit tests use a minimal
// fakeGit (defined in commitGitignore_test.go) that just
// answers "yes, dirty" for status, and asserts the commit
// args are well-formed. The end-to-end "real git" behavior
// is covered by TestIntegration_CommitGitignore in
// close_integration_test.go.