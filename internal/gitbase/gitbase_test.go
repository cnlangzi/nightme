package gitbase

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/proc"
)

// git runs a git command in dir and fails the test on error.
func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	c := proc.New(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

// gitTry runs a git command in dir and returns its output without
// failing the test — for the commands that are expected to fail.
func gitTry(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := proc.New(t.Context(), "git", append([]string{"-C", dir}, args...)...)
	out, _ := c.CombinedOutput()
	return strings.TrimSpace(string(out))
}

// newLocalRepo creates a repository with no remote.
func newLocalRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	git(t, dir, "init", "-b", "main")
	git(t, dir, "config", "user.email", "test@test.local")
	git(t, dir, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write file: %v", err)
	}
	git(t, dir, "add", "a.txt")
	git(t, dir, "commit", "-m", "init")
	return dir
}

// newBareRemote creates a bare repository holding one commit on main.
func newBareRemote(t *testing.T) string {
	t.Helper()
	base := t.TempDir()
	bare := filepath.Join(base, "remote.git")
	git(t, base, "init", "--bare", "-b", "main", bare)

	seed := filepath.Join(base, "seed")
	git(t, seed, "init", "-b", "main")
	git(t, seed, "config", "user.email", "test@test.local")
	git(t, seed, "config", "user.name", "test")
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("hello\n"), 0o600); err != nil {
		t.Fatalf("write seed file: %v", err)
	}
	git(t, seed, "add", "a.txt")
	git(t, seed, "commit", "-m", "init")
	git(t, seed, "remote", "add", "origin", bare)
	git(t, seed, "push", "origin", "main")
	return bare
}

// addOrigin points repo at origin and fetches, so the local
// remote-tracking ref exists.
//
// The origin/HEAD ref is removed afterwards: some git versions create
// it on fetch, and the repair path under test needs it absent.
func addOrigin(t *testing.T, repo, origin string) {
	t.Helper()
	git(t, repo, "remote", "add", "origin", origin)
	git(t, repo, "fetch", "origin")
	removeOriginHead(t, repo)
}

// removeOriginHead deletes refs/remotes/origin/HEAD in whichever form
// it exists — symbolic or direct.
func removeOriginHead(t *testing.T, repo string) {
	t.Helper()
	gitTry(t, repo, "symbolic-ref", "-d", "refs/remotes/origin/HEAD")
	gitTry(t, repo, "update-ref", "--no-deref", "-d", "refs/remotes/origin/HEAD")
}

// TestResolve_SymbolicRef covers the offline tier: a repository that
// already has refs/remotes/origin/HEAD resolves without contacting the
// remote — the origin URL here cannot be reached at all.
func TestResolve_SymbolicRef(t *testing.T) {
	dir := newLocalRepo(t)
	git(t, dir, "remote", "add", "origin", filepath.Join(dir, "does-not-exist.git"))
	git(t, dir, "update-ref", "refs/remotes/origin/main", "HEAD")
	git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	res := Resolve(context.Background(), dir, Options{Repair: true})
	if res.Source != SourceSymbolicRef {
		t.Fatalf("Source = %v, want SourceSymbolicRef", res.Source)
	}
	if res.Branch != "main" {
		t.Errorf("Branch = %q, want %q", res.Branch, "main")
	}
	if res.WroteHead {
		t.Error("WroteHead = true, want false — the symbolic-ref tier writes nothing")
	}
	if !res.HasRemote {
		t.Error("HasRemote = false, want true")
	}
}

// TestResolve_ExistingHeadUntouched pins the "never overwrite" rule:
// an existing refs/remotes/origin/HEAD is left byte-identical.
func TestResolve_ExistingHeadUntouched(t *testing.T) {
	dir := newLocalRepo(t)
	remote := newBareRemote(t)
	addOrigin(t, dir, remote)
	git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/main")

	headFile := filepath.Join(dir, ".git", "refs", "remotes", "origin", "HEAD")
	before, err := os.ReadFile(headFile)
	if err != nil {
		t.Fatalf("read origin/HEAD: %v", err)
	}

	res := Resolve(context.Background(), dir, Options{Repair: true})
	if res.Source != SourceSymbolicRef {
		t.Fatalf("Source = %v, want SourceSymbolicRef", res.Source)
	}
	if res.WroteHead {
		t.Error("WroteHead = true, want false")
	}
	after, err := os.ReadFile(headFile)
	if err != nil {
		t.Fatalf("read origin/HEAD: %v", err)
	}
	if string(before) != string(after) {
		t.Errorf("origin/HEAD changed:\n before %q\n after  %q", before, after)
	}
}

