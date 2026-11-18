package gtw

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
)

// --- LabelCache ---

// TestLabelCache_NilIsInert pins the nil-receiver contract: a
// HandlerDeps that wires no cache behaves like the pre-cache code —
// every lookup misses, every store is dropped, and nothing panics.
func TestLabelCache_NilIsInert(t *testing.T) {
	var c *LabelCache
	if c.lookup(ProviderGitHub, "cnlangzi", "nightme") {
		t.Errorf("a nil cache must never report a hit")
	}
	c.store(ProviderGitHub, "cnlangzi", "nightme")
	if c.lookup(ProviderGitHub, "cnlangzi", "nightme") {
		t.Errorf("a nil cache must not remember anything")
	}
}

// TestLabelCache_StoreThenLookup pins the key: the entry is the
// repository on a platform, so the same repository hits and every
// other one misses.
func TestLabelCache_StoreThenLookup(t *testing.T) {
	c := &LabelCache{}
	if c.lookup(ProviderGitHub, "cnlangzi", "nightme") {
		t.Errorf("an empty cache must miss")
	}
	c.store(ProviderGitHub, "cnlangzi", "nightme")
	if !c.lookup(ProviderGitHub, "cnlangzi", "nightme") {
		t.Errorf("a stored repository must hit")
	}
	if c.lookup(ProviderGitHub, "cnlangzi", "other") {
		t.Errorf("another repository must miss")
	}
	if c.lookup(ProviderGitHub, "other", "nightme") {
		t.Errorf("another owner must miss")
	}
}

// TestLabelCache_PlatformIsPartOfTheKey pins that two platforms
// hosting repositories with the same owner and name keep separate
// entries: each platform has its own label catalog, and a shared
// entry would leave the second platform's labels uncreated.
func TestLabelCache_PlatformIsPartOfTheKey(t *testing.T) {
	c := &LabelCache{}
	c.store(ProviderGitHub, "acme", "api")
	if c.lookup(ProviderGitLab, "acme", "api") {
		t.Errorf("a GitLab project must not hit the GitHub repository's entry")
	}
	if !c.lookup(ProviderGitHub, "acme", "api") {
		t.Errorf("the GitHub repository must still hit its own entry")
	}
}

// --- ensureRepoLabels ---

// TestEnsureRepoLabels_CreatesOnlyTheMissingLabels pins that the
// check creates what the catalog lacks — not every label in
// AllLabels. A repository whose catalog was bootstrapped before
// `nightme/assigned` existed gets exactly one creation call.
func TestEnsureRepoLabels_CreatesOnlyTheMissingLabels(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListLabels([]string{
		LabelWIP, LabelReady, LabelReviewing, LabelRevise, LabelDone, LabelStuck,
	})
	deps := HandlerDeps{LabelCache: &LabelCache{}}

	created, err := ensureRepoLabels(context.Background(), deps, prov, "cnlangzi", "nightme")
	if err != nil {
		t.Fatalf("ensureRepoLabels: %v", err)
	}
	if len(created.Created) != 1 || created.Created[0] != LabelAssigned {
		t.Errorf("created = %v, want [%s]", created.Created, LabelAssigned)
	}
	if !created.Verified {
		t.Errorf("the check read the catalog; Verified = false")
	}
	calls := prov.CallsByMethod("CreateLabel")
	if len(calls) != 1 {
		t.Fatalf("CreateLabel calls = %d, want 1 (only the missing label): %+v", len(calls), calls)
	}
	if calls[0].Label != LabelAssigned {
		t.Errorf("CreateLabel label = %q, want %q", calls[0].Label, LabelAssigned)
	}
	meta := LabelMetaFor(LabelAssigned)
	if calls[0].Color != meta.Color || calls[0].Description != meta.Description {
		t.Errorf("CreateLabel metadata = %q / %q, want %q / %q",
			calls[0].Color, calls[0].Description, meta.Color, meta.Description)
	}
}

