package gtw

import (
	"strings"
	"testing"
)

// TestBuildIssueDispatchText_BareIssue covers the canonical
// shape: title / body / metadata fields all present and
// plain-text (no special characters).
func TestBuildIssueDispatchText_BareIssue(t *testing.T) {
	issue := &Issue{
		ID:     42,
		Title:  "Login state expiration",
		Body:   "When the user is logged in for 7 days, the session should expire.",
		State:  "open",
		Labels: []string{"nightme/wip", "priority/high"},
		URL:    "https://github.com/cnlangzi/nightme/issues/42",
	}
	out := buildIssueDispatchText(issue, "login-state-expiration", "cnlangzi/nightme")

	// Header
	if !strings.Contains(out, "📥 GitHub issue #42 — Login state expiration") {
		t.Errorf("missing header line; got:\n%s", out)
	}
	// Metadata
	for _, want := range []string{
		"- repo: cnlangzi/nightme",
		"- branch: login-state-expiration",
		"- url: https://github.com/cnlangzi/nightme/issues/42",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing metadata line %q; got:\n%s", want, out)
		}
	}
	// Description (raw body preserved verbatim)
	if !strings.Contains(out, "When the user is logged in for 7 days, the session should expire.") {
		t.Errorf("missing body content; got:\n%s", out)
	}
	// Closing task prompt
	if !strings.Contains(out, "## Task") {
		t.Errorf("missing Task section; got:\n%s", out)
	}
	if !strings.Contains(out, "Please investigate the issue above") {
		t.Errorf("missing closing instruction; got:\n%s", out)
	}
}

// TestBuildIssueDispatchText_EmptyBody covers the
// trim-and-skip behaviour: a whitespace-only body should NOT
// produce an empty "## Description" section header.
func TestBuildIssueDispatchText_EmptyBody(t *testing.T) {
	issue := &Issue{ID: 7, Title: "x", Body: "   \n\t  ", URL: "u"}
	out := buildIssueDispatchText(issue, "x", "o/r")
	if strings.Contains(out, "## Description") {
		t.Errorf("empty body should suppress ## Description, got:\n%s", out)
	}
	// Header and Task should still be present.
	if !strings.Contains(out, "## Task") {
		t.Errorf("missing Task section")
	}
}

// TestBuildIssueDispatchText_BodyWithBackticks covers
// embedding raw markdown — the template does NOT escape
// anything (the agent prompt is consumed as markdown).
func TestBuildIssueDispatchText_BodyWithBackticks(t *testing.T) {
	issue := &Issue{
		ID:    1,
		Title: "Code injection",
		Body:  "Use `rm -rf $HOME` and ```bash\necho pwned\n``` blocks.",
		URL:   "u",
	}
	out := buildIssueDispatchText(issue, "x", "o/r")
	if !strings.Contains(out, "Use `rm -rf $HOME` and ```bash\necho pwned\n```") {
		t.Errorf("body should be embedded verbatim (no escape); got:\n%s", out)
	}
}

// TestBuildIssueDispatchText_BodyWithCJK pins the CJK
// preservation. Dropping CJK happens in branch-slug
// derivation, but the dispatch prompt keeps everything
// verbatim so the agent sees the original title.
func TestBuildIssueDispatchText_BodyWithCJK(t *testing.T) {
	issue := &Issue{
		ID:    99,
		Title: "登录状态过期",
		Body:  "用户登录 7 天后会话应该过期，请修复。",
		URL:   "https://example.com/issues/99",
	}
	out := buildIssueDispatchText(issue, "login-expire", "o/r")
	if !strings.Contains(out, "登录状态过期") {
		t.Errorf("CJK title should be preserved in header; got:\n%s", out)
	}
	if !strings.Contains(out, "用户登录 7 天后会话应该过期，请修复。") {
		t.Errorf("CJK body should be preserved verbatim; got:\n%s", out)
	}
}

// TestBuildIssueDispatchText_NoURL pins the empty-URL branch.
// The metadata line should still be emitted (with empty value)
// so the agent template has a stable shape across all issues.
func TestBuildIssueDispatchText_NoURL(t *testing.T) {
	issue := &Issue{ID: 5, Title: "t", Body: "b", URL: ""}
	out := buildIssueDispatchText(issue, "br", "o/r")
	if !strings.Contains(out, "- url: ") {
		t.Errorf("url line should be present even when empty; got:\n%s", out)
	}
}

// TestBuildIssueDispatchText_SectionOrder pins the section
// order — header / metadata / description / task. Agent
// prompts rely on this stable shape (the README says so).
func TestBuildIssueDispatchText_SectionOrder(t *testing.T) {
	issue := &Issue{ID: 1, Title: "t", Body: "b", URL: "u"}
	out := buildIssueDispatchText(issue, "br", "o/r")

	headerAt := strings.Index(out, "📥")
	metaAt := strings.Index(out, "## Metadata")
	bodyAt := strings.Index(out, "## Description")
	taskAt := strings.Index(out, "## Task")

	if headerAt < 0 || metaAt < 0 || bodyAt < 0 || taskAt < 0 {
		t.Fatalf("missing one of the sections; got:\n%s", out)
	}
	if !(headerAt < metaAt && metaAt < bodyAt && bodyAt < taskAt) {
		t.Errorf("sections out of order: header=%d meta=%d body=%d task=%d",
			headerAt, metaAt, bodyAt, taskAt)
	}
}