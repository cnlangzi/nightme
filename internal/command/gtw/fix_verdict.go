package gtw

import (
	"context"
	"fmt"
	"maps"
	"sync"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// The verdict `/gtw fix` reports after the coding agent's turn.
//
// The command dispatches the issue body to the chat under the same
// message id it was called with, so the message's outcome is held
// until the agent's turn ends. To report the verdict at that point,
// the command takes a share of the message's work (cs.Hold) and
// subscribes to the prompt-end bus (cs.PromptEndBus). When the turn
// ends it reads the worktree, reports the verdict, and releases its
// share — the settled signal goes out after that, so the settled
// outcome is the post-work one.

// fixVerdict judges what the coding agent produced in the worktree
// and reports it when the agent's turn ends.
//
// The share of the message's work is what keeps the message alive:
// the settled signal goes out when the last participant releases
// its share, and this one releases only after the verdict is on the
// work.
type fixVerdict struct {
	ctx      context.Context
	deps     HandlerDeps
	work     *chatsession.Work
	worktree string
	branch   string
	base     string
	data     map[string]string
	once     sync.Once
}

// watchFixVerdict takes a share of the message's work and waits for
// the turn that carries the fix to end.
//
// The share is taken BEFORE the dispatch: the dispatch queues the
// issue under the command's own message id, and the queue takes its
// own share inside that call — a share taken later could arrive
// after the message already settled.
//
// The subscription happens before the dispatch too: the bus calls
// its subscribers in registration order, so this one reads the
// worktree and reports before the queue's subscriber releases the
// queue's share.
//
// The context keeps the command's values (the message's work lives
// on them) but drops its cancellation: the verdict is judged long
// after the command returned, and the git calls it makes must not
// fail because the command's context is done.
func watchFixVerdict(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	messageID, worktree, branch, base string,
	data map[string]string,
) *fixVerdict {
	v := &fixVerdict{
		ctx:      context.WithoutCancel(ctx),
		deps:     deps,
		worktree: worktree,
		branch:   branch,
		base:     base,
		data:     data,
	}
	v.work = cs.Hold(messageID)
	bus := cs.PromptEndBus
	if bus == nil {
		// No bus means no prompt-end will ever be published: the
		// turn this verdict waits for cannot end. Give the share
		// back and let the message settle.
		v.work.Release()
		return v
	}
	bus.Subscribe(func(e chatsession.PromptEndedEvent) bool {
		if e.UserMsgID != messageID {
			return false
		}
		v.once.Do(func() { v.report(e.Reason) })
		return false
	})
	return v
}

// release gives the share of the message's work back without
// reporting a verdict — the path where no turn will run, so there
// is nothing to judge.
func (v *fixVerdict) release() {
	v.once.Do(func() { v.work.Release() })
}

// report judges the work and puts the verdict on the message's
// work.
//
// A turn that did not end cleanly keeps the prompt-end's own verdict:
// a cancelled turn is a cancellation, not a verdict about the work.
func (v *fixVerdict) report(reason agent.PromptEndReason) {
	defer v.work.Release()
	if reason != agent.PromptEndClean {
		return
	}
	reportOutcome(v.ctx, judgeFixWork(v.ctx, v.deps, v.worktree, v.branch, v.base, v.data))
}

// judgeFixWork reads the worktree and decides what the coding agent
// produced.
//
// The agent's work is what the tree holds — staged, unstaged and
// untracked — plus the commits it made since the branch was cut. No
// changes at all is an exception: the command was asked to fix an
// issue and nothing was fixed.
//
// A worktree that cannot be read is an exception too: the agent's
// turn ended, but nothing is known about what it left behind.
func judgeFixWork(
	ctx context.Context,
	deps HandlerDeps,
	worktree, branch, base string,
	data map[string]string,
) messages.Outcome {
	staged, unstaged, untracked, err := countWorktreeChanges(ctx, worktree, deps)
	if err != nil {
		return fixWorkException(data, "error",
			"the worktree could not be read after the agent's turn",
			err.Error())
	}
	commits, err := agentCommits(ctx, worktree, base, deps)
	if err != nil {
		return fixWorkException(data, "error",
			"the worktree could not be read after the agent's turn",
			err.Error())
	}
	w := workCounts{
		staged:    staged,
		unstaged:  unstaged,
		untracked: untracked,
		commits:   len(commits),
	}
	facts := maps.Clone(data)
	if facts == nil {
		facts = map[string]string{}
	}
	facts["changes"] = fmt.Sprintf("%d", w.total())
	facts["staged"] = fmt.Sprintf("%d", w.staged)
	facts["unstaged"] = fmt.Sprintf("%d", w.unstaged)
	facts["untracked"] = fmt.Sprintf("%d", w.untracked)
	facts["commits"] = fmt.Sprintf("%d", w.commits)
	if w.total() == 0 {
		return command.FailedOutcomeData("no_changes",
			"the agent left the worktree unchanged", facts)
	}
	return messages.Outcome{
		Status:  messages.OutcomeOK,
		Summary: fmt.Sprintf("the agent produced %d change(s) on %s", w.total(), branch),
		Data:    facts,
	}
}

// fixWorkException builds the outcome of a fix whose work could not
// be judged — the facts the fix produced ride along, the error
// description says what broke.
func fixWorkException(data map[string]string, reason, summary, desc string) messages.Outcome {
	facts := maps.Clone(data)
	if facts == nil {
		facts = map[string]string{}
	}
	return command.FailedOutcomeData(reason, summary, facts, desc)
}
