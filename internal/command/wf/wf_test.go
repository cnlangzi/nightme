package wf

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/httpclient"
	"github.com/cnlangzi/nightme/internal/yamlfile"
)

// validWorkflow is a workflow the hub could serve.
const validWorkflow = "title: code it\njobs:\n  main:\n    steps: [{message: hi}]\n"

// testFactory builds a Factory over the given tiers.
func testFactory(dir string, sources []Source, client *http.Client) *Factory {
	if client == nil {
		client = httpclient.Default()
	}
	return &Factory{dir: dir, client: client, sources: sources}
}

// servingTier returns a tier that serves body on every request.
func servingTier(name, body string) (Source, *httptest.Server) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead, http.MethodOptions:
			w.WriteHeader(http.StatusOK)
		default:
			_, _ = w.Write([]byte(body))
		}
	}))
	return Source{Name: name, BaseURL: srv.URL + "/"}, srv
}

// collector gathers the checklists the install published.
type collector struct {
	snapshots [][]agent.AgentTaskItem
}

// emit records one snapshot.
func (c *collector) emit(items []agent.AgentTaskItem) error {
	c.snapshots = append(c.snapshots, items)
	return nil
}

// last returns the state of a row of the last snapshot.
func (c *collector) last(row int) (agent.AgentTaskStatus, string) {
	if len(c.snapshots) == 0 {
		return agent.TaskPending, ""
	}
	items := c.snapshots[len(c.snapshots)-1]
	if row >= len(items) {
		return agent.TaskPending, ""
	}
	return items[row].Status, items[row].ActiveForm
}

// sandboxHome points os.UserHomeDir at a scratch directory, so a test
// that renders a home-relative path does not read the developer's own
// home. USERPROFILE is set too — os.UserHomeDir reads it on Windows.
func sandboxHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	return home
}

// TestParseAddress_TheShapeOfAnAddress — an address is <owner>/<name>.
func TestParseAddress_TheShapeOfAnAddress(t *testing.T) {
	addr, err := ParseAddress("nightme/coder")
	if err != nil {
		t.Fatalf("ParseAddress: %v", err)
	}
	if addr.Owner != "nightme" || addr.Name != "coder" {
		t.Errorf("the address is %q/%q", addr.Owner, addr.Name)
	}
	if addr.String() != "nightme/coder" {
		t.Errorf("the address reads %q", addr.String())
	}
}

// TestParseAddress_RefusesTheAddressesThatDoNotFit — an address that
// does not fit the hub's shape never reaches the network.
func TestParseAddress_RefusesTheAddressesThatDoNotFit(t *testing.T) {
	for _, address := range []string{
		"../etc/passwd",
		"Nightme/Coder",
		"nightme/",
		"/coder",
		"nightme/coder/extra",
		"nightme/Coder",
		"nightme/coder_1",
		"night me/coder",
		"john.doe/coder",
		"-nightme/coder",
		"actions/foo",
		"runs/foo",
	} {
		t.Run(address, func(t *testing.T) {
			if _, err := ParseAddress(address); err == nil {
				t.Errorf("the address %q was accepted", address)
			}
		})
	}
}

// TestParseAddress_TheOwnerIsAnyNamespace — the owner is a namespace
// of the hub, not an account name of the host: it may carry an
// underscore and it may be longer than an account name.
func TestParseAddress_TheOwnerIsAnyNamespace(t *testing.T) {
	const long = "a-very-long-namespace-name-over-the-account-limits"
	if len(long) != 50 {
		t.Fatalf("the long owner is %d characters, want 50", len(long))
	}
	for _, owner := range []string{"john_doe", long} {
		addr, err := ParseAddress(owner + "/coder")
		if err != nil {
			t.Errorf("the owner %q was refused: %v", owner, err)
			continue
		}
		if addr.Owner != owner {
			t.Errorf("the owner is %q, want %q", addr.Owner, owner)
		}
	}
}

