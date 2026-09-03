package gtw

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/pathutil"
	"gopkg.in/yaml.v3"
)

// gtwYmlDoc is the on-disk shape of `.nightme/gtw.yml`. Lives in
// the worktree created by `/gtw fix` (or written by the /gtw fix
// retry path after a successful WorktreeAdd). It is the
// cwd-scoped source of truth for everything /gtw does — there is
// no parallel in-memory state. /gtw close reads it back via
// toContext, which synthesises State=StateFixing / UpdatedAt=
// CreatedAt because those live-state-machine fields are not
// relevant once a fix has been written to disk (close tears down
// the worktree, ending the fix regardless of sub-state).
//
// Schema mirrors §14.3 of wip/gtw.md. Title/Slug are intentionally
// omitted from the on-disk shape — they live on FixDraftPayload
// (transient, in-memory) and aren't needed by /gtw close. When
// /gtw push lands, Title/Slug can either be re-pulled from the
// provider on demand or be promoted to Context fields at that
// time.
type gtwYmlDoc struct {
	Mode      string    `yaml:"mode"`
	Issue     int       `yaml:"issue"`
	Branch    string    `yaml:"branch"`
	Worktree  string    `yaml:"worktree"`
	RepoRoot  string    `yaml:"repoRoot"`
	Repo      string    `yaml:"repo"`
	Provider  string    `yaml:"provider"`
	CreatedAt time.Time `yaml:"createdAt"`
}

// toYmlDoc projects a Context into the on-disk shape. We don't
// store State / UpdatedAt on disk — see gtwYmlDoc's doc comment.
func (c Context) toYmlDoc(createdAt time.Time) gtwYmlDoc {
	return gtwYmlDoc{
		Mode:      string(c.Mode),
		Issue:     c.Issue,
		Branch:    c.Branch,
		Worktree:  c.Worktree,
		RepoRoot:  c.RepoRoot,
		Repo:      c.Repo,
		Provider:  c.Provider,
		CreatedAt: createdAt,
	}
}

// toContext hydrates a Context from a gtwYmlDoc. State defaults
// to StateFixing on read — the disk snapshot is always of a
// "in-flight" fix (yml only exists while fix → close is open).
// UpdatedAt is reset to CreatedAt so the next state transition
// fires a fresh timestamp.
func (d gtwYmlDoc) toContext() Context {
	return Context{
		Mode:      Mode(d.Mode),
		Issue:     d.Issue,
		Branch:    d.Branch,
		Worktree:  d.Worktree,
		RepoRoot:  d.RepoRoot,
		Repo:      d.Repo,
		Provider:  d.Provider,
		State:     StateFixing,
		UpdatedAt: d.CreatedAt,
	}
}

// NightmeDir is the per-worktree scratch directory holding gtw
// state. Kept package-private so callers can't bypass the
// Ensure/Write/Read helpers below.
const nightmeDirName = ".nightme"

// GtwYmlName is the canonical filename inside .nightme/.
const gtwYmlName = "gtw.yml"

// gtwYmlPath is a small helper used by both Write and Read.
// F-PATHUTIL-001 §13.3.1: route through pathutil.Join so the
// platform-specific separator handling is consistent with every
// other path operation in this package.
func gtwYmlPath(worktreePath string) string {
	return pathutil.Join(worktreePath, nightmeDirName, gtwYmlName)
}

// ErrGtwYmlExists is returned by WriteGTWYml when the file is
// already present — callers (RunFix) translate this into a
// "another fix in progress" reply.
var ErrGtwYmlExists = errors.New("gtw: .nightme/gtw.yml already exists")

