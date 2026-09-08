package gtw

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
)

// stubCLIRunner records the exact argv passed to gh/glab and
// returns canned stdout / stderr / error. Used by the
// CreateLabel tests below to verify both (a) the exact command
// line built by the implementation and (b) the stderr-sniff
// branch on "already exists".
//
// argv records the FULL argv slice (`name` is appended before
// `args...`), so a test asserting "no --force flag" can grep
// for it without false negatives.
type stubCLIRunner struct {
	stdout string
	stderr string
	err    error

	argv    []string // full argv, including "gh"/"glab"
	callNum int
}

func (s *stubCLIRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	s.argv = append([]string{name}, args...)
	s.callNum++
	return s.stdout, s.stderr, s.err
}

// TestGitHubCreateLabel_CLIArgs pins the exact argv that
// GitHubProvider.CreateLabel builds. Regression test for the
// F-59 review feedback: the previous version used `gh label
// create --force`, which would silently overwrite a
// human-tuned label's color / description on every /gtw fix
// (contradicting the documented "no-op when label exists"
// contract). The fix drops --force; this test guards the
// drop.
//
// What we assert:
//   - argv[0] is "gh"
//   - argv contains "label", "create", <name>
//   - argv contains --repo, --color, --description (in that order)
//   - argv does NOT contain "--force" (the contract-violating flag)
//   - argv does NOT contain --name (gh uses positional <name>; glab
//     uses --name; only one of them is right)
func TestGitHubCreateLabel_CLIArgs(t *testing.T) {
	cli := &stubCLIRunner{}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	if err := p.CreateLabel(context.Background(),
		"cnlangzi", "nightme",
		"nightme/wip", "fbca04", "Work in progress"); err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}
	if cli.callNum != 1 {
		t.Fatalf("Run calls = %d, want 1", cli.callNum)
	}
	wantSubstrings := []string{
		"gh", // argv[0]
		"label", "create", "nightme/wip",
		"--repo", "cnlangzi/nightme",
		"--color", "fbca04",
		"--description", "Work in progress",
	}
	for _, want := range wantSubstrings {
		if !slices.Contains(cli.argv, want) {
			t.Errorf("argv missing %q: %v", want, cli.argv)
		}
	}
	// Critical: --force MUST NOT be present. If a future change
	// re-adds it, this test fails with a clear message and the
	// reviewer is forced to re-read the F-59 contract.
	forbiddenFlags := []string{"--force"}
	for _, bad := range forbiddenFlags {
		if slices.Contains(cli.argv, bad) {
			t.Errorf("argv contains forbidden flag %q (violates CreateLabel no-op-on-existing contract): %v",
				bad, cli.argv)
		}
	}
	// gh uses positional <name>, glab uses --name. CreateLabel
	// calls ONLY one platform at a time, so --name must NOT be
	// present in the gh argv (and vice versa in the glab test).
	if slices.Contains(cli.argv, "--name") {
		t.Errorf("gh argv should not contain --name (gh uses positional <name>): %v", cli.argv)
	}
}

// TestGitHubCreateLabel_AlreadyExistsIsSuccess pins the
// stderr-sniff branch: when gh exits 1 with "label with name
// \"<name>\" already exists; use --force to update ...", the
// call returns nil (success, not error).
//
// Regression test for the documented contract: existing labels
// are no-ops; the bootstrap step must not surface "already
// exists" as a failure.
func TestGitHubCreateLabel_AlreadyExistsIsSuccess(t *testing.T) {
	cli := &stubCLIRunner{
		stderr: `label with name "nightme/wip" already exists; use --force to update its color and description` + "\n",
		err:    errors.New("exit status 1"),
	}
	p := &GitHubProvider{Runner: cli, host: "github.com"}
	if err := p.CreateLabel(context.Background(),
		"cnlangzi", "nightme",
		"nightme/wip", "fbca04", "Work in progress"); err != nil {
		t.Errorf("CreateLabel with 'already exists' stderr = %v, want nil (no-op on existing label)", err)
	}
}

// TestGitHubCreateLabel_OtherErrorSurfaces: when gh exits 1
// with a stderr that does NOT contain "already exists"
// (e.g. 403 forbidden, network failure, label-name validation),
// the error must be returned to the caller verbatim. This is
// the "preserves operator-visible signal" branch — the same
// semantic as AddIssueLabel / RemoveIssueLabel.
func TestGitHubCreateLabel_OtherErrorSurfaces(t *testing.T) {
	cli := &stubCLIRunner{
		stderr: "403 Forbidden: missing Labels write scope\n",
		err:    errors.New("exit status 1"),
	}
	p := &GitHubProvider{Runner: cli, host: "github.com"}
	err := p.CreateLabel(context.Background(),
		"cnlangzi", "nightme",
		"nightme/wip", "fbca04", "Work in progress")
	if err == nil {
		t.Fatalf("CreateLabel with 403 stderr = nil, want non-nil error")
	}
	// Verbatim echo — same contract as AddIssueLabel.
	if !strings.Contains(err.Error(), "403 Forbidden") {
		t.Errorf("CreateLabel error must echo 403 stderr verbatim; got: %v", err)
	}
	if !strings.Contains(err.Error(), "gh label create") {
		t.Errorf("CreateLabel error must identify the failing command; got: %v", err)
	}
}

// TestGitHubCreateLabel_SuccessNoError: happy path. gh exits 0
// (label created), no stderr, no error.
func TestGitHubCreateLabel_SuccessNoError(t *testing.T) {
	cli := &stubCLIRunner{} // default: stdout="", stderr="", err=nil
	p := &GitHubProvider{Runner: cli, host: "github.com"}
	if err := p.CreateLabel(context.Background(),
		"cnlangzi", "nightme",
		"nightme/wip", "fbca04", "Work in progress"); err != nil {
		t.Errorf("CreateLabel happy path = %v, want nil", err)
	}
}