// TestParseAddress_ANameWithoutAnOwnerIsTheAddressOfTheProject —
// "coder" is the address "nightme/coder".
func TestParseAddress_ANameWithoutAnOwnerIsTheAddressOfTheProject(t *testing.T) {
	addr, err := ParseAddress("coder")
	if err != nil {
		t.Fatalf("ParseAddress: %v", err)
	}
	if addr.Owner != "nightme" || addr.Name != "coder" {
		t.Errorf("the address is %q/%q", addr.Owner, addr.Name)
	}
	if addr.String() != "nightme/coder" {
		t.Errorf("the address reads %q", addr.String())
	}
}

// TestParseAddress_ANameWithoutAnOwnerKeepsTheShapeOfTheHub — the
// owner is filled in first, so the name it carries has to fit the
// hub's shape like any other name.
func TestParseAddress_ANameWithoutAnOwnerKeepsTheShapeOfTheHub(t *testing.T) {
	_, err := ParseAddress("Coder")
	if err == nil {
		t.Fatal("the name that is not kebab-case was accepted")
	}
	if !strings.Contains(err.Error(), "kebab-case") {
		t.Errorf("the error is %q, want it to name the shape of the name", err)
	}
}

// TestInstall_WritesTheWorkflowUnderItsAddress — the installed file
// is the workflow file of the address's bundle, so the workflow keeps
// its name.
func TestInstall_WritesTheWorkflowUnderItsAddress(t *testing.T) {
	dir := t.TempDir()
	tier, srv := servingTier("github", validWorkflow)
	defer srv.Close()

	f := testFactory(dir, []Source{tier}, nil)
	list := newChecklist("download nightme/coder", func([]agent.AgentTaskItem) error { return nil })

	result := f.install(context.Background(), list, Address{Owner: "nightme", Name: "coder"}, false)
	if result.Reason != ReasonInstalled {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonInstalled)
	}
	path := filepath.Join(dir, "nightme", "coder", "workflow.yml")
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the workflow was not written: %v", err)
	}
	if string(content) != validWorkflow {
		t.Errorf("the file holds %q", content)
	}
}

// TestInstall_TheReportedPathIsCrossPlatform — the path the operator
// reads is the same on every platform: the home directory as a "~"
// and forward slashes between the components. The outcome carries the
// absolute path, as the operating system spells it.
func TestInstall_TheReportedPathIsCrossPlatform(t *testing.T) {
	home := sandboxHome(t)
	dir := filepath.Join(home, ".nightme", "workflows")
	tier, srv := servingTier("github", validWorkflow)
	defer srv.Close()

	f := testFactory(dir, []Source{tier}, nil)
	col := &collector{}
	list := newChecklist("download nightme/coder", col.emit)

	result := f.install(context.Background(), list, Address{Owner: "nightme", Name: "coder"}, false)
	if result.Reason != ReasonInstalled {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonInstalled)
	}
	_, detail := col.last(rowInstall)
	want := ReasonInstalled + " · ~/.nightme/workflows/nightme/coder/workflow.yml"
	if detail != want {
		t.Errorf("the install row says %q, want %q", detail, want)
	}
	if strings.Contains(detail, "\\") {
		t.Errorf("the install row holds a backslash: %q", detail)
	}
	if result.Path != filepath.Join(dir, "nightme", "coder", "workflow.yml") {
		t.Errorf("the path is %q, want the absolute path of the file", result.Path)
	}
}

// TestDisplayPath_APathOutsideTheHomeIsRenderedAsItIs — a path that
// is not under the home directory keeps its shape, with forward
// slashes.
func TestDisplayPath_APathOutsideTheHomeIsRenderedAsItIs(t *testing.T) {
	sandboxHome(t)

	got := displayPath(filepath.Join(string(filepath.Separator), "srv", "nightme"))
	if strings.Contains(got, "\\") {
		t.Errorf("the path holds a backslash: %q", got)
	}
	if got != "/srv/nightme" {
		t.Errorf("the path is %q, want /srv/nightme", got)
	}
}