// TestResolve_RemoteQueryRepairs covers the remote tier: the branch is
// learned from the remote, the missing ref is created, and a second
// resolution is offline.
func TestResolve_RemoteQueryRepairs(t *testing.T) {
	dir := newLocalRepo(t)
	remote := newBareRemote(t)
	addOrigin(t, dir, remote)

	res := Resolve(context.Background(), dir, Options{Repair: true})
	if res.Source != SourceRepaired {
		t.Fatalf("Source = %v, want SourceRepaired", res.Source)
	}
	if res.Branch != "main" {
		t.Errorf("Branch = %q, want %q", res.Branch, "main")
	}
	if !res.WroteHead {
		t.Error("WroteHead = false, want true")
	}
	if !res.HasRemote {
		t.Error("HasRemote = false, want true")
	}

	want := "refs/remotes/origin/main"
	if got := git(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); got != want {
		t.Errorf("origin/HEAD = %q, want %q", got, want)
	}

	second := Resolve(context.Background(), dir, Options{Repair: true})
	if second.Source != SourceSymbolicRef {
		t.Errorf("second Source = %v, want SourceSymbolicRef", second.Source)
	}
	if second.WroteHead {
		t.Error("second WroteHead = true, want false")
	}
}

// TestResolve_RemoteQueryNoRepair covers the read-only mode /me uses:
// the branch is learned, but no ref is created.
func TestResolve_RemoteQueryNoRepair(t *testing.T) {
	dir := newLocalRepo(t)
	remote := newBareRemote(t)
	addOrigin(t, dir, remote)

	res := Resolve(context.Background(), dir, Options{Repair: false})
	if res.Source != SourceRemoteQuery {
		t.Fatalf("Source = %v, want SourceRemoteQuery", res.Source)
	}
	if res.Branch != "main" {
		t.Errorf("Branch = %q, want %q", res.Branch, "main")
	}
	if res.WroteHead {
		t.Error("WroteHead = true, want false")
	}
	if out := gitTry(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); !strings.Contains(out, "not a symbolic ref") {
		t.Errorf("origin/HEAD = %q, want it to stay absent", out)
	}
}

// TestResolve_DirectHeadIsNotOverwritten covers a repository whose
// origin/HEAD exists but is not a symbolic ref: the repair leaves it
// alone, and the branch is still learned from the remote.
func TestResolve_DirectHeadIsNotOverwritten(t *testing.T) {
	dir := newLocalRepo(t)
	remote := newBareRemote(t)
	addOrigin(t, dir, remote)
	git(t, dir, "update-ref", "refs/remotes/origin/HEAD", "HEAD")

	res := Resolve(context.Background(), dir, Options{Repair: true})
	if res.Branch != "main" {
		t.Errorf("Branch = %q, want %q", res.Branch, "main")
	}
	if res.WroteHead {
		t.Error("WroteHead = true, want false — an existing ref is never overwritten")
	}
	if out := gitTry(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); !strings.Contains(out, "not a symbolic ref") {
		t.Errorf("origin/HEAD = %q, want the existing direct ref left alone", out)
	}
}

// TestResolve_UnreachableRemote covers a repository whose origin does
// not answer: the base stays unknown, but the repository is known to
// have a remote — the caller decides what that means.
func TestResolve_UnreachableRemote(t *testing.T) {
	dir := newLocalRepo(t)
	git(t, dir, "remote", "add", "origin", filepath.Join(dir, "does-not-exist.git"))

	res := Resolve(context.Background(), dir, Options{Repair: true})
	if res.Source != SourceUnknown {
		t.Fatalf("Source = %v, want SourceUnknown", res.Source)
	}
	if res.Branch != "" {
		t.Errorf("Branch = %q, want empty", res.Branch)
	}
	if !res.HasRemote {
		t.Error("HasRemote = false, want true")
	}
	if res.Reason != ReasonRemoteUnreachable {
		t.Errorf("Reason = %q, want %q", res.Reason, ReasonRemoteUnreachable)
	}
	if res.WroteHead {
		t.Error("WroteHead = true, want false")
	}
}

// TestResolve_NoRemote covers a repository without an origin remote:
// there is no base, and the working tree is the only scope.
func TestResolve_NoRemote(t *testing.T) {
	dir := newLocalRepo(t)

	res := Resolve(context.Background(), dir, Options{Repair: true})
	if res.HasRemote {
		t.Error("HasRemote = true, want false")
	}
	if res.Branch != "" {
		t.Errorf("Branch = %q, want empty", res.Branch)
	}
	if res.Source != SourceUnknown {
		t.Errorf("Source = %v, want SourceUnknown", res.Source)
	}
	if res.Reason != ReasonNoRemote {
		t.Errorf("Reason = %q, want %q", res.Reason, ReasonNoRemote)
	}
}

