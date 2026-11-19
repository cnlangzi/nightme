package wf

import (
	"os"
	"path/filepath"

	"github.com/cnlangzi/nightme/internal/agent"
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
// The owner directory goes with the file when nothing else is left in
// it: the uninstall never removes a file the install did not write.
func (f *Factory) uninstall(list *checklist, addr Address) Result {
	path := f.pathFor(addr)
	result := Result{Path: path}

	list.set(rowRemove, agent.TaskInProgress, "")
	_ = list.send()
	if err := os.Remove(path); err != nil {
		if os.IsNotExist(err) {
			list.set(rowRemove, agent.TaskSkipped, ReasonNotInstalled+" · "+displayPath(path))
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

	detail := "removed · " + displayPath(path)
	dir := filepath.Dir(path)
	if entries, err := os.ReadDir(dir); err == nil && len(entries) == 0 {
		if err := os.Remove(dir); err == nil {
			detail += " · " + displayPath(dir) + " removed too"
		}
	}
	list.set(rowRemove, agent.TaskCompleted, detail)
	_ = list.send()
	result.Reason = ReasonUninstalled
	return result
}