// TestInstall_TheFilesBesideTheWorkflowStay — the bundle belongs to
// the workflow: the install writes the workflow file and leaves the
// rest of the bundle alone.
func TestInstall_TheFilesBesideTheWorkflowStay(t *testing.T) {
	dir := t.TempDir()
	tier, srv := servingTier("github", validWorkflow)
	defer srv.Close()

	bundle := filepath.Join(dir, "nightme", "coder")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	notes := filepath.Join(bundle, "notes.md")
	if err := os.WriteFile(notes, []byte("# notes\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	f := testFactory(dir, []Source{tier}, nil)
	result := f.install(context.Background(), newChecklist("download x", nil),
		Address{Owner: "nightme", Name: "coder"}, false)
	if result.Reason != ReasonInstalled {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonInstalled)
	}
	if _, err := os.Stat(notes); err != nil {
		t.Errorf("the file beside the workflow is gone: %v", err)
	}
	content, err := os.ReadFile(filepath.Join(bundle, "workflow.yml"))
	if err != nil {
		t.Fatalf("the workflow was not written: %v", err)
	}
	if string(content) != validWorkflow {
		t.Errorf("the file holds %q", content)
	}
}

// TestInstall_TheSameContentTwiceChangesNothing — the content decides
// whether a forced install rewrites the file.
func TestInstall_TheSameContentTwiceChangesNothing(t *testing.T) {
	dir := t.TempDir()
	tier, srv := servingTier("github", validWorkflow)
	defer srv.Close()

	f := testFactory(dir, []Source{tier}, nil)
	addr := Address{Owner: "nightme", Name: "coder"}
	path := filepath.Join(dir, "nightme", "coder", "workflow.yml")

	first := f.install(context.Background(), newChecklist("download x", nil), addr, false)
	if first.Reason != ReasonInstalled {
		t.Fatalf("the reason is %q, want %q", first.Reason, ReasonInstalled)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	time.Sleep(10 * time.Millisecond)

	second := f.install(context.Background(), newChecklist("download x", nil), addr, true)
	if second.Reason != ReasonUnchanged {
		t.Fatalf("the reason is %q, want %q", second.Reason, ReasonUnchanged)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("the file was rewritten: %v -> %v", before.ModTime(), after.ModTime())
	}
}

// TestInstall_DifferentContentOverwritesTheFile — a forced install of
// a workflow that changed is written over the old one.
func TestInstall_DifferentContentOverwritesTheFile(t *testing.T) {
	dir := t.TempDir()
	tier, srv := servingTier("github", validWorkflow)
	defer srv.Close()

	f := testFactory(dir, []Source{tier}, nil)
	addr := Address{Owner: "nightme", Name: "coder"}
	path := filepath.Join(dir, "nightme", "coder", "workflow.yml")

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	old := "title: old\njobs:\n  main:\n    steps: [{message: hi}]\n"
	if err := os.WriteFile(path, []byte(old), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result := f.install(context.Background(), newChecklist("download x", nil), addr, true)
	if result.Reason != ReasonUpdated {
		t.Fatalf("the reason is %q, want %q", result.Reason, ReasonUpdated)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != validWorkflow {
		t.Errorf("the file holds %q", content)
	}
}

// TestInstall_ContentThatDoesNotValidateIsNotWritten — a broken file
// from the hub does not replace a working local one, not even in a
// forced install.
func TestInstall_ContentThatDoesNotValidateIsNotWritten(t *testing.T) {
	dir := t.TempDir()
	tier, srv := servingTier("github", "jobs:\n  main:\n    steps: [{message: hi}]\n")
	defer srv.Close()

	f := testFactory(dir, []Source{tier}, nil)
	addr := Address{Owner: "nightme", Name: "coder"}
	path := filepath.Join(dir, "nightme", "coder", "workflow.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	local := "title: local\njobs:\n  main:\n    steps: [{message: hi}]\n"
	if err := os.WriteFile(path, []byte(local), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}

	result := f.install(context.Background(), newChecklist("download x", nil), addr, true)
	if result.Reason != ReasonInvalidContent {
		t.Fatalf("the reason is %q, want %q", result.Reason, ReasonInvalidContent)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != local {
		t.Errorf("the file holds %q, want the local one", content)
	}
}

// TestInstall_AnInstalledWorkflowIsSkippedUnlessForced — the
// workflow that is already there is left alone: the install costs no
// request and writes nothing.
func TestInstall_AnInstalledWorkflowIsSkippedUnlessForced(t *testing.T) {
	dir := t.TempDir()
	var requests atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		switch r.Method {
		case http.MethodHead, http.MethodOptions:
			w.WriteHeader(http.StatusOK)
		default:
			_, _ = w.Write([]byte(validWorkflow))
		}
	}))
	defer srv.Close()

	path := filepath.Join(dir, "nightme", "coder", "workflow.yml")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	local := "title: local\njobs:\n  main:\n    steps: [{message: hi}]\n"
	if err := os.WriteFile(path, []byte(local), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	f := testFactory(dir, []Source{{Name: "github", BaseURL: srv.URL + "/"}}, nil)
	col := &collector{}
	list := newChecklist("download nightme/coder", col.emit)

	result := f.install(context.Background(), list, Address{Owner: "nightme", Name: "coder"}, false)
	if result.Reason != ReasonSkipped {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonSkipped)
	}
	if got := requests.Load(); got != 0 {
		t.Errorf("the hub was asked %d times, want none", got)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != local {
		t.Errorf("the file holds %q, want the local one", content)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Errorf("the file was rewritten: %v -> %v", before.ModTime(), after.ModTime())
	}
	for row, name := range map[int]string{
		rowDownload: "download",
		rowValidate: "validate",
		rowInstall:  "install",
	} {
		status, _ := col.last(row)
		if status != agent.TaskSkipped {
			t.Errorf("the %s row is %v, want it skipped", name, status)
		}
	}
	_, detail := col.last(rowDownload)
	if detail != ReasonSkipped+" · "+displayPath(path) {
		t.Errorf("the download row says %q", detail)
	}
}

// TestInstall_APathThatIsNotAFileIsNotSkipped — a directory where the
// workflow file should be is not an installed workflow: the install
// does not claim it is one.
func TestInstall_APathThatIsNotAFileIsNotSkipped(t *testing.T) {
	dir := t.TempDir()
	tier, srv := servingTier("github", validWorkflow)
	defer srv.Close()

	path := filepath.Join(dir, "nightme", "coder", "workflow.yml")
	if err := os.MkdirAll(path, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	f := testFactory(dir, []Source{tier}, nil)
	result := f.install(context.Background(), newChecklist("download x", nil),
		Address{Owner: "nightme", Name: "coder"}, false)
	if result.Reason != ReasonWriteFailed {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonWriteFailed)
	}
}

// TestChecklist_ASetOnARowThatIsNotThereIsIgnored — the row indices
// of the two checklists are separate, so a row that belongs to the
// other one is ignored instead of crashing the command.
func TestChecklist_ASetOnARowThatIsNotThereIsIgnored(t *testing.T) {
	list := newRemovalChecklist("remove nightme/coder", nil)
	list.set(rowInstall, agent.TaskFailed, "boom")
	if list.items[0].Status != agent.TaskPending {
		t.Errorf("the row is %v, want it untouched", list.items[0].Status)
	}
}

// TestInstall_TheNextTierServesTheFile — a tier that cannot be
// reached is skipped, and the next one is tried.
func TestInstall_TheNextTierServesTheFile(t *testing.T) {
	dir := t.TempDir()
	// The first tier is closed before it is used: its address refuses
	// the connection.
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	deadURL := dead.URL
	dead.Close()
	live, srv := servingTier("jsdelivr", validWorkflow)
	defer srv.Close()

	f := testFactory(dir, []Source{
		{Name: "github", BaseURL: deadURL + "/"},
		live,
	}, nil)
	col := &collector{}
	list := newChecklist("download nightme/coder", col.emit)

	result := f.install(context.Background(), list, Address{Owner: "nightme", Name: "coder"}, false)
	if result.Reason != ReasonInstalled {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonInstalled)
	}
	if result.Source != "jsdelivr" {
		t.Errorf("the tier that served it is %q, want jsdelivr", result.Source)
	}
	_, detail := col.last(rowDownload)
	if !strings.Contains(detail, "jsdelivr") {
		t.Errorf("the download row says %q, want the tier that served it", detail)
	}
}

// TestInstall_ATierThatRefusesHEADIsAskedWithOPTIONS — a tier that
// does not do HEAD is still usable.
func TestInstall_ATierThatRefusesHEADIsAskedWithOPTIONS(t *testing.T) {
	dir := t.TempDir()
	var options int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead:
			w.WriteHeader(http.StatusMethodNotAllowed)
		case http.MethodOptions:
			options++
			w.WriteHeader(http.StatusOK)
		default:
			_, _ = w.Write([]byte(validWorkflow))
		}
	}))
	defer srv.Close()

	f := testFactory(dir, []Source{{Name: "github", BaseURL: srv.URL + "/"}}, nil)
	result := f.install(context.Background(), newChecklist("download x", nil),
		Address{Owner: "nightme", Name: "coder"}, false)
	if result.Reason != ReasonInstalled {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonInstalled)
	}
	if options == 0 {
		t.Error("the tier was not asked with OPTIONS")
	}
}