// TestResolve_MissingTrackingRef covers a reachable remote whose
// branch was never fetched: the base ref cannot resolve, so no ref is
// written.
func TestResolve_MissingTrackingRef(t *testing.T) {
	dir := newLocalRepo(t)
	remote := newBareRemote(t)
	git(t, dir, "remote", "add", "origin", remote)

	res := Resolve(context.Background(), dir, Options{Repair: true})
	if res.Reason != ReasonMissingTrackingRef {
		t.Fatalf("Reason = %q, want %q", res.Reason, ReasonMissingTrackingRef)
	}
	if res.Branch != "" {
		t.Errorf("Branch = %q, want empty", res.Branch)
	}
	if !res.HasRemote {
		t.Error("HasRemote = false, want true")
	}
	if out := gitTry(t, dir, "symbolic-ref", "refs/remotes/origin/HEAD"); !strings.Contains(out, "not a symbolic ref") {
		t.Errorf("origin/HEAD = %q, want it to stay absent", out)
	}
}

// TestResolve_FailedWriteIsNotFatal covers a failed repair write: the
// branch learned from the remote is still returned.
func TestResolve_FailedWriteIsNotFatal(t *testing.T) {
	dir := newLocalRepo(t)
	remote := newBareRemote(t)
	addOrigin(t, dir, remote)

	// A lock file in the way makes git's ref write fail.
	lock := filepath.Join(dir, ".git", "refs", "remotes", "origin", "HEAD.lock")
	if err := os.MkdirAll(lock, 0o755); err != nil {
		t.Fatalf("create lock dir: %v", err)
	}

	res := Resolve(context.Background(), dir, Options{Repair: true})
	if res.Branch != "main" {
		t.Errorf("Branch = %q, want %q", res.Branch, "main")
	}
	if res.WroteHead {
		t.Error("WroteHead = true, want false")
	}
	if res.Source != SourceRemoteQuery {
		t.Errorf("Source = %v, want SourceRemoteQuery", res.Source)
	}
}

// TestResolve_QueryTimeoutOption covers the remote-query budget: the
// option carries it, and an unresponsive remote does not block past
// it.
func TestResolve_QueryTimeoutOption(t *testing.T) {
	if DefaultQueryTimeout != 20*time.Second {
		t.Fatalf("DefaultQueryTimeout = %v, want 20s", DefaultQueryTimeout)
	}

	dir := newLocalRepo(t)
	git(t, dir, "remote", "add", "origin", filepath.Join(dir, "does-not-exist.git"))

	start := time.Now()
	res := Resolve(context.Background(), dir, Options{QueryTimeout: 2 * time.Second})
	elapsed := time.Since(start)
	if res.Source != SourceUnknown {
		t.Fatalf("Source = %v, want SourceUnknown", res.Source)
	}
	if elapsed > 30*time.Second {
		t.Errorf("resolution took %v, want it bounded by the query budget", elapsed)
	}
}

// TestResolve_EmptyWorkspace covers a workspace that is not a
// repository at all.
func TestResolve_EmptyWorkspace(t *testing.T) {
	res := Resolve(context.Background(), "", Options{Repair: true})
	if res.HasRemote || res.Branch != "" || res.Source != SourceUnknown {
		t.Errorf("resolution = %+v, want an unknown base", res)
	}
}

// TestParseSymrefHead covers the `git ls-remote --symref` parser.
func TestParseSymrefHead(t *testing.T) {
	out := "ref: refs/heads/main\tHEAD\nc7abca26271058f4c286fdc054568d7b72da7e06\tHEAD\n"
	branch, ok := parseSymrefHead(out)
	if !ok || branch != "main" {
		t.Fatalf("parseSymrefHead = (%q, %v), want (main, true)", branch, ok)
	}
	if _, ok := parseSymrefHead("c7abca26271058f4c286fdc054568d7b72da7e06\tHEAD\n"); ok {
		t.Error("parseSymrefHead accepted a payload without a symref line")
	}
	if _, ok := parseSymrefHead("ref: refs/tags/v1.0\tHEAD\n"); ok {
		t.Error("parseSymrefHead accepted a non-branch target")
	}
}

// TestParseRemoteShowHead covers the `git remote show origin` parser —
// the fallback for servers that do not answer --symref.
func TestParseRemoteShowHead(t *testing.T) {
	out := `* remote origin
  Fetch URL: /tmp/remote.git
  Push  URL: /tmp/remote.git
  HEAD branch: main
  Remote branch:
    main tracked
`
	branch, ok := parseRemoteShowHead(out)
	if !ok || branch != "main" {
		t.Fatalf("parseRemoteShowHead = (%q, %v), want (main, true)", branch, ok)
	}
	if _, ok := parseRemoteShowHead("  HEAD branch: (unknown)\n"); ok {
		t.Error("parseRemoteShowHead accepted an unknown branch")
	}
	if _, ok := parseRemoteShowHead("  HEAD branch:\n"); ok {
		t.Error("parseRemoteShowHead accepted an empty branch")
	}
}
