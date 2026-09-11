// Package nightmedir owns the `.nightme` namespace that nightme
// uses for runtime-generated state. Two distinct scopes share
// the name:
//
//   - Per-cwd: `<cwd>/.nightme/` — state that belongs to one
//     working directory (gtw state, future per-project caches).
//     Owned by the existing Path / FilePath / EnsureDir /
//     EnsureGitignoreEntry surface; not usable for cross-cwd
//     handoff documents because each cwd would carve its own
//     copy out of git status and lose them on cleanup.
//   - Per-user: `$HOME/.nightme/` — state that belongs to the
//     user across all cwds (config, Feishu inbox, bot workflow
//     state, named handoff documents). The handoff helpers at
//     the bottom of this file are the per-user subset the
//     /handoff and /resume commands need.
//
// Filenames inside either directory are owned by each calling
// command (gtw.yml is gtw's, <name>.md is the handoff command's,
// etc.). This package owns only the directory names, the
// per-cwd .gitignore entry, and the per-user handoff-dir creation
// helper. Everything else is caller's responsibility.
package nightmedir

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/cnlangzi/nightme/internal/pathutil"
)

// DirName is the per-cwd nightme directory name. Hardcoded across
// the codebase — every per-cwd nightme file lives under this
// directory so a single .gitignore entry covers them all.
//
// Follows the same convention as other CLI tools: .git/, .claude/,
// .codex/, .docker/, .terraform/. Dot-prefixed directory names are
// the de-facto standard for "tool-owned runtime state under a
// project root" and are excluded from `git status` by default
// when the repo's ignore-file chain treats them correctly (see
// EnsureGitignoreEntry below).
const DirName = ".nightme"

// gitignoreEntry is the literal line written into .gitignore to
// keep the directory out of `git status`. Trailing slash pins the
// form to a directory, which avoids accidentally matching a same-
// named file (`.nightme` is legal on POSIX; `.nightme/` rejects
// it). Matches what gtw/persist.go has been writing since F-gtw.
const gitignoreEntry = DirName + "/"

// Path returns the absolute path to the per-cwd nightme
// directory for cwd. Forward-slash separator regardless of
// platform — matches what users see on every channel's IM card
// reply (Feishu / Slack / Telegram / bot all render forward
// slashes).
//
// cwd must be a non-empty absolute path; the function does not
// resolve relative paths or expand ~. Callers typically obtain
// cwd from cs.SelectedCwd(), which already enforces the absolute
// form via /cwd's resolver.
func Path(cwd string) string {
	return pathutil.Join(cwd, DirName)
}

// FilePath returns the absolute path to a specific file inside
// the per-cwd nightme directory, platform-canonical via
// pathutil.Join. Provided as a convenience for callers that want
// a single source of truth for "<cwd>/.nightme/<name>" joins;
// the on-disk file itself is the caller's responsibility (this
// package doesn't validate that name is a "well-known" filename
// — it joins whatever the caller passes).
//
// Use FilePath for filesystem calls (os.Stat, os.Open). Use
// RelPath for user-visible text where forward-slash consistency
// is required regardless of platform.
func FilePath(cwd, name string) string {
	return pathutil.Join(cwd, DirName, name)
}

// RelPath returns the slash-form relative path to a file inside
// the per-cwd nightme directory, for embedding in user-visible
// reply text (Feishu / Slack / Telegram / bot IM cards all render
// forward slashes regardless of host platform, so platform-
// canonical `pathutil.Join` output would look wrong to the user).
//
// Symmetric with FilePath but always emits "/" — never run this
// value through os.Stat or os.Open; use FilePath for those.
func RelPath(name string) string {
	return DirName + "/" + name
}