// TestInstall_ATierThatCannotBeReachedIsSkippedWithoutAGET — a tier
// that does not answer the probe is not asked for the file.
func TestInstall_ATierThatCannotBeReachedIsSkippedWithoutAGET(t *testing.T) {
	dir := t.TempDir()
	gets := 0
	// The tier does not answer the probe in time.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			time.Sleep(300 * time.Millisecond)
			return
		}
		gets++
		_, _ = w.Write([]byte(validWorkflow))
	}))
	defer srv.Close()

	// The client gives up before the tier answers.
	f := testFactory(dir, []Source{{Name: "github", BaseURL: srv.URL + "/"}},
		&http.Client{Timeout: 50 * time.Millisecond})
	result := f.install(context.Background(), newChecklist("download x", nil),
		Address{Owner: "nightme", Name: "coder"}, false)
	if result.Reason != ReasonUnreachable {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonUnreachable)
	}
	if gets != 0 {
		t.Errorf("the tier was asked for the file %d times, want none", gets)
	}
}

// TestInstall_EveryTierAnswers404IsNotFound — a workflow no tier has
// is not found.
func TestInstall_EveryTierAnswers404IsNotFound(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := testFactory(dir, []Source{{Name: "github", BaseURL: srv.URL + "/"}}, nil)
	result := f.install(context.Background(), newChecklist("download x", nil),
		Address{Owner: "nightme", Name: "coder"}, false)
	if result.Reason != ReasonNotFound {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonNotFound)
	}
	if !strings.Contains(result.Detail, "404") {
		t.Errorf("the detail is %q, want it to name what the tier answered", result.Detail)
	}
}

