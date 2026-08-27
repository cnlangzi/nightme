package gtw

import (
	"strings"
	"testing"
)

// TestBuildIssueDispatchText_BareIssue covers the canonical
// shape: title / body / metadata fields all present and
// plain-text (no special characters). F-XX: takes an explicit
// IssueDispatchMode (Plan by default).
func TestBuildIssueDispatchText_BareIssue(t *testing.T) {
	issue := &Issue{
		ID:     42,
		Title:  "Login state expiration",
		Body:   "When the user is logged in for 7 days, the session should expire.",
		State:  "open",
		Labels: []string{"nightme/wip", "priority/high"},
		URL:    "https://github.com/cnlangzi/nightme/issues/42",
	}
	out := buildIssueDispatchText(issue, "login-state-expiration", "cnlangzi/nightme", DispatchPlan, 0, 0)

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
	// Plan-mode due-diligence framing: Plan is a question +
	// decision pass, NOT an implementation pass. These pins
	// lock the methodology in:
	if !strings.Contains(out, "due-diligence pass") {
		t.Errorf("Plan prompt must frame as due-diligence (not implementation); got:\n%s", out)
	}
	if !strings.Contains(out, "Do NOT modify, create, or delete any files.") {
		t.Errorf("missing Plan-mode read-only instruction; got:\n%s", out)
	}
	if !strings.Contains(out, "Baseline") {
		t.Errorf("Plan prompt must anchor analysis to the code baseline; got:\n%s", out)
	}
	// Step 1 + Step 2 wording — the two-step decompose-then-verify
	// discipline is the core.
	if !strings.Contains(out, "Decompose the request") {
		t.Errorf("Plan prompt must require request decomposition (Step 1); got:\n%s", out)
	}
	if !strings.Contains(out, "Verify every claim against the code") {
		t.Errorf("Plan prompt must require code verification (Step 2); got:\n%s", out)
	}
	// Step 6 — the questions-for-the-user section is now an
	// explicit deliverable (not buried in "Risks").
	if !strings.Contains(out, "Questions for the user") {
		t.Errorf("Plan prompt must require 'Questions for the user' section (Step 6 deliverable); got:\n%s", out)
	}
	if !strings.Contains(out, "Present the plan and STOP") {
		t.Errorf("missing Plan-mode STOP signal; got:\n%s", out)
	}
}

// TestBuildIssueDispatchText_BareIssue_ExecuteMode mirrors
// TestBuildIssueDispatchText_BareIssue for the F-XX Execute
// prompt variant: same canonical shape (header / metadata /
// description / task), but the §Task block uses the
// user-authorised "implement the fix" wording instead of
// the read-only Plan prompt.
func TestBuildIssueDispatchText_BareIssue_ExecuteMode(t *testing.T) {
	issue := &Issue{
		ID:     42,
		Title:  "Login state expiration",
		Body:   "When the user is logged in for 7 days, the session should expire.",
		State:  "open",
		Labels: []string{"nightme/wip", "priority/high"},
		URL:    "https://github.com/cnlangzi/nightme/issues/42",
	}
	out := buildIssueDispatchText(issue, "login-state-expiration", "cnlangzi/nightme", DispatchExecute, 0, 0)

	// Header / metadata / description / task — same as Plan mode
	for _, want := range []string{
		"📥 GitHub issue #42 — Login state expiration",
		"- repo: cnlangzi/nightme",
		"- branch: login-state-expiration",
		"- url: https://github.com/cnlangzi/nightme/issues/42",
		"When the user is logged in for 7 days, the session should expire.",
		"## Task",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Execute prompt missing shared-shape %q; got:\n%s", want, out)
		}
	}
	// Execute-mode-specific wording (GOBL mode replaces the old
	// "Implement the change" instruction). The lead-in must NOT
	// assume a prior Plan turn exists in chat — `-y` can dispatch
	// Execute directly with no Plan round.
	if !strings.Contains(out, "GOBL mode") {
		t.Errorf("Execute prompt missing 'GOBL mode' marker; got:\n%s", out)
	}
	if !strings.Contains(out, "a plan may or may not have been produced") {
		t.Errorf("Execute prompt must not assume a prior Plan turn; got:\n%s", out)
	}
	// Plan-mode-specific wording must NOT leak
	if strings.Contains(out, "Do NOT modify") || strings.Contains(out, "Present the plan and STOP") {
		t.Errorf("Execute prompt must NOT contain Plan-mode wording; got:\n%s", out)
	}
}

