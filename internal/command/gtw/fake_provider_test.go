package gtw

import (
	"context"
	"fmt"
	"sync"
)

// fakeGitProvider is a test-only GitProvider that returns
// pre-set issues / errors and records every call. Used by
// fix_remote_integration_test.go to drive /gtw fix ID mode
// end-to-end without touching github.com / gitlab.com.
//
// Tests construct one via newFakeGitProvider(kind, host),
// seed it via SetIssue / SetGetIssueErr / SetAddIssueLabelErr /
// SetRemoveIssueLabelErr, then wire the package-level fakeDetect
// into deps.Detect so runFixRemote's `detect(ctx, remoteURL,
// prober)` returns it.
//
// Recording: every method appends a fakeProviderCall to .calls
// under the provider's own mutex so concurrent tests (or a
// test that intentionally fires two provider actions in
// parallel) don't race the slice.
type fakeGitProvider struct {
	kind    ProviderKind
	host    string
	version string

	mu sync.Mutex

	// Per-issue responses. issueByID[id] is returned when
	// GetIssue is called; issueErr[id] is returned as error if
	// set (overrides issueByID). "default" sentinel is used when
	// id is not in the map.
	issueByID map[int]*Issue
	issueErr  map[int]error

	// Generic response for AddIssueLabel / RemoveIssueLabel. Returning
	// nil = success.
	addIssueLabelErr    error
	removeIssueLabelErr error

	// Generic response for CreateLabel. Returning nil = success.
	// Used by tests that simulate a label-create failure
	// (network / 403 / etc.) on the bootstrap path.
	createLabelErr error

	// Per-name response for CreateLabel. When a label name is
	// present in this map, createLabelErrFor[name] is returned
	// INSTEAD of the generic createLabelErr. Lets tests pin a
	// failure to a specific bootstrap step (e.g. "fail on the
	// second label, succeed on the first") to verify the
	// loop short-circuits mid-iteration.
	createLabelErrFor map[string]error

	// CreatePR response. createPRResp is the URL the fake
	// returns when createPRErr is nil. createPRErr wins when
	// non-nil (lets tests simulate failure modes).
	createPRResp string
	createPRErr  error

	// FindOpenPRForBranch response. findOpenPRResp is the PR
	// the fake returns when findOpenPRErr is nil. findOpenPRErr
	// wins when non-nil. Mirrors the prByHead / prErr split
	// (FindOpenPRForBranch and GetPR share the same shape, but
	// different error semantics — FindOpenPRForBranch is the
	// /gtw pr precheck where every error is surfaced).
	findOpenPRResp *PR
	findOpenPRErr  error

	// GetPR response. prByHead[head] is returned when GetPR is
	// called with the matching head branch; prErr[head] wins
	// when set (overrides prByHead). Missing key returns
	// (nil, nil) so the footer render path treats it as "no
	// PR yet" — same fail-soft contract as the real providers.
	prByHead map[string]*PR
	prErr    map[string]error

	// Recorded calls (chronological).
	calls []fakeProviderCall
}

// fakeProviderCall is a single recorded invocation. Fields are
// named after the interface method (one variant per interface
// method) so tests can pattern-match cleanly.
type fakeProviderCall struct {
	Method string // "GetIssue" / "AddIssueLabel" / "RemoveIssueLabel" / "CreatePR" / "GetPR" / "CreateLabel"
	Owner  string
	Repo   string
	ID     int
	Label  string // only set for AddIssueLabel / RemoveIssueLabel / CreateLabel
	Head   string // only set for CreatePR / GetPR

	// CreateLabel-only fields. Empty for the other methods.
	// Stored so tests can assert colour / description were
	// threaded through LabelMetaFor correctly.
	Color       string
	Description string

	// CreatePR-only fields. Empty for the other methods.
	Base    string
	PRTitle string
	PRBody  string
	PRURL   string // returned URL when CreatePRResp is set
}