// TestInstall_TheChecklistHasThreeRows — the install is three steps,
// and a row that never ran is skipped.
func TestInstall_TheChecklistHasThreeRows(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	f := testFactory(dir, []Source{{Name: "github", BaseURL: srv.URL + "/"}}, nil)
	col := &collector{}
	list := newChecklist("download nightme/coder", col.emit)

	f.install(context.Background(), list, Address{Owner: "nightme", Name: "coder"}, false)

	if len(col.snapshots) == 0 {
		t.Fatal("the checklist was never published")
	}
	items := col.snapshots[0]
	if len(items) != 3 {
		t.Fatalf("the checklist holds %d rows, want three", len(items))
	}
	if items[0].ID != "download" || items[1].ID != "validate" || items[2].ID != "install" {
		t.Errorf("the rows are %q, %q, %q", items[0].ID, items[1].ID, items[2].ID)
	}
	status, _ := col.last(rowDownload)
	if status != agent.TaskFailed {
		t.Errorf("the download row is %v, want it failed", status)
	}
	status, _ = col.last(rowValidate)
	if status != agent.TaskSkipped {
		t.Errorf("the validate row is %v, want it skipped", status)
	}
	status, _ = col.last(rowInstall)
	if status != agent.TaskSkipped {
		t.Errorf("the install row is %v, want it skipped", status)
	}
}