// TestBuildIssueDispatchText_Plan_StopsBeforeEdits pins the
// F-XX Plan-mode prompt: due-diligence pass (not
// implementation) grounded in the worktree's source via a
// two-step discipline (decompose claims, then verify each
// against the code), explicit "Questions for the user"
// deliverable (Step 6), explicit "STOP" signal, no
// "Implement" leakage.
func TestBuildIssueDispatchText_Plan_StopsBeforeEdits(t *testing.T) {
	issue := &Issue{ID: 42, Title: "Login state", Body: "b", URL: "u"}
	out := buildIssueDispatchText(issue, "br", "o/r", DispatchPlan, 0, 0)
	for _, want := range []string{
		"due-diligence pass",                         // framing
		"Baseline",                                   // methodology anchor
		"Decompose the request",                      // Step 1 discipline
		"Verify every claim against the code",        // Step 2 discipline
		"Questions for the user",                     // Step 6 deliverable
		"Do NOT modify, create, or delete any files.", // read-only invariant
		"Present the plan and STOP",                  // wait-for-user gate
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Plan prompt missing %q; got:\n%s", want, out)
		}
	}
	if strings.Contains(out, "Implement the change committed") {
		t.Errorf("Plan prompt must not contain 'Implement' (that's Execute)")
	}
}

// TestBuildIssueDispatchText_Execute_AuthorisesEdits pins
// the F-XX Execute-mode prompt: GOBL mode (Goal/Obstacles/
// Boundaries/Learn) — agent is autonomous on the path
// (which files to open, which tests to run, sequencing)
// but every decision must be code-grounded, every test
// must pass before completion, and any deviation from the
// plan must be announced in chat BEFORE acting.
func TestBuildIssueDispatchText_Execute_AuthorisesEdits(t *testing.T) {
	issue := &Issue{ID: 42, Title: "Login state", Body: "b", URL: "u"}
	out := buildIssueDispatchText(issue, "br", "o/r", DispatchExecute, 0, 0)
	for _, want := range []string{
		"GOBL",                                          // methodology pin
		"Do not invent functionality",                    // boundary
		"Do not skip, suppress, or mark-expected",         // boundary
		"do NOT silently suppress",                        // boundary (test failure)
		"Do not report 'complete'",                        // boundary
		"declare the revision in chat FIRST",             // deviation discipline
		"Pre-existing failures",                          // diagnose-vs-introduced
		"file:line",                                      // grounding
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Execute prompt missing %q; got:\n%s", want, out)
		}
	}
	for _, forbid := range []string{
		"Do NOT modify, create, or delete", // Plan-only invariant
		"Present the plan and STOP",        // Plan-only gate
	} {
		if strings.Contains(out, forbid) {
			t.Errorf("Execute prompt must not contain %q (that's Plan)", forbid)
		}
	}
}

// TestBuildIssueDispatchText_RuntimeSelfContained pins the
// runtime self-containment invariant: the dispatch prompt
// runs in a standalone agent on the user's own worktree and
// cannot see this repo's docs (F-gtw-fix.md,
// REVIEWER_INSTRUCTIONS.md) or the other dispatch mode. So
// neither mode's runtime text may leak internal section
// numbers, doc filenames, or cross-mode references like
// "the Execute pass" / "the plan above". Each prompt must be
// fully self-contained.
func TestBuildIssueDispatchText_RuntimeSelfContained(t *testing.T) {
	issue := &Issue{ID: 42, Title: "Login state", Body: "b", URL: "u"}
	for _, mode := range []IssueDispatchMode{DispatchPlan, DispatchExecute} {
		out := buildIssueDispatchText(issue, "br", "o/r", mode, 0, 0)
		for _, leak := range []string{
			"§4",                 // internal section number
			"F-gtw-fix",          // internal design doc filename
			"REVIEWER_INSTRUCTIONS", // internal methodology doc
			"Execute (§",         // Plan cross-referencing Execute
			"the plan above",     // Execute assuming a prior Plan turn
		} {
			if strings.Contains(out, leak) {
				t.Errorf("%v prompt must not leak internal reference %q (runtime agent can't see it):\n%s", mode, leak, out)
			}
		}
	}
}