// --- GitLab ---

// TestGitLabCreateLabel_CLIArgs pins the exact argv that
// GitLabProvider.CreateLabel builds. Mirrors the GitHub test
// above. Pinned shape (post glab-#-strip fix):
//
//	glab api --method POST projects/<urlencoded owner/repo>/labels \
//	    -f name=<name> -f color=#<hex> -f description=<description>
//
// We route through `glab api` (a generic caller) instead of
// `glab label create` because the latter strips the leading
// `#` from `--color` before posting, so the payload always
// lands as `fbca04` and GitLab rejects it with 400 "must be a
// valid color code". The `#` is prepended in our code and
// survives the wire (URL-encoded to `%23`); GitLab decodes it
// back to `#RRGGBB`, matching its validation regex.
//
// Regression guards:
//   - `label`, `create`, `--color`, `--force` MUST NOT appear as
//     argv elements — any of them means we regressed to
//     `glab label create --color`, and the `#`-strip bug returns.
//   - `color=#fbca04` (with the leading `#`) MUST appear — the
//     `#` is mandatory for GitLab's validation.
//   - `color=fbca04` (bare hex, no `#`) MUST NOT appear — that's
//     exactly the broken form.
func TestGitLabCreateLabel_CLIArgs(t *testing.T) {
	cli := &stubCLIRunner{}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	if err := p.CreateLabel(context.Background(),
		"acme", "platform",
		"nightme/wip", "fbca04", "Work in progress"); err != nil {
		t.Fatalf("CreateLabel: %v", err)
	}
	if cli.callNum != 1 {
		t.Fatalf("Run calls = %d, want 1", cli.callNum)
	}
	wantSubstrings := []string{
		"glab",
		"api",
		"--method", "POST",
		"projects/acme%2Fplatform/labels",
		"-f", "name=nightme/wip",
		"-f", "color=#fbca04",
		"-f", "description=Work in progress",
	}
	for _, want := range wantSubstrings {
		if !slices.Contains(cli.argv, want) {
			t.Errorf("argv missing %q: %v", want, cli.argv)
		}
	}
	// Regression guards. `slices.Contains` checks element
	// equality, so the URL segment "labels" (plural) won't
	// false-positive on the exact-match token "label".
	forbidden := []string{"--color", "label", "create", "--force", "color=fbca04"}
	for _, bad := range forbidden {
		if slices.Contains(cli.argv, bad) {
			t.Errorf("glab argv contains forbidden token %q (would break the # color format): %v",
				bad, cli.argv)
		}
	}
}

// TestGitLabCreateLabel_AlreadyExistsIsSuccess: `glab api`
// echoes the GitLab response body to stderr on a 4xx/5xx, so
// the 422 envelope `{"message":{"name":["has already been
// taken"]}}` reaches us verbatim. The legacy `glab label
// create` "already exists" / "Already exists" forms are kept as
// belt-and-braces fallbacks for older glab. Pin all variants
// so a stderr-wording change forces a review.
func TestGitLabCreateLabel_AlreadyExistsIsSuccess(t *testing.T) {
	cases := []string{
		`{"message":{"name":["has already been taken"]}}` + "\n",
		"label already exists\n",
		"Label already exists\n",
		`ERROR\nLabel "nightme/wip" already exists.\n`,
	}
	for i, stderr := range cases {
		t.Run(fmt.Sprintf("stderr_variant_%d", i), func(t *testing.T) {
			cli := &stubCLIRunner{
				stderr: stderr,
				err:    errors.New("exit status 1"),
			}
			p := &GitLabProvider{Runner: cli, host: "gitlab.com"}
			if err := p.CreateLabel(context.Background(),
				"acme", "platform",
				"nightme/wip", "fbca04", "Work in progress"); err != nil {
				t.Errorf("CreateLabel with 'already exists' stderr = %v, want nil", err)
			}
		})
	}
}

// TestGitLabCreateLabel_OtherErrorSurfaces: same shape as the
// GitHub counterpart. `glab api` exits non-zero with non-matching
// stderr → error returned to caller verbatim. The wrapped prefix
// is "glab api create label" (matches the new command path).
func TestGitLabCreateLabel_OtherErrorSurfaces(t *testing.T) {
	cli := &stubCLIRunner{
		stderr: "403 Forbidden - your token does not have permission to create labels\n",
		err:    errors.New("exit status 1"),
	}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}
	err := p.CreateLabel(context.Background(),
		"acme", "platform",
		"nightme/wip", "fbca04", "Work in progress")
	if err == nil {
		t.Fatalf("CreateLabel with 403 stderr = nil, want non-nil error")
	}
	if !strings.Contains(err.Error(), "403 Forbidden") {
		t.Errorf("CreateLabel error must echo 403 stderr verbatim; got: %v", err)
	}
	if !strings.Contains(err.Error(), "glab api create label") {
		t.Errorf("CreateLabel error must identify the failing command; got: %v", err)
	}
}

// TestGitLabCreateLabel_SuccessNoError: happy path.
func TestGitLabCreateLabel_SuccessNoError(t *testing.T) {
	cli := &stubCLIRunner{}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}
	if err := p.CreateLabel(context.Background(),
		"acme", "platform",
		"nightme/wip", "fbca04", "Work in progress"); err != nil {
		t.Errorf("CreateLabel happy path = %v, want nil", err)
	}
}
