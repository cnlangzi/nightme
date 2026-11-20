package gtw

// Tests for GitProvider.AddPRLabel: the exact argv each platform
// builds, and the error each one surfaces when the CLI fails.
//
// The pull request is the review unit, so the state label has to land
// on it — but the CLI differs per platform (gh edits a pull request,
// glab updates a merge request), which is why the provider carries a
// pull-request-scoped method instead of reusing AddIssueLabel.

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// TestGitHubAddPRLabel_CLIArgs pins the exact argv that
// GitHubProvider.AddPRLabel builds:
//
//	gh pr edit <number> --add-label <label> --repo <owner>/<repo>
//
// Element-wise equality, not a substring search: a stray flag or a
// swapped pair would still match the substrings the implementation
// happens to use.
func TestGitHubAddPRLabel_CLIArgs(t *testing.T) {
	cli := &stubCLIRunner{}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	if err := p.AddPRLabel(context.Background(),
		"cnlangzi", "nightme", 7, LabelReady); err != nil {
		t.Fatalf("AddPRLabel: %v", err)
	}
	if cli.callNum != 1 {
		t.Fatalf("Run calls = %d, want 1", cli.callNum)
	}
	want := []string{
		"gh", "pr", "edit", "7",
		"--add-label", LabelReady,
		"--repo", "cnlangzi/nightme",
	}
	if !slices.Equal(cli.argv, want) {
		t.Errorf("argv = %v, want %v", cli.argv, want)
	}
}

// TestGitLabAddPRLabel_CLIArgs pins the exact argv that
// GitLabProvider.AddPRLabel builds:
//
//	glab mr update <number> --label <label> --repo <owner>/<repo>
//
// The merge request is addressed through `mr update` — reusing the
// issue-scoped command would put the label on an issue instead.
func TestGitLabAddPRLabel_CLIArgs(t *testing.T) {
	cli := &stubCLIRunner{}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	if err := p.AddPRLabel(context.Background(),
		"acme", "platform", 7, LabelReady); err != nil {
		t.Fatalf("AddPRLabel: %v", err)
	}
	if cli.callNum != 1 {
		t.Fatalf("Run calls = %d, want 1", cli.callNum)
	}
	want := []string{
		"glab", "mr", "update", "7",
		"--label", LabelReady,
		"--repo", "acme/platform",
	}
	if !slices.Equal(cli.argv, want) {
		t.Errorf("argv = %v, want %v", cli.argv, want)
	}
}

// TestGitHubAddPRLabel_ErrorCarriesTheCLIAndStderr pins the error
// contract: the failing command and the CLI's stderr survive into
// the error, so the reply can name the cause.
func TestGitHubAddPRLabel_ErrorCarriesTheCLIAndStderr(t *testing.T) {
	cli := &stubCLIRunner{
		stderr: "403 Forbidden: missing Labels write scope\n",
		err:    errors.New("exit status 1"),
	}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	err := p.AddPRLabel(context.Background(), "cnlangzi", "nightme", 7, LabelReady)
	if err == nil {
		t.Fatalf("AddPRLabel with 403 stderr = nil, want non-nil error")
	}
	if !strings.Contains(err.Error(), "gh pr edit") {
		t.Errorf("AddPRLabel error must identify the failing command; got: %v", err)
	}
	if !strings.Contains(err.Error(), "403 Forbidden") {
		t.Errorf("AddPRLabel error must echo the stderr verbatim; got: %v", err)
	}
}

// TestGitLabAddPRLabel_ErrorCarriesTheCLIAndStderr mirrors the
// GitHub error contract for the merge request command.
func TestGitLabAddPRLabel_ErrorCarriesTheCLIAndStderr(t *testing.T) {
	cli := &stubCLIRunner{
		stderr: "403 Forbidden - your token does not have permission to add labels\n",
		err:    errors.New("exit status 1"),
	}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	err := p.AddPRLabel(context.Background(), "acme", "platform", 7, LabelReady)
	if err == nil {
		t.Fatalf("AddPRLabel with 403 stderr = nil, want non-nil error")
	}
	if !strings.Contains(err.Error(), "glab mr update") {
		t.Errorf("AddPRLabel error must identify the failing command; got: %v", err)
	}
	if !strings.Contains(err.Error(), "403 Forbidden") {
		t.Errorf("AddPRLabel error must echo the stderr verbatim; got: %v", err)
	}
}