// newFakeGitProvider returns a minimal provider. Tests then
// seed issue maps / errors before driving /gtw fix.
func newFakeGitProvider(kind ProviderKind, host string) *fakeGitProvider {
	return &fakeGitProvider{
		kind:               kind,
		host:               host,
		version:            "v0.0.0-test",
		issueByID:          make(map[int]*Issue),
		issueErr:           make(map[int]error),
		prByHead:           make(map[string]*PR),
		prErr:              make(map[string]error),
		createLabelErrFor:  make(map[string]error),
	}
}

// SetIssue registers `issue` as the response for GetIssue(id).
func (f *fakeGitProvider) SetIssue(id int, issue *Issue) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issueByID[id] = issue
	delete(f.issueErr, id)
}

// SetIssueAttachments patches the issue for `id` to include the
// given attachments. Convenience helper for tests that don't
// want to construct a full Issue literal just to add files.
func (f *fakeGitProvider) SetIssueAttachments(id int, atts []IssueAttachment) {
	f.mu.Lock()
	defer f.mu.Unlock()
	iss, ok := f.issueByID[id]
	if !ok {
		iss = &Issue{ID: id}
		f.issueByID[id] = iss
	}
	cp := *iss
	cp.Attachments = atts
	f.issueByID[id] = &cp
}

// SetGetIssueErr registers `err` as the response for GetIssue(id).
// If `err` wraps ErrIssueNotFound (via fmt.Errorf("%w: ...", ...))
// the runFixRemote handler will surface "issue not found" to the
// user — that's the canonical way to simulate a 404.
func (f *fakeGitProvider) SetGetIssueErr(id int, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.issueErr[id] = err
	delete(f.issueByID, id)
}

// SetAddIssueLabelErr configures the response for all AddIssueLabel
// calls. Used to simulate a label-API failure (network / 403 / etc.).
func (f *fakeGitProvider) SetAddIssueLabelErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addIssueLabelErr = err
}

// SetRemoveIssueLabelErr configures the response for all RemoveIssueLabel
// calls.
func (f *fakeGitProvider) SetRemoveIssueLabelErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeIssueLabelErr = err
}

// SetCreateLabelErr configures the response for all CreateLabel
// calls. Used by the bootstrap-failure tests
// (TestFixRemote_CreateLabelFailure_RollsBack et al.) to
// simulate a label-create API failure.
func (f *fakeGitProvider) SetCreateLabelErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createLabelErr = err
}

// SetCreateLabelErrFor configures CreateLabel(name) to return
// `err`. Overrides the generic SetCreateLabelErr for the named
// label only. Use this to simulate a mid-loop bootstrap
// failure: previous labels in AllLabels succeed, the named
// one fails, and subsequent ones never run (loop short-
// circuits). Pass err=nil to remove the override.
func (f *fakeGitProvider) SetCreateLabelErrFor(name string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.createLabelErrFor == nil {
		f.createLabelErrFor = map[string]error{}
	}
	if err == nil {
		delete(f.createLabelErrFor, name)
		return
	}
	f.createLabelErrFor[name] = err
}

// SetCreatePRResp configures the response for all CreatePR
// calls. The returned URL is what dispatchPR will echo in its
// ✅ card. Tests wrap the existing PR URL in ErrPRExists to
// simulate the "already exists" path; see SetCreatePRErr.
func (f *fakeGitProvider) SetCreatePRResp(url string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createPRResp = url
	f.createPRErr = nil
}

// SetCreatePRErr configures the error returned by CreatePR.
// Wrap ErrPRExists to simulate "PR already exists" so the
// dispatcher's branch can use errors.Is cleanly.
func (f *fakeGitProvider) SetCreatePRErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.createPRErr = err
}

// SetFindOpenPRResp registers the response for FindOpenPRForBranch.
// Pass nil to simulate "no PR yet" (gate 2 pass); pass a
// non-nil PR to simulate "already open" (gate 2 fail).
func (f *fakeGitProvider) SetFindOpenPRResp(pr *PR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findOpenPRResp = pr
}

