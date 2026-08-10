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
// seed it via SetIssue / SetGetIssueErr / SetAddLabelErr /
// SetRemoveLabelErr, then wire the package-level fakeDetect
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

	// Generic response for AddLabel / RemoveLabel. Returning
	// nil = success.
	addLabelErr    error
	removeLabelErr error

	// CreatePR response. createPRResp is the URL the fake
	// returns when createPRErr is nil. createPRErr wins when
	// non-nil (lets tests simulate failure modes).
	createPRResp string
	createPRErr  error

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
	Method string // "GetIssue" / "AddLabel" / "RemoveLabel" / "CreatePR" / "GetPR"
	Owner  string
	Repo   string
	ID     int
	Label  string // only set for AddLabel / RemoveLabel
	Head   string // only set for CreatePR / GetPR

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
		kind:      kind,
		host:      host,
		version:   "v0.0.0-test",
		issueByID: make(map[int]*Issue),
		issueErr:  make(map[int]error),
		prByHead:  make(map[string]*PR),
		prErr:     make(map[string]error),
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

// SetAddLabelErr configures the response for all AddLabel calls.
// Used to simulate a label-API failure (network / 403 / etc.).
func (f *fakeGitProvider) SetAddLabelErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.addLabelErr = err
}

// SetRemoveLabelErr configures the response for all RemoveLabel
// calls.
func (f *fakeGitProvider) SetRemoveLabelErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.removeLabelErr = err
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
// that only care about e.g. AddLabel calls.
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

func (f *fakeGitProvider) AddLabel(_ context.Context, owner, repo string, id int, label string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeProviderCall{
		Method: "AddLabel", Owner: owner, Repo: repo, ID: id, Label: label,
	})
	err := f.addLabelErr
	f.mu.Unlock()
	return err
}

func (f *fakeGitProvider) RemoveLabel(_ context.Context, owner, repo string, id int, label string) error {
	f.mu.Lock()
	f.calls = append(f.calls, fakeProviderCall{
		Method: "RemoveLabel", Owner: owner, Repo: repo, ID: id, Label: label,
	})
	err := f.removeLabelErr
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
//	deps.Detect = func(_ context.Context, _ string, _ HTTPProber) (GitProvider, error) {
//	    return prov, nil
//	}
//
// The remoteURL / prober arguments are ignored — the fake has
// no notion of "URL hint" or "API probe" semantics. We rely on
// the test seeding the issue map directly via SetIssue /
// SetGetIssueErr.
//
// Kept as a constructor rather than a free function so tests
// don't accidentally share a provider across cases — each test
// gets its own.
func fakeDetect(prov GitProvider) func(context.Context, string, HTTPProber) (GitProvider, error) {
	return func(_ context.Context, _ string, _ HTTPProber) (GitProvider, error) {
		return prov, nil
	}
}

// Compile-time assertion: fakeGitProvider must implement
// GitProvider. If a future interface change breaks this, the
// test binary won't compile — exactly what we want.
var _ GitProvider = (*fakeGitProvider)(nil)