// TestEnsureRepoLabels_CompleteCatalogCreatesNothing pins that a
// repository whose catalog already holds every label is left alone.
func TestEnsureRepoLabels_CompleteCatalogCreatesNothing(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListLabels(AllLabels)
	deps := HandlerDeps{LabelCache: &LabelCache{}}

	chk, err := ensureRepoLabels(context.Background(), deps, prov, "cnlangzi", "nightme")
	if err != nil {
		t.Fatalf("ensureRepoLabels: %v", err)
	}
	if len(chk.Created) != 0 {
		t.Errorf("created = %v, want none", chk.Created)
	}
	if n := len(prov.CallsByMethod("CreateLabel")); n != 0 {
		t.Errorf("CreateLabel calls = %d, want 0 (the catalog is complete)", n)
	}
}

// TestEnsureRepoLabels_CacheHitMakesNoRemoteCall pins that a
// verified repository costs nothing on the next run: neither a
// catalog read nor a label creation.
func TestEnsureRepoLabels_CacheHitMakesNoRemoteCall(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListLabels(AllLabels)
	deps := HandlerDeps{LabelCache: &LabelCache{}}
	ctx := context.Background()

	if _, err := ensureRepoLabels(ctx, deps, prov, "cnlangzi", "nightme"); err != nil {
		t.Fatalf("first ensureRepoLabels: %v", err)
	}
	chk, err := ensureRepoLabels(ctx, deps, prov, "cnlangzi", "nightme")
	if err != nil {
		t.Fatalf("second ensureRepoLabels: %v", err)
	}
	if len(chk.Created) != 0 {
		t.Errorf("created = %v, want none on a cache hit", chk.Created)
	}
	if chk.Verified {
		t.Errorf("a cache hit did not read the catalog; Verified = true")
	}
	if n := len(prov.CallsByMethod("ListLabels")); n != 1 {
		t.Errorf("ListLabels calls = %d, want 1 (the second check is a cache hit)", n)
	}
	if n := len(prov.CallsByMethod("CreateLabel")); n != 0 {
		t.Errorf("CreateLabel calls = %d, want 0", n)
	}
}

// TestEnsureRepoLabels_CacheIsKeyedByRepository pins that the
// entry belongs to the repository, not to one worktree of it: a
// second repository is checked on its own.
func TestEnsureRepoLabels_CacheIsKeyedByRepository(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListLabels(AllLabels)
	deps := HandlerDeps{LabelCache: &LabelCache{}}
	ctx := context.Background()

	if _, err := ensureRepoLabels(ctx, deps, prov, "cnlangzi", "nightme"); err != nil {
		t.Fatalf("ensureRepoLabels: %v", err)
	}
	if _, err := ensureRepoLabels(ctx, deps, prov, "cnlangzi", "other"); err != nil {
		t.Fatalf("ensureRepoLabels (other repo): %v", err)
	}
	if n := len(prov.CallsByMethod("ListLabels")); n != 2 {
		t.Errorf("ListLabels calls = %d, want 2 (another repository is checked on its own)", n)
	}
}

// TestEnsureRepoLabels_FailedCheckIsNotRecorded pins that only a
// successful check is remembered: a failed read is retried by the
// next command.
func TestEnsureRepoLabels_FailedCheckIsNotRecorded(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListLabelsErr(errors.New("403 Forbidden: missing Labels read scope"))
	deps := HandlerDeps{LabelCache: &LabelCache{}}
	ctx := context.Background()

	if _, err := ensureRepoLabels(ctx, deps, prov, "cnlangzi", "nightme"); err == nil {
		t.Fatalf("ensureRepoLabels = nil error, want the provider's error")
	}
	if _, err := ensureRepoLabels(ctx, deps, prov, "cnlangzi", "nightme"); err == nil {
		t.Fatalf("second ensureRepoLabels = nil error, want the provider's error")
	}
	if n := len(prov.CallsByMethod("ListLabels")); n != 2 {
		t.Errorf("ListLabels calls = %d, want 2 (a failed check is retried)", n)
	}
}