// TestProgressBar — the bar is twenty cells wide, and the percentage
// follows it.
func TestProgressBar(t *testing.T) {
	got := progressBar(47, 100)
	want := "[#########-----------] 47%"
	if got != want {
		t.Errorf("the bar is %q, want %q", got, want)
	}
	if got := progressBar(0, 100); got != "[--------------------] 0%" {
		t.Errorf("the bar is %q", got)
	}
	if got := progressBar(100, 100); got != "[####################] 100%" {
		t.Errorf("the bar is %q", got)
	}
}

// TestThrottle_AtMostOneUpdateEvery500ms — the progress updates are
// throttled, because a channel that cannot patch a message in place
// renders every snapshot as a message of its own.
func TestThrottle_AtMostOneUpdateEvery500ms(t *testing.T) {
	th := &throttle{interval: 500 * time.Millisecond}
	base := time.Now()
	allowed := 0
	for i := 0; i <= 20; i++ {
		if th.allow(base.Add(time.Duration(i) * 100 * time.Millisecond)) {
			allowed++
		}
	}
	if allowed > 5 {
		t.Errorf("the throttle allowed %d updates over 2s, want at most 5", allowed)
	}
}

// TestInstall_TheProgressUpdatesAreThrottled — a body that streams
// over two seconds produces at most five progress snapshots.
func TestInstall_TheProgressUpdatesAreThrottled(t *testing.T) {
	dir := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodHead, http.MethodOptions:
			w.WriteHeader(http.StatusOK)
			return
		}
		flusher, _ := w.(http.Flusher)
		for i := 0; i < 20; i++ {
			_, _ = w.Write([]byte("x"))
			if flusher != nil {
				flusher.Flush()
			}
			time.Sleep(100 * time.Millisecond)
		}
	}))
	defer srv.Close()

	f := testFactory(dir, []Source{{Name: "github", BaseURL: srv.URL + "/"}}, nil)
	col := &collector{}
	list := newChecklist("download nightme/coder", col.emit)

	f.install(context.Background(), list, Address{Owner: "nightme", Name: "coder"}, false)

	progress := 0
	for _, snapshot := range col.snapshots {
		if len(snapshot) == 0 {
			continue
		}
		if strings.HasPrefix(snapshot[0].ActiveForm, "downloading") {
			progress++
		}
	}
	if progress > 5 {
		t.Errorf("the install published %d progress snapshots over 2s, want at most 5", progress)
	}
}

// TestWriteFile_LeavesNoPartialFile — the write is a temporary file
// and a rename.
func TestWriteFile_LeavesNoPartialFile(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "nightme", "coder")
	path := filepath.Join(dir, "workflow.yml")

	if err := yamlfile.WriteFileAtomic(path, []byte(validWorkflow)); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != validWorkflow {
		t.Errorf("the file holds %q", content)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("readdir: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("the directory holds %d files, want only the workflow", len(entries))
	}
}

// TestWriteFile_ReplacesTheFileThatIsThere — an install overwrites
// the workflow that is already installed.
func TestWriteFile_ReplacesTheFileThatIsThere(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "coder.yml")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := yamlfile.WriteFileAtomic(path, []byte("new")); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(content) != "new" {
		t.Errorf("the file holds %q", content)
	}
}

// TestTheSources_AreTheThreeTiers — the tiers are tried in order.
func TestTheSources_AreTheThreeTiers(t *testing.T) {
	sources := Sources()
	if len(sources) != 3 {
		t.Fatalf("there are %d tiers, want three", len(sources))
	}
	want := []string{"github", "jsdelivr", "jsdmirror"}
	for i, src := range sources {
		if src.Name != want[i] {
			t.Errorf("the tier %d is %q, want %q", i, src.Name, want[i])
		}
	}
	addr := Address{Owner: "nightme", Name: "coder"}
	got := sources[0].URL(addr)
	if !strings.HasSuffix(got, "/nightme-dev/workflows/main/workflows/nightme/coder/workflow.yml") {
		t.Errorf("the URL is %q", got)
	}
}