// EnsureDir creates <cwd>/.nightme/ if absent. Idempotent —
// returns nil when the directory already exists.
//
// cwd must already exist; this function does NOT create
// intermediate directories above .nightme. The narrow contract
// matters because callers get an explicit error when they pass a
// cwd that doesn't exist on disk yet, rather than a surprise
// directory tree silently materialising under the user's home.
// This package makes the directory, not the path to it.
//
// Mode is 0o755 — readable by everyone (the .gitignore entry
// keeps the contents out of git; the perms just let tooling like
// `ls` work in a multi-user setup). Per-cwd data is not
// considered sensitive; secret material (Feishu AppSecret,
// inbox attachments) lives under $HOME/.nightme/ with 0o700
// perms, which is a separate concern owned by cmd/nightme/clean.go.
//
// Implementation: stat the path first to absorb the existing-
// directory case without hitting Mkdir's EEXIST. We don't use
// MkdirAll because we explicitly do NOT want parent creation as
// a side effect — see the doc comment above.
func EnsureDir(cwd string) error {
	p := pathutil.Join(cwd, DirName)
	info, err := os.Stat(p)
	if err == nil {
		if info.IsDir() {
			return nil
		}
		return &os.PathError{Op: "ensure", Path: p, Err: errNotDirectory}
	}
	if !os.IsNotExist(err) {
		return err
	}
	if err := os.Mkdir(p, 0o755); err != nil {
		// Race: another caller created it between Stat and Mkdir.
		// Stat again to confirm it's a directory now.
		if os.IsExist(err) {
			if info, statErr := os.Stat(p); statErr == nil && info.IsDir() {
				return nil
			}
		}
		return err
	}
	return nil
}

// errNotDirectory is returned by EnsureDir when the target path
// exists but is not a directory (e.g. a regular file with the
// same name got placed there).
var errNotDirectory = notDirectoryErr{}

type notDirectoryErr struct{}

func (notDirectoryErr) Error() string { return "not a directory" }

// EnsureGitignoreEntry makes sure `<cwd>/.gitignore` lists the
// nightme directory so `git status` doesn't surface runtime
// files written inside it.
//
// Idempotent:
//
//   - .gitignore missing → created with just the entry.
//   - .gitignore present, no matching entry → entry appended
//     (preserves existing content; adds a leading newline if the
//     last existing line doesn't end with one).
//   - .gitignore present, matching entry already there → no-op.
//
// Matches both the directory form (.nightme/) and an explicit-
// file form (.nightme/<name>) in case a user has manually
// tightened the entry. We don't parse .gitignore syntax — just
// line-equality after TrimSpace — which is enough for the
// patterns nightme's own runtime files use.
//
// Does NOT commit the change. Callers that need a clean worktree
// for `git worktree remove --force` (gtw does) follow up with
// their own commit using a per-command tool identity. Keeping
// the write and the commit separate lets each caller pick its
// own policy and lets tests cover each in isolation.
func EnsureGitignoreEntry(cwd string) error {
	giPath := pathutil.Join(cwd, ".gitignore")

	existing, readErr := os.ReadFile(giPath)
	if readErr != nil && !os.IsNotExist(readErr) {
		return readErr
	}

	for _, raw := range strings.Split(string(existing), "\n") {
		trimmed := strings.TrimSpace(raw)
		// Match the directory form (.nightme/) and any explicit
		// file inside the directory — both keep the directory's
		// contents out of git status. We don't expand globs or
		// parse comments; line-equality is enough for the
		// patterns nightme's runtime writes produce.
		if trimmed == gitignoreEntry || strings.HasPrefix(trimmed, DirName+"/") {
			return nil
		}
	}

	f, err := os.OpenFile(giPath, os.O_APPEND|os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()

	if len(existing) > 0 && !strings.HasSuffix(string(existing), "\n") {
		if _, err := f.WriteString("\n"); err != nil {
			return err
		}
	}
	if _, err := f.WriteString(gitignoreEntry + "\n"); err != nil {
		return err
	}
	return nil
}

// HandoffDirName is the per-user subdirectory of $HOME/.nightme
// where named handoff documents live. Kept distinct from the
// per-cwd ".nightme/" so a /handoff written from one cwd can be
// /resume'd from a different cwd — the whole reason /handoff
// exists is to survive a cwd switch, and storing it under
// <cwd>/.nightme/ would defeat the purpose.
const HandoffDirName = "handoff"

// HandoffFileSuffix is the on-disk extension RenderedHandoffFilename
// appends. Single source of truth for the extension so /handoff
// and /resume cannot drift on a rename.
const HandoffFileSuffix = ".md"

// handoffNameMaxLen caps ValidateHandoffName at 64 characters —
// long enough for "nightme-gtw-fix-2026-08-02-claude-opus" and
// short enough that a user reading the path aloud doesn't have
// to take a breath.
const handoffNameMaxLen = 64

// HandoffDir returns the absolute path to the per-user handoff
// directory: $HOME/.nightme/handoff. Does NOT create it —
// /handoff owns EnsureHandoffDir so the per-user .nightme is
// only materialised when at least one handoff has been written.
//
// Returns the os.UserHomeDir error wrapped, so callers can
// surface "HOME unset" without re-importing os.
func HandoffDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve home: %w", err)
	}
	return pathutil.Join(home, DirName, HandoffDirName), nil
}