// SetFindOpenPRErr registers an error response for
// FindOpenPRForBranch — e.g. ErrCLINotInstalled or a wrapped
// unknown stderr. Wraps errors.Is-friendly sentinels so the
// dispatcher's switch can use errors.Is cleanly.
func (f *fakeGitProvider) SetFindOpenPRErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.findOpenPRErr = err
}

// SetPR registers `pr` as the response for GetPR(head). Tests
// use this to simulate a repository where the current head
// branch has an associated open PR / MR.
func (f *fakeGitProvider) SetPR(head string, pr *PR) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prByHead[head] = pr
	delete(f.prErr, head)
}

// SetGetPRErr configures GetPR(head) to return err. Use this to
// simulate a provider-side failure (network down, auth expired,
// rate limit). No sentinel — the footer path treats any error
// as "no PR, omit Line 4".
func (f *fakeGitProvider) SetGetPRErr(head string, err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.prErr[head] = err
	delete(f.prByHead, head)
}

// Calls returns a snapshot of recorded calls (so the caller
// doesn't race the provider's mutex).
func (f *fakeGitProvider) Calls() []fakeProviderCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]fakeProviderCall, len(f.calls))
	copy(out, f.calls)
	return out
}

// CallsByMethod filters Calls() by method name. Sugar for tests
// that only care about e.g. AddIssueLabel calls.
func (f *fakeGitProvider) CallsByMethod(method string) []fakeProviderCall {
	out := []fakeProviderCall{}
	for _, c := range f.Calls() {
		if c.Method == method {
			out = append(out, c)
		}
	}
	return out
}

// --- GitProvider interface ---

func (f *fakeGitProvider) Kind() ProviderKind    { return f.kind }
func (f *fakeGitProvider) Host() string         { return f.host }
func (f *fakeGitProvider) Version() string      { return f.version }

func (f *fakeGitProvider) GetIssue(_ context.Context, owner, repo string, id int) (*Issue, error) {
	f.mu.Lock()
	// Record first, then read response. Recording inside the
	// lock guarantees call order matches the order of method
	// invocations even when the test later reads Calls() under
	// its own lock.
	f.calls = append(f.calls, fakeProviderCall{
		Method: "GetIssue", Owner: owner, Repo: repo, ID: id,
	})
	if err, ok := f.issueErr[id]; ok {
		f.mu.Unlock()
		return nil, err
	}
	iss, ok := f.issueByID[id]
	f.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("%w: fake has no issue %d", ErrIssueNotFound, id)
	}
	// Defensive copy so the test's stored Issue can't be
	// mutated by downstream code.
	cp := *iss
	return &cp, nil
}

func (f *fakeGitProvider) AddIssueLabel(_ context.Context, owner, repo string, id int, label string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeProviderCall{
		Method: "AddIssueLabel", Owner: owner, Repo: repo, ID: id, Label: label,
	})
	err := f.addIssueLabelErr
	f.mu.Unlock()
	return err
}

func (f *fakeGitProvider) RemoveIssueLabel(_ context.Context, owner, repo string, id int, label string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeProviderCall{
		Method: "RemoveIssueLabel", Owner: owner, Repo: repo, ID: id, Label: label,
	})
	err := f.removeIssueLabelErr
	f.mu.Unlock()
	return err
}

// CreateLabel records the call and returns createLabelErr (nil
// by default). Per-name overrides via SetCreateLabelErrFor win
// over the generic createLabelErr — tests that simulate a
// mid-loop failure use the per-name API to pin the failing
// label; tests that simulate a blanket bootstrap failure use
// SetCreateLabelErr.
//
// Mirrors the AddIssueLabel / RemoveIssueLabel pattern so tests
// can assert chronological ordering (e.g. all 6 CreateLabels
// BEFORE AddIssueLabel) via CallsByMethod.
func (f *fakeGitProvider) CreateLabel(_ context.Context, owner, repo, name, color, description string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeProviderCall{
		Method:      "CreateLabel",
		Owner:       owner,
		Repo:        repo,
		Label:       name,
		Color:       color,
		Description: description,
	})
	err := f.createLabelErr
	if specific, ok := f.createLabelErrFor[name]; ok {
		err = specific
	}
	f.mu.Unlock()
	return err
}