// EnsureGitignore makes sure `<worktreePath>/.gitignore` lists
// `.nightme/` so the yml file inside that directory is not
// surfaced by `git status`.
//
// Idempotent:
//   - .gitignore missing              → create with `.nightme/\n`
//   - .gitignore present, has entry    → no-op
//   - .gitignore present, no entry     → append a line, preserve original content
//
// Each worktree has its own working tree, so this writes only to
// the worktree's .gitignore (not the main repo's). See wip/gtw.md
// §14.6 for the design rationale.
//
// NOTE: this only writes the file — it does NOT commit it. The
// caller (completeFixAndDispatch) follows up with
// CommitGitignore so the worktree ends up genuinely clean for
// `git worktree remove`. Keeping the write and the commit as
// separate steps lets tests cover each one in isolation.
func EnsureGitignore(worktreePath string) error {
	// F-PATHUTIL-001 §13.3.1: pathutil.Join for separator consistency.
	giPath := pathutil.Join(worktreePath, ".gitignore")

	existing, readErr := os.ReadFile(giPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return fmt.Errorf("read .gitignore: %w", readErr)
	}

	for _, raw := range strings.Split(string(existing), "\n") {
		trimmed := strings.TrimSpace(raw)
		// Match both the directory form (.nightme/) and the
		// explicit-file form, in case a future user edits it
		// manually. We don't expand globs or parse comments.
		if trimmed == ".nightme/" || trimmed == ".nightme/gtw.yml" {
			return nil
		}
	}

	f, err := os.OpenFile(giPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("open .gitignore: %w", err)
	}
	defer f.Close()

	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return fmt.Errorf("write newline before .nightme/ entry: %w", err)
		}
	}
	if _, err := f.WriteString(".nightme/\n"); err != nil {
		return fmt.Errorf("append .nightme/ to .gitignore: %w", err)
	}
	return nil
}

// gitToolIdentity is the per-command identity override used by
// CommitGitignoreIfDirty. We deliberately do NOT rely on the
// user's git config: gtw-managed commits need a stable author so
// the user can recognize / amend / squash them safely. The email
// is `gtw@localhost` rather than a real domain — gtw commits are
// always tooling artifacts, not human-signed work.
const (
	gitToolIdentityName  = "nightme-gtw"
	gitToolIdentityEmail = "gtw@localhost"
)

// CommitGitignoreIfDirty makes a tool-tagged commit on the
// worktree's branch staging any change EnsureGitignore made to
// `.gitignore`. This is what lets `git worktree remove` later
// succeed without `--force`: once .gitignore is committed, the
// worktree has no untracked / modified files from our side, so
// git considers it clean.
//
// No-op when .gitignore is unchanged from HEAD (e.g. EnsureGitignore
// was a no-op because the entry was already present).
//
// The commit author is forced to the gtw tool identity via `git -c
// user.name=... -c user.email=...` so the commit survives even
// when the user has no global git identity configured. The author
// makes it visually obvious in `git log` that this isn't a
// human-authored commit.
//
// Returns nil on success OR on no-op. Real errors (git not a
// repo, index corruption, etc.) propagate to the caller — RunClose
// logs the failure but does not abort /gtw fix, mirroring the
// "worktree is the durable side effect" pattern used elsewhere.
func CommitGitignoreIfDirty(ctx context.Context, worktreePath string, git GitRunner) error {
	// Quick exit: is .gitignore dirty?
	statusOut, _, err := git.Run(ctx, worktreePath,
		"status", "--porcelain", "--", ".gitignore")
	if err != nil {
		return fmt.Errorf("git status .gitignore: %w", err)
	}
	if strings.TrimSpace(statusOut) == "" {
		return nil // clean — nothing to commit
	}

	// Stage the file.
	if _, _, err := git.Run(ctx, worktreePath, "add", "--", ".gitignore"); err != nil {
		return fmt.Errorf("git add .gitignore: %w", err)
	}

	// Commit with the tool identity forced inline. We use `-c`
	// instead of relying on the user's global config so the
	// commit succeeds even when `git config user.email` is unset.
	_, _, err = git.Run(ctx, worktreePath,
		"-c", "user.name="+gitToolIdentityName,
		"-c", "user.email="+gitToolIdentityEmail,
		"commit", "-m", "gtw: ignore .nightme/",
		"--", ".gitignore")
	if err != nil {
		return fmt.Errorf("git commit .gitignore: %w", err)
	}
	return nil
}