// TestInstall_TheThirdTierServesTheFile — the tiers are tried in
// order, and the one that serves the file is named.
func TestInstall_TheThirdTierServesTheFile(t *testing.T) {
	dir := t.TempDir()
	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	firstURL := first.URL
	first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	secondURL := second.URL
	second.Close()
	third, srv := servingTier("jsdmirror", validWorkflow)
	defer srv.Close()

	f := testFactory(dir, []Source{
		{Name: "github", BaseURL: firstURL + "/"},
		{Name: "jsdelivr", BaseURL: secondURL + "/"},
		third,
	}, nil)
	col := &collector{}
	list := newChecklist("download nightme/coder", col.emit)

	result := f.install(context.Background(), list, Address{Owner: "nightme", Name: "coder"}, false)
	if result.Reason != ReasonInstalled {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonInstalled)
	}
	if result.Source != "jsdmirror" {
		t.Errorf("the tier that served it is %q, want jsdmirror", result.Source)
	}
	_, detail := col.last(rowDownload)
	if !strings.Contains(detail, "jsdmirror") {
		t.Errorf("the download row says %q, want the tier that served it", detail)
	}
}

// TestUninstall_RemovesTheBundleAndTheEmptyOwnerDirectory — the
// bundle goes with everything under it, and the owner directory goes
// with it when nothing else is left in it.
func TestUninstall_RemovesTheBundleAndTheEmptyOwnerDirectory(t *testing.T) {
	dir := t.TempDir()
	addr := Address{Owner: "nightme", Name: "coder"}
	bundle := filepath.Join(dir, "nightme", "coder")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	path := filepath.Join(bundle, "workflow.yml")
	if err := os.WriteFile(path, []byte(validWorkflow), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The bundle holds more than the workflow file: it all goes.
	notes := filepath.Join(bundle, "notes.md")
	if err := os.WriteFile(notes, []byte("# notes\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	f := testFactory(dir, nil, nil)
	col := &collector{}
	list := newRemovalChecklist("remove nightme/coder", col.emit)

	result := f.uninstall(list, addr)
	if result.Reason != ReasonUninstalled {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonUninstalled)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("the workflow is still there: %v", err)
	}
	if _, err := os.Stat(notes); !os.IsNotExist(err) {
		t.Errorf("the file beside the workflow is still there: %v", err)
	}
	if _, err := os.Stat(bundle); !os.IsNotExist(err) {
		t.Errorf("the bundle is still there: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(bundle)); !os.IsNotExist(err) {
		t.Errorf("the owner directory is still there: %v", err)
	}
	status, detail := col.last(rowRemove)
	if status != agent.TaskCompleted {
		t.Errorf("the remove row is %v, want it completed", status)
	}
	if !strings.Contains(detail, "removed too") {
		t.Errorf("the remove row says %q, want it to name the removed directory", detail)
	}
}

// TestUninstall_KeepsTheOwnerDirectoryThatHoldsAnotherWorkflow — the
// uninstall removes the workflow it was asked for and nothing else.
func TestUninstall_KeepsTheOwnerDirectoryThatHoldsAnotherWorkflow(t *testing.T) {
	dir := t.TempDir()
	addr := Address{Owner: "nightme", Name: "coder"}
	bundle := filepath.Join(dir, "nightme", "coder")
	sibling := filepath.Join(dir, "nightme", "reviewer", "workflow.yml")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(sibling), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "workflow.yml"), []byte(validWorkflow), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if err := os.WriteFile(sibling, []byte(validWorkflow), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	f := testFactory(dir, nil, nil)
	col := &collector{}
	list := newRemovalChecklist("remove nightme/coder", col.emit)

	result := f.uninstall(list, addr)
	if result.Reason != ReasonUninstalled {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonUninstalled)
	}
	if _, err := os.Stat(filepath.Dir(bundle)); err != nil {
		t.Errorf("the owner directory is gone: %v", err)
	}
	if _, err := os.Stat(sibling); err != nil {
		t.Errorf("the sibling workflow is gone: %v", err)
	}
	_, detail := col.last(rowRemove)
	if strings.Contains(detail, "removed too") {
		t.Errorf("the remove row says %q, want it to leave the directory alone", detail)
	}
}

// TestUninstall_AWorkflowThatIsNotInstalledIsSkipped — removing what
// is not there is not a failure.
func TestUninstall_AWorkflowThatIsNotInstalledIsSkipped(t *testing.T) {
	dir := t.TempDir()
	addr := Address{Owner: "nightme", Name: "coder"}
	bundle := filepath.Join(dir, "nightme", "coder")

	f := testFactory(dir, nil, nil)
	col := &collector{}
	list := newRemovalChecklist("remove nightme/coder", col.emit)

	result := f.uninstall(list, addr)
	if result.Reason != ReasonNotInstalled {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonNotInstalled)
	}
	status, detail := col.last(rowRemove)
	if status != agent.TaskSkipped {
		t.Errorf("the remove row is %v, want it skipped", status)
	}
	if detail != ReasonNotInstalled+" · "+displayPath(bundle) {
		t.Errorf("the remove row says %q", detail)
	}
}

// withRemoveAllStub swaps the package-level removeAll for the
// duration of the test and restores it via t.Cleanup.
//
// The stub is what makes the failure path of the removal reachable:
// chmod'ing the owner directory does not stop the removal on a
// machine whose permissions the test user bypasses (root, or a
// filesystem that ignores the mode bits), so the test would report a
// successful removal where it means to fail one.
func withRemoveAllStub(t *testing.T, fn func(string) error) {
	t.Helper()
	orig := removeAll
	removeAll = fn
	t.Cleanup(func() { removeAll = orig })
}

// TestUninstall_ABundleThatCannotBeRemovedFails — a bundle that
// cannot be removed is a failure, not a removal.
func TestUninstall_ABundleThatCannotBeRemovedFails(t *testing.T) {
	dir := t.TempDir()
	addr := Address{Owner: "nightme", Name: "coder"}
	bundle := filepath.Join(dir, "nightme", "coder")
	if err := os.MkdirAll(bundle, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bundle, "workflow.yml"), []byte(validWorkflow), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	withRemoveAllStub(t, func(string) error {
		return errors.New("the bundle is locked")
	})

	f := testFactory(dir, nil, nil)
	col := &collector{}
	list := newRemovalChecklist("remove nightme/coder", col.emit)

	result := f.uninstall(list, addr)
	if result.Reason != ReasonRemoveFailed {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonRemoveFailed)
	}
	if result.Detail == "" {
		t.Error("the detail is empty, want the error")
	}
	status, _ := col.last(rowRemove)
	if status != agent.TaskFailed {
		t.Errorf("the remove row is %v, want it failed", status)
	}
	if _, err := os.Stat(bundle); err != nil {
		t.Errorf("the bundle is gone: %v", err)
	}
}