func (f *fakeGitProvider) CreatePR(_ context.Context, owner, repo, base, head, title, body string) (string, error) {
	f.mu.Lock()
	url := f.createPRResp
	err := f.createPRErr
	f.calls = append(f.calls, fakeProviderCall{
		Method:  "CreatePR",
		Owner:   owner,
		Repo:    repo,
		Base:    base,
		Head:    head,
		PRTitle: title,
		PRBody:  body,
		PRURL:   url,
	})
	f.mu.Unlock()
	return url, err
}

// FindOpenPRForBranch is the fake implementation of
// GitProvider.FindOpenPRForBranch. Like the real providers, it
// returns (nil, nil) when no PR is set — that means "no PR
// yet", which is gate 2's pass condition.
//
// Unlike GetPR, FindOpenPRForBranch errors propagate verbatim:
// dispatchPR surfaces them with a `❌ check existing PR: <err>`
// prefix, so the test must wrap any non-nil error in a way that
// errors.Is can resolve (or that stringifies to a useful
// diagnostic). See SetFindOpenPRErr.
func (f *fakeGitProvider) FindOpenPRForBranch(_ context.Context, owner, repo, head string) (*PR, error) {
	f.mu.Lock()
	pr := f.findOpenPRResp
	err := f.findOpenPRErr
	f.calls = append(f.calls, fakeProviderCall{
		Method: "FindOpenPRForBranch",
		Owner:  owner,
		Repo:   repo,
		Head:   head,
	})
	f.mu.Unlock()
	if err != nil {
		return nil, err
	}
	if pr == nil {
		return nil, nil
	}
	cp := *pr
	return &cp, nil
}

func (f *fakeGitProvider) GetPR(_ context.Context, owner, repo, head string) (*PR, error) {
	f.mu.Lock()
	// Record first, then read response. Recording inside the
	// lock guarantees call order matches invocation order
	// even when a test later reads Calls() under its own lock.
	f.calls = append(f.calls, fakeProviderCall{
		Method: "GetPR", Owner: owner, Repo: repo, Head: head,
	})
	if err, ok := f.prErr[head]; ok {
		f.mu.Unlock()
		return nil, err
	}
	pr, ok := f.prByHead[head]
	f.mu.Unlock()
	if !ok {
		// "no PR for this head" — same fail-soft contract as
		// the real providers. Caller (footer render path) treats
		// (nil, nil) as "omit Line 4".
		return nil, nil
	}
	cp := *pr
	return &cp, nil
}

// --- dependency-injection shim ---

// fakeDetect is the deps.Detect replacement that always returns
// the given provider. Tests install this via:
//
//	deps.Detect = func(_ context.Context, _ string, _ HTTPProber, _ string) (GitProvider, error) {
//	    return prov, nil
//	}
//
// The remoteURL / prober / worktree arguments are ignored — the
// fake has no notion of "URL hint" or "API probe" semantics. We
// rely on the test seeding the issue map directly via SetIssue /
// SetGetIssueErr.
//
// Kept as a constructor rather than a free function so tests
// don't accidentally share a provider across cases — each test
// gets its own.
func fakeDetect(prov GitProvider) func(context.Context, string, HTTPProber, string) (GitProvider, error) {
	return func(_ context.Context, _ string, _ HTTPProber, _ string) (GitProvider, error) {
		return prov, nil
	}
}

// Compile-time assertion: fakeGitProvider must implement
// GitProvider. If a future interface change breaks this, the
// test binary won't compile — exactly what we want.
var _ GitProvider = (*fakeGitProvider)(nil)