// TestEnsureRepoLabels_FailedCreationIsNotRecorded pins the same
// for a failed creation: the check stops at the first error and
// remembers nothing.
func TestEnsureRepoLabels_FailedCreationIsNotRecorded(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListLabels([]string{})
	prov.SetCreateLabelErr(errors.New("403 Forbidden: missing Labels write scope"))
	deps := HandlerDeps{LabelCache: &LabelCache{}}
	ctx := context.Background()

	if _, err := ensureRepoLabels(ctx, deps, prov, "cnlangzi", "nightme"); err == nil {
		t.Fatalf("ensureRepoLabels = nil error, want the provider's error")
	}
	if _, err := ensureRepoLabels(ctx, deps, prov, "cnlangzi", "nightme"); err == nil {
		t.Fatalf("second ensureRepoLabels = nil error, want the provider's error")
	}
	if n := len(prov.CallsByMethod("ListLabels")); n != 2 {
		t.Errorf("ListLabels calls = %d, want 2 (a failed check is retried)", n)
	}
}

// TestEnsureRepoLabels_SameOwnerRepoOnTwoPlatforms pins that the
// platform is part of the cache key: a GitLab project and a GitHub
// repository can share an owner/repo string, and each platform has
// its own label catalog.
func TestEnsureRepoLabels_SameOwnerRepoOnTwoPlatforms(t *testing.T) {
	github := newFakeGitProvider(ProviderGitHub, "github.com")
	github.SetListLabels(AllLabels)
	gitlab := newFakeGitProvider(ProviderGitLab, "gitlab.com")
	gitlab.SetListLabels(AllLabels)
	deps := HandlerDeps{LabelCache: &LabelCache{}}
	ctx := context.Background()

	if _, err := ensureRepoLabels(ctx, deps, github, "acme", "api"); err != nil {
		t.Fatalf("ensureRepoLabels (github): %v", err)
	}
	if _, err := ensureRepoLabels(ctx, deps, gitlab, "acme", "api"); err != nil {
		t.Fatalf("ensureRepoLabels (gitlab): %v", err)
	}
	if n := len(gitlab.CallsByMethod("ListLabels")); n != 1 {
		t.Errorf("ListLabels calls = %d, want 1 (the GitLab project has its own catalog)", n)
	}
}

// TestEnsureRepoLabels_NilCacheAlwaysReadsRemotely pins that a
// HandlerDeps without a cache keeps working: the check runs every
// time, against the platform.
func TestEnsureRepoLabels_NilCacheAlwaysReadsRemotely(t *testing.T) {
	prov := newFakeGitProvider(ProviderGitHub, "github.com")
	prov.SetListLabels(AllLabels)
	deps := HandlerDeps{}
	ctx := context.Background()

	for range 2 {
		if _, err := ensureRepoLabels(ctx, deps, prov, "cnlangzi", "nightme"); err != nil {
			t.Fatalf("ensureRepoLabels: %v", err)
		}
	}
	if n := len(prov.CallsByMethod("ListLabels")); n != 2 {
		t.Errorf("ListLabels calls = %d, want 2 (a nil cache never hits)", n)
	}
}

// TestLabelCatalogNote_SaysWhatTheQueueNeeds pins the empty-queue
// note: it names the queue marker, says the label was just created
// when the check repaired the repository, and says the catalog was
// verified earlier when the answer came from the cache.
func TestLabelCatalogNote_SaysWhatTheQueueNeeds(t *testing.T) {
	created := labelCatalogNote(labelCheck{Created: []string{LabelAssigned}, Verified: true})
	if !strings.Contains(created, LabelAssigned) {
		t.Errorf("note = %q, want it to name %q", created, LabelAssigned)
	}
	if !strings.Contains(created, "just created") {
		t.Errorf("note = %q, want it to say the label was just created", created)
	}
	complete := labelCatalogNote(labelCheck{Verified: true})
	if !strings.Contains(complete, LabelAssigned) {
		t.Errorf("note = %q, want it to name %q", complete, LabelAssigned)
	}
	if strings.Contains(complete, "just created") {
		t.Errorf("note = %q, want it not to claim the label was created", complete)
	}
	// A cached answer was verified earlier in the session, not
	// now — the note must not claim the label is in the catalog.
	cached := labelCatalogNote(labelCheck{})
	if !strings.Contains(cached, "earlier in this session") {
		t.Errorf("note = %q, want it to say the catalog was verified earlier", cached)
	}
	if strings.Contains(cached, "just created") {
		t.Errorf("note = %q, want it not to claim the label was created", cached)
	}
}