// withStatStub swaps the package-level statPath for the duration of
// the test and restores it via t.Cleanup.
//
// The stub is what makes the "cannot be reached" branch reachable:
// whether a path whose parent is a file is "not there" or "not a
// directory" depends on the operating system, so the file system
// cannot steer the branch on every platform.
func withStatStub(t *testing.T, fn func(string) (os.FileInfo, error)) {
	t.Helper()
	orig := statPath
	statPath = fn
	t.Cleanup(func() { statPath = orig })
}

// TestUninstall_ABundleThatCannotBeReachedFails — a bundle that
// cannot even be looked at is a failure, not a removal.
func TestUninstall_ABundleThatCannotBeReachedFails(t *testing.T) {
	dir := t.TempDir()
	addr := Address{Owner: "nightme", Name: "coder"}
	// The ask fails with something other than "not there": the bundle
	// is there, and the failure is in reaching it.
	withStatStub(t, func(string) (os.FileInfo, error) {
		return nil, os.ErrPermission
	})

	f := testFactory(dir, nil, nil)
	col := &collector{}
	list := newRemovalChecklist("remove nightme/coder", col.emit)

	result := f.uninstall(list, addr)
	if result.Reason != ReasonRemoveFailed {
		t.Fatalf("the reason is %q (%s), want %q", result.Reason, result.Detail, ReasonRemoveFailed)
	}
	if result.Detail == "" {
		t.Error("the detail is empty, want the error")
	}
	status, _ := col.last(rowRemove)
	if status != agent.TaskFailed {
		t.Errorf("the remove row is %v, want it failed", status)
	}
}