// HandoffFilePath returns the absolute path to a specific named
// handoff document: $HOME/.nightme/handoff/<name>.md. Caller
// owns directory creation (EnsureHandoffDir) and name validation
// (ValidateHandoffName); both must run before this join lands
// anywhere on disk.
func HandoffFilePath(name string) (string, error) {
	dir, err := HandoffDir()
	if err != nil {
		return "", err
	}
	return pathutil.Join(dir, name+HandoffFileSuffix), nil
}

// EnsureHandoffDir creates $HOME/.nightme/handoff if absent.
// Uses MkdirAll (not the narrow EnsureDir contract) so the
// per-user .nightme can be created lazily — calling /handoff
// before any other nightme tool has run is a valid first-run
// path. 0o700 matches the inbox convention from
// internal/channel/feishu/attachment.go — even though handoff
// docs are not secret, keeping the .nightme tree uniform means
// `ls -la` on the directory doesn't surface a perm-bumped outlier.
func EnsureHandoffDir() error {
	dir, err := HandoffDir()
	if err != nil {
		return err
	}
	return os.MkdirAll(dir, 0o700)
}

// ValidateHandoffName is the single source of truth for the
// <name> character set. Both /handoff and /resume call it before
// any filesystem op so a malicious or malformed name never
// reaches pathutil.Join. Character set is intentionally narrow:
// ASCII letters, digits, '.', '_', '-'. Mirrors git refnames
// minus the slash allowance (slashes would let a name escape the
// handoff directory). Also rejects the bookkeeping sentinels
// ('.' and '..') and any leading '-' or '.' so a name cannot
// masquerade as a flag or a hidden file.
func ValidateHandoffName(name string) error {
	if name == "" {
		return errors.New("name must not be empty")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("%q is reserved", name)
	}
	if len(name) > handoffNameMaxLen {
		return fmt.Errorf("name must be at most %d characters (got %d)",
			handoffNameMaxLen, len(name))
	}
	if strings.HasPrefix(name, ".") {
		return errors.New("name must not start with '.'")
	}
	if strings.HasPrefix(name, "-") {
		return errors.New("name must not start with '-'")
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
		default:
			return fmt.Errorf("invalid character %q in name (only letters, digits, '_', '-' allowed)", r)
		}
	}
	return nil
}

// HandoffRelPath renders a slash-form, host-friendly
// `~/.nightme/handoff/<name>.md` for user-visible reply text
// (Feishu / Slack / Telegram / bot IM cards all render forward
// slashes regardless of host platform, so platform-canonical
// pathutil.Join output would look wrong to the user).
//
// Caller must pass a name that ValidateHandoffName has already
// accepted — HandoffRelPath does not re-validate, so a stray
// '/' in name would render as an escaped path the user can't
// follow.
func HandoffRelPath(name string) string {
	return DirName + "/" + HandoffDirName + "/" + name + HandoffFileSuffix
}
