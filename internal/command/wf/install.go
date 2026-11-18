package wf

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/pkg/wfe"
)

// The reasons an install can come to. They are the reason codes of
// the command's outcome: a program reads them without parsing
// anything.
const (
	// ReasonInstalled is a workflow that was not there before.
	ReasonInstalled = "installed"
	// ReasonUpdated is a workflow that was there and changed.
	ReasonUpdated = "updated"
	// ReasonUnchanged is a workflow that was there and did not change.
	ReasonUnchanged = "unchanged"
	// ReasonNotFound is a workflow no tier has.
	ReasonNotFound = "not_found"
	// ReasonInvalidAddress is an address that does not fit the hub's
	// shape.
	ReasonInvalidAddress = "invalid_address"
	// ReasonUnreachable is a workflow no tier could serve.
	ReasonUnreachable = "unreachable"
	// ReasonInvalidContent is a file that does not validate as a
	// workflow.
	ReasonInvalidContent = "invalid_content"
	// ReasonWriteFailed is a file that could not be written.
	ReasonWriteFailed = "write_failed"
)

// progressInterval is the shortest time between two progress
// snapshots.
//
// The updates are throttled because a channel that cannot patch a
// message in place renders every snapshot as a message of its own.
const progressInterval = 500 * time.Millisecond

// barCells is the width of the progress bar.
const barCells = 20

// The rows of the install's checklist.
const (
	rowDownload = 0
	rowValidate = 1
	rowInstall  = 2
)

// Result is what one install did.
//
// The reason is the verdict; the rest are the facts behind it.
type Result struct {
	// Reason is the outcome's reason code.
	Reason string
	// Path is the file the workflow is installed as.
	Path string
	// Source is the tier that served the file.
	Source string
	// SHA256 is the digest of the installed content.
	SHA256 string
	// Detail says why the install did not finish.
	Detail string
}

// checklist is the three-row task list the install renders.
//
// Every message carries the whole snapshot, so a channel that cannot
// patch a message in place still shows the current state.
type checklist struct {
	emit  func(items []agent.AgentTaskItem) error
	items []agent.AgentTaskItem
}

// newChecklist builds the checklist of one install.
//
// The subject of the first row carries the address: a channel that
// renders every snapshot as a new message would otherwise show rows
// that do not say which install they belong to.
func newChecklist(subject string, emit func(items []agent.AgentTaskItem) error) *checklist {
	return &checklist{
		emit: emit,
		items: []agent.AgentTaskItem{
			{ID: "download", Subject: subject, Status: agent.TaskPending},
			{ID: "validate", Subject: "validate", Status: agent.TaskPending},
			{ID: "install", Subject: "install", Status: agent.TaskPending},
		},
	}
}

// set records the state of one row.
func (c *checklist) set(row int, status agent.AgentTaskStatus, detail string) {
	c.items[row].Status = status
	c.items[row].ActiveForm = detail
}

// send publishes the checklist.
func (c *checklist) send() error {
	if c.emit == nil {
		return nil
	}
	items := make([]agent.AgentTaskItem, len(c.items))
	copy(items, c.items)
	return c.emit(items)
}

// install fetches the workflow at addr and installs it.
//
// The three steps of the install are reported in the checklist: the
// download, the validation and the write. The result carries the
// reason the install came to.
func (f *Factory) install(ctx context.Context, list *checklist, addr Address) Result {
	path := f.pathFor(addr)
	result := Result{Path: path}

	// ── download ──────────────────────────────────────────────
	content, tier, reason, detail := f.download(ctx, list, addr)
	if reason != "" {
		list.set(rowDownload, agent.TaskFailed, detail)
		list.set(rowValidate, agent.TaskSkipped, "")
		list.set(rowInstall, agent.TaskSkipped, "")
		_ = list.send()
		result.Reason = reason
		result.Detail = detail
		return result
	}
	result.Source = tier
	list.set(rowDownload, agent.TaskCompleted, tier+" · "+sizeLabel(len(content)))

	// ── validate ──────────────────────────────────────────────
	list.set(rowValidate, agent.TaskInProgress, "")
	_ = list.send()
	if _, err := wfe.Parse(content); err != nil {
		problem := firstDiagnostic(err)
		list.set(rowValidate, agent.TaskFailed, problem)
		list.set(rowInstall, agent.TaskSkipped, "")
		_ = list.send()
		result.Reason = ReasonInvalidContent
		result.Detail = problem
		return result
	}
	sum := sha256hex(content)
	result.SHA256 = sum
	list.set(rowValidate, agent.TaskCompleted, "ok · sha256 "+sum[:8])

	// ── install ───────────────────────────────────────────────
	list.set(rowInstall, agent.TaskInProgress, "")
	_ = list.send()
	existing, readErr := os.ReadFile(path)
	switch {
	case readErr == nil && sha256hex(existing) == sum:
		list.set(rowInstall, agent.TaskCompleted, ReasonUnchanged+" · "+displayPath(path))
		_ = list.send()
		result.Reason = ReasonUnchanged
		return result
	case readErr == nil:
		reason = ReasonUpdated
	default:
		reason = ReasonInstalled
	}
	if err := writeFile(path, content); err != nil {
		list.set(rowInstall, agent.TaskFailed, err.Error())
		_ = list.send()
		result.Reason = ReasonWriteFailed
		result.Detail = err.Error()
		return result
	}
	list.set(rowInstall, agent.TaskCompleted, reason+" · "+displayPath(path))
	_ = list.send()
	result.Reason = reason
	return result
}