// TestPrependWarning pins the shared warning helper: an empty
// warning leaves the body alone, a non-empty one leads the body, and
// several warnings ride as one block in the order they are given.
func TestPrependWarning(t *testing.T) {
	if got := prependWarning("body"); got != "body" {
		t.Errorf("prependWarning with no warning = %q, want %q", got, "body")
	}
	if got := prependWarning("body", ""); got != "body" {
		t.Errorf("prependWarning with an empty warning = %q, want %q", got, "body")
	}
	if got := prependWarning("body", "⚠️ boom"); got != "⚠️ boom\nbody" {
		t.Errorf("prependWarning = %q, want %q", got, "⚠️ boom\nbody")
	}
	if got := prependWarning("body", "⚠️ one", "", "⚠️ two"); got != "⚠️ one\n⚠️ two\nbody" {
		t.Errorf("prependWarning = %q, want %q", got, "⚠️ one\n⚠️ two\nbody")
	}
}

// --- provider ListLabels ---

// TestGitHubListLabels_ReturnsCatalogNames pins that the provider
// reports the names the platform returns.
func TestGitHubListLabels_ReturnsCatalogNames(t *testing.T) {
	cli := &stubCLIRunner{
		stdout: `[{"id":1,"name":"bug"},{"id":2,"name":"nightme/wip","color":"fbca04"}]`,
	}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	got, err := p.ListLabels(context.Background(), "cnlangzi", "nightme")
	if err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	want := []string{"bug", "nightme/wip"}
	if !slices.Equal(got, want) {
		t.Errorf("ListLabels = %v, want %v", got, want)
	}
}

// TestGitHubListLabels_CLIArgs pins the exact argv: the repository
// label catalog is read through `gh api`, like every other
// platform call in the package.
func TestGitHubListLabels_CLIArgs(t *testing.T) {
	cli := &stubCLIRunner{stdout: "[]"}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	if _, err := p.ListLabels(context.Background(), "cnlangzi", "nightme"); err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	want := []string{"gh", "api", "repos/cnlangzi/nightme/labels?per_page=100"}
	if !slices.Equal(cli.argv, want) {
		t.Errorf("argv = %v, want %v", cli.argv, want)
	}
}

// TestGitHubListLabels_ErrorSurfaces pins that a failed read keeps
// the provider's own stderr — the caller shows it to the user.
func TestGitHubListLabels_ErrorSurfaces(t *testing.T) {
	cli := &stubCLIRunner{
		stderr: "403 Forbidden: Resource not accessible by personal access token\n",
		err:    errors.New("exit status 1"),
	}
	p := &GitHubProvider{Runner: cli, host: "github.com"}

	_, err := p.ListLabels(context.Background(), "cnlangzi", "nightme")
	if err == nil {
		t.Fatalf("ListLabels = nil error, want non-nil")
	}
	if !strings.Contains(err.Error(), "403 Forbidden") {
		t.Errorf("error must echo the provider stderr verbatim; got: %v", err)
	}
}

// TestGitLabListLabels_ReturnsCatalogNames pins the GitLab
// counterpart: the project's label catalog, read through `glab api`.
func TestGitLabListLabels_ReturnsCatalogNames(t *testing.T) {
	cli := &stubCLIRunner{
		stdout: `[{"id":1,"name":"bug"},{"id":2,"name":"nightme/wip","color":"#fbca04"}]`,
	}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	got, err := p.ListLabels(context.Background(), "acme", "platform")
	if err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	want := []string{"bug", "nightme/wip"}
	if !slices.Equal(got, want) {
		t.Errorf("ListLabels = %v, want %v", got, want)
	}
}

// TestGitLabListLabels_CLIArgs pins the exact argv, including the
// URL-encoded project path.
func TestGitLabListLabels_CLIArgs(t *testing.T) {
	cli := &stubCLIRunner{stdout: "[]"}
	p := &GitLabProvider{Runner: cli, host: "gitlab.com"}

	if _, err := p.ListLabels(context.Background(), "acme", "platform"); err != nil {
		t.Fatalf("ListLabels: %v", err)
	}
	want := []string{"glab", "api", "projects/acme%2Fplatform/labels?per_page=100"}
	if !slices.Equal(cli.argv, want) {
		t.Errorf("argv = %v, want %v", cli.argv, want)
	}
}