// TestBuildIssueDispatchText_EmptyBody covers the
// trim-and-skip behaviour: a whitespace-only body should NOT
// produce an empty "## Description" section header.
func TestBuildIssueDispatchText_EmptyBody(t *testing.T) {
	issue := &Issue{ID: 7, Title: "x", Body: "   \n\t  ", URL: "u"}
	out := buildIssueDispatchText(issue, "x", "o/r", DispatchPlan, 0, 0)
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
	out := buildIssueDispatchText(issue, "x", "o/r", DispatchPlan, 0, 0)
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
	out := buildIssueDispatchText(issue, "login-expire", "o/r", DispatchPlan, 0, 0)
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
	out := buildIssueDispatchText(issue, "br", "o/r", DispatchPlan, 0, 0)
	if !strings.Contains(out, "- url: ") {
		t.Errorf("url line should be present even when empty; got:\n%s", out)
	}
}

// TestBuildIssueDispatchText_SectionOrder pins the section
// order — header / metadata / description / task. Agent
// prompts rely on this stable shape (the README says so).
func TestBuildIssueDispatchText_SectionOrder(t *testing.T) {
	issue := &Issue{ID: 1, Title: "t", Body: "b", URL: "u"}
	out := buildIssueDispatchText(issue, "br", "o/r", DispatchPlan, 0, 0)

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

// TestBuildIssueDispatchText_AttachmentsSection pins the
// Attachments section: it must report actual downloaded counts
// (not len(issue.Attachments)), speak the agent's language
// ("images shown inline" / "files downloaded, read on demand")
// — never our internal block-type names ("ContentImage" /
// "ContentFile"), which the runtime agent has no concept of.
// The bridges translate the blocks themselves; the prompt text
// only primes the agent that attachments exist.
func TestBuildIssueDispatchText_AttachmentsSection(t *testing.T) {
	issue := &Issue{ID: 1, Title: "t", Body: "b", URL: "u"}

	tests := []struct {
		name              string
		images, files     int
		wantContains      []string
		wantNotContains   []string
		wantSectionAbsent bool
	}{
		{
			name: "images only", images: 2, files: 0,
			wantContains:    []string{"## Attachments", "2 image(s): shown inline in this message (you can see them directly)"},
			wantNotContains: []string{"file(s):", "ContentImage", "ContentFile"},
		},
		{
			name: "files only", images: 0, files: 3,
			wantContains:    []string{"## Attachments", "3 file(s): downloaded to the worktree; read on demand with your file tools"},
			wantNotContains: []string{"image(s):", "ContentImage", "ContentFile"},
		},
		{
			name: "both", images: 1, files: 2,
			wantContains: []string{
				"## Attachments",
				"1 image(s): shown inline in this message (you can see them directly)",
				"2 file(s): downloaded to the worktree; read on demand with your file tools",
			},
			wantNotContains: []string{"ContentImage", "ContentFile"},
		},
		{
			name: "none suppresses section", images: 0, files: 0,
			wantSectionAbsent: true,
			wantNotContains:   []string{"## Attachments", "ContentImage", "ContentFile"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			out := buildIssueDispatchText(issue, "br", "o/r", DispatchPlan, tc.images, tc.files)
			if tc.wantSectionAbsent {
				if strings.Contains(out, "## Attachments") {
					t.Errorf("zero attachments should suppress ## Attachments; got:\n%s", out)
				}
				return
			}
			for _, want := range tc.wantContains {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q; got:\n%s", want, out)
				}
			}
			for _, forbid := range tc.wantNotContains {
				if strings.Contains(out, forbid) {
					t.Errorf("must not contain internal term %q (runtime agent doesn't know it); got:\n%s", forbid, out)
				}
			}
			// Section order: Attachments sits between Description and Task.
			attachAt := strings.Index(out, "## Attachments")
			bodyAt := strings.Index(out, "## Description")
			taskAt := strings.Index(out, "## Task")
			if !(bodyAt < attachAt && attachAt < taskAt) {
				t.Errorf("Attachments section out of order: desc=%d attach=%d task=%d", bodyAt, attachAt, taskAt)
			}
		})
	}
}