// download fetches the workflow file, tier by tier.
//
// Every tier is probed before its file is fetched: a tier that cannot
// be reached is skipped, and the next one is tried. The reason is
// `not_found` when every tier says the workflow is not there, and
// `unreachable` when a tier could not be asked.
func (f *Factory) download(ctx context.Context, list *checklist, addr Address) ([]byte, string, string, string) {
	var reasons []string
	missing := 0
	for i, src := range f.sources {
		list.set(rowDownload, agent.TaskInProgress, "probing "+src.Name)
		_ = list.send()
		if err := probe(ctx, f.client, src, addr); err != nil {
			reasons = append(reasons, err.Error())
			if isMissing(err) {
				missing++
			}
			if i+1 < len(f.sources) {
				list.set(rowDownload, agent.TaskInProgress,
					skipDetail(src, err, f.sources[i+1].Name))
				_ = list.send()
			}
			continue
		}
		content, err := f.fetch(ctx, src, addr, list)
		if err != nil {
			reasons = append(reasons, err.Error())
			if isMissing(err) {
				missing++
			}
			continue
		}
		return content, src.Name, "", ""
	}
	detail := "no tier could serve it"
	if len(reasons) > 0 {
		detail = strings.Join(reasons, "; ")
	}
	reason := ReasonUnreachable
	if missing == len(f.sources) && missing > 0 {
		reason = ReasonNotFound
	}
	return nil, "", reason, detail
}

// fetch downloads the file from one tier and reports the transfer.
func (f *Factory) fetch(ctx context.Context, src Source, addr Address, list *checklist) ([]byte, error) {
	throttle := &throttle{interval: progressInterval}
	return fetch(ctx, f.client, src, addr, func(transferred, total int64) {
		if total <= 0 || !throttle.allow(time.Now()) {
			return
		}
		list.set(rowDownload, agent.TaskInProgress, "downloading "+progressBar(transferred, total))
		_ = list.send()
	})
}

// pathFor returns the file the workflow of addr is installed as.
//
// The file sits under the address, so the workflow's name keeps the
// address it was published with.
func (f *Factory) pathFor(addr Address) string {
	return filepath.Join(f.dir, addr.Owner, addr.Name+".yml")
}

// isMissing reports whether the tier said the workflow is not there.
func isMissing(err error) bool {
	var status *tierStatus
	return errors.As(err, &status) && status.status == http.StatusNotFound
}

// skipDetail says why a tier was skipped and which tier comes next.
func skipDetail(src Source, err error, next string) string {
	if isMissing(err) {
		return src.Name + " has no such workflow — trying " + next
	}
	return src.Name + " unreachable — trying " + next
}

// firstDiagnostic returns the first problem of a workflow that does
// not validate.
func firstDiagnostic(err error) string {
	var verr *wfe.ValidationError
	if errors.As(err, &verr) && len(verr.Diagnostics) > 0 {
		return verr.Diagnostics[0].Message
	}
	return err.Error()
}

// throttle gates the progress updates.
type throttle struct {
	interval time.Duration
	last     time.Time
}

// allow reports whether an update may be sent now.
func (t *throttle) allow(now time.Time) bool {
	if t.last.IsZero() {
		t.last = now
		return true
	}
	if now.Sub(t.last) < t.interval {
		return false
	}
	t.last = now
	return true
}

// progressBar renders the percentage bar of a transfer.
//
// The bar is the row's detail: the status marker is a fixed-width
// state glyph, and a channel that parses a leading "[ ]" as a
// checkbox renders that row by other rules than the rest of the list.
func progressBar(transferred, total int64) string {
	if total <= 0 {
		return ""
	}
	pct := transferred * 100 / total
	if pct > 100 {
		pct = 100
	}
	filled := int(pct * barCells / 100)
	return "[" + strings.Repeat("#", filled) +
		strings.Repeat("-", barCells-filled) + "] " + strconv.FormatInt(pct, 10) + "%"
}

// sizeLabel renders a size in bytes the way a reader reads it.
func sizeLabel(size int) string {
	switch {
	case size < 1024:
		return strconv.Itoa(size) + " B"
	case size < 1024*1024:
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	default:
		return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
	}
}

// displayPath renders a path the way the operator writes it: the home
// directory is a "~".
func displayPath(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}
	if path == home {
		return "~"
	}
	rel, err := filepath.Rel(home, path)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return path
	}
	return filepath.Join("~", rel)
}