// WriteGTWYml serializes c into `<worktreePath>/.nightme/gtw.yml`.
// Creates the .nightme/ directory if missing.
//
// Returns ErrGtwYmlExists if the file is already there — the
// single-fix-per-repo invariant (§14.3) means a pre-existing yml
// signals an unfinished /gtw close (or a concurrent chat).
// Callers should translate this into a "first run /gtw close"
// reply and not silently overwrite.
//
// F-PATHUTIL-001 §13.3.1: yml is the durable path carrier;
// normalize Worktree / RepoRoot at the write boundary so the
// on-disk form is platform-canonical regardless of which OS
// wrote it (git on Windows returns forward-slash paths, and a
// mixed / inconsistent yml triggers downstream `git worktree
// remove` "Invalid argument" errors). Normalize errors are
// non-fatal here — we fall back to the raw string, matching
// the read-side permissiveness.
func WriteGTWYml(worktreePath string, c Context, now func() time.Time) error {
	if worktreePath == "" {
		return errors.New("gtw: WriteGTWYml: empty worktreePath")
	}
	if now == nil {
		now = time.Now
	}

	target := gtwYmlPath(worktreePath)
	if _, err := os.Stat(target); err == nil {
		return ErrGtwYmlExists
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("stat gtw.yml: %w", err)
	}

	dir := pathutil.Dir(target)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir .nightme: %w", err)
	}

	doc := c.toYmlDoc(now().UTC())
	// Normalize at the write boundary so the on-disk form is
	// always the canonical OS-local form. ReadGTWYml also
	// normalizes defensively (in case the yml was hand-edited
	// or written by a different tool), but writing canonical
	// means round-trip identity: read(p) == p.
	if n, err := pathutil.NormalizeForOS(doc.Worktree); err == nil {
		doc.Worktree = n
	}
	if n, err := pathutil.NormalizeForOS(doc.RepoRoot); err == nil {
		doc.RepoRoot = n
	}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("marshal gtw.yml: %w", err)
	}

	// 0o600 — local state, never readable by other users.
	if err := os.WriteFile(target, out, 0o600); err != nil {
		return fmt.Errorf("write gtw.yml: %w", err)
	}
	return nil
}

// ReadGTWYml parses `<worktreePath>/.nightme/gtw.yml` back into
// a Context. Returns the underlying error (typically
// os.ErrNotExist) if the file is missing — callers should treat
// that as "no active fix in this worktree".
//
// F-PATHUTIL-001 §5.2: yml is the durable path carrier; both
// Worktree and RepoRoot are Normalized at the read boundary so
// downstream callers see the platform-canonical form regardless
// of how the yml was written (git emits forward slashes on
// Windows; some external tools emit backslashes; hand-edited
// yml is anyone's guess). Without this normalization, a yml
// written by `git rev-parse --show-toplevel` ("F:/...") is
// passed verbatim to `git worktree remove`, which fails on
// Windows with ERROR_INVALID_PARAMETER.
func ReadGTWYml(worktreePath string) (Context, error) {
	target := gtwYmlPath(worktreePath)
	raw, err := os.ReadFile(target)
	if err != nil {
		return Context{}, err
	}
	var doc gtwYmlDoc
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return Context{}, fmt.Errorf("parse gtw.yml: %w", err)
	}
	if doc.RepoRoot == "" {
		return Context{}, errors.New("gtw.yml: repoRoot is empty")
	}
	// F-PATHUTIL-001 §13.3.1: pathutil.IsAbs for cross-platform
	// consistency. On Windows the absolute check accepts drive-
	// rooted, root-relative, and UNC forms (the same set
	// pathutil.NormalizeForOS produces).
	if !pathutil.IsAbs(doc.RepoRoot) {
		return Context{}, fmt.Errorf("gtw.yml: repoRoot %q is not an absolute path", doc.RepoRoot)
	}
	// Normalize at the yml boundary so every downstream caller
	// (RunClose, deriveHookContext, preflightOrphanYml, …) can
	// treat Worktree / RepoRoot as already-canonical. Errors
	// from NormalizeForOS are non-fatal here: a malformed path
	// is already a "yml is malformed" case, and the previous
	// behaviour was to pass it through; we preserve that
	// permissiveness rather than failing the read.
	if n, err := pathutil.NormalizeForOS(doc.Worktree); err == nil {
		doc.Worktree = n
	}
	if n, err := pathutil.NormalizeForOS(doc.RepoRoot); err == nil {
		doc.RepoRoot = n
	}
	return doc.toContext(), nil
}

// RemoveGTWYml deletes the yml file. Used by RunClose as a
// fallback for the rare case where the worktree is gone but the
// yml somehow survived (e.g. user manually moved the directory).
// Idempotent — returns nil if the file is already missing.
func RemoveGTWYml(worktreePath string) error {
	if err := os.Remove(gtwYmlPath(worktreePath)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}