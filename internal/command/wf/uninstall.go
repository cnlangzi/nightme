package wf

import (
	"os"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/pathutil"
)

// The reasons an uninstall can come to. They are the reason codes of
// the command's outcome: a program reads them without parsing
// anything.
const (
	// ReasonUninstalled is a workflow that was removed.
	ReasonUninstalled = "uninstalled"
	// ReasonNotInstalled is a workflow that was not there to remove.
	ReasonNotInstalled = "not_installed"
	// ReasonRemoveFailed is a file that could not be removed.
	ReasonRemoveFailed = "remove_failed"
)

// rowRemove is the row of the removal checklist.
//
// The removal checklist has one row of its own: the row indices of
// the install's checklist are a separate index space.
const rowRemove = 0

// newRemovalChecklist builds the checklist of one uninstall.
//
// The uninstall is one step, so the checklist holds one row. Its
// subject carries the address: a channel that renders every snapshot
// as a new message would otherwise show a row that does not say which
// workflow it belongs to.
func newRemovalChecklist(subject string, emit func(items []agent.AgentTaskItem) error) *checklist {
	return &checklist{
		emit: emit,
		items: []agent.AgentTaskItem{
			{ID: "remove", Subject: subject, Status: agent.TaskPending},
		},
	}
}

// uninstall removes the workflow at addr.
//
// The bundle goes with it: every file under the bundle directory
// belongs to the workflow. The owner directory goes with the bundle
// when nothing else is left in it: the uninstall removes the one
// bundle it was asked for and never a sibling beside it.
func (f *Factory) uninstall(list *checklist, addr Address) Result {
	path := f.pathFor(addr)
	bundle := f.bundleFor(addr)
	owner := pathutil.Dir(bundle)
	result := Result{Path: path}

	list.set(rowRemove, agent.TaskInProgress, "")
	_ = list.send()
	// The bundle is asked for before it is removed: the removal
	// succeeds for a path that was never there, and a workflow that
	// was never installed is not a removal.
	if _, err := os.Stat(bundle); err != nil {
		if os.IsNotExist(err) {
			list.set(rowRemove, agent.TaskSkipped, ReasonNotInstalled+" · "+displayPath(bundle))
			_ = list.send()
			result.Reason = ReasonNotInstalled
			return result
		}
		list.set(rowRemove, agent.TaskFailed, err.Error())
		_ = list.send()
		result.Reason = ReasonRemoveFailed
		result.Detail = err.Error()
		return result
	}
	if err := os.RemoveAll(bundle); err != nil {
		list.set(rowRemove, agent.TaskFailed, err.Error())
		_ = list.send()
		result.Reason = ReasonRemoveFailed
		result.Detail = err.Error()
		return result
	}

	detail := "removed · " + displayPath(bundle)
	if entries, err := os.ReadDir(owner); err == nil && len(entries) == 0 {
		if err := os.Remove(owner); err == nil {
			detail += " · " + displayPath(owner) + " removed too"
		}
	}
	list.set(rowRemove, agent.TaskCompleted, detail)
	_ = list.send()
	result.Reason = ReasonUninstalled
	return result
}
