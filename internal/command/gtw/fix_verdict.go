package gtw

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/pkg/outcome"
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

// verdictGitTimeout bounds the git calls a verdict makes. The
// judgment runs on its own goroutine, but a hung git must not keep
// the message from settling forever.
const verdictGitTimeout = 30 * time.Second

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
	unsub    func()
	worktree string
	branch   string
	repoRoot string
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
// its subscribers in registration order, so this one is in place
// before the queue's subscriber releases the queue's share.
//
// The context keeps the command's values (the message's work lives
// on them) but drops its cancellation: the verdict is judged long
// after the command returned, and the git calls it makes must not
// fail because the command's context is done.
func watchFixVerdict(
	ctx context.Context,
	cs *chatsession.ChatSession,
	deps HandlerDeps,
	messageID, worktree, branch, repoRoot, base string,
	data map[string]string,
) *fixVerdict {
	v := &fixVerdict{
		ctx:      context.WithoutCancel(ctx),
		deps:     deps,
		worktree: worktree,
		branch:   branch,
		repoRoot: repoRoot,
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
	v.unsub = bus.Subscribe(func(e chatsession.PromptEndedEvent) bool {
		if e.UserMsgID != messageID {
			return false
		}
		v.once.Do(func() { v.report(e.Reason, e.Err) })
		return false
	})
	return v
}

// release gives the share of the message's work back without
// reporting a verdict — the path where no turn will run, so there
// is nothing to judge.
func (v *fixVerdict) release() {
	v.once.Do(func() {
		v.unsubscribe()
		v.work.Release()
	})
}

// unsubscribe drops the bus subscription. The handler holds the
// verdict — and through it the worktree and the message's work — so
// it must not outlive the verdict.
func (v *fixVerdict) unsubscribe() {
	if v.unsub != nil {
		v.unsub()
	}
}

// report judges the work and puts the verdict on the message's
// work.
//
// The judgment runs on its own goroutine: the bus publishes the
// prompt end on the chat's event pump, and the git calls the
// judgment makes must not hold that pump up.
//
// A turn that did not end cleanly keeps the prompt-end's own verdict:
// a cancelled turn is a cancellation, not a verdict about the work.
// The facts the fix produced still have to reach the program, so
// they ride along without a verdict of their own.
//
// An upstream failure says more than the worktree does: the turn died
// before the agent could work, so the status the upstream reported is
// the verdict — a worktree that carries no change is what a dead turn
// looks like, not what a broken one looks like.
func (v *fixVerdict) report(reason agent.PromptEndReason, err error) {
	go func() {
		defer v.unsubscribe()
		defer v.work.Release()
		var apiErr *agent.APIError
		if errors.As(err, &apiErr) {
			v.work.Report(outcome.Outcome{
				Success:    false,
				Status:     apiErr.Status,
				StatusText: apiErr.Message,
				Data:       maps.Clone(v.data),
			})
			return
		}
		if reason != agent.PromptEndClean {
			v.work.Report(outcome.Outcome{
				Success: false,
				Status:  0,
				Data:    maps.Clone(v.data),
			})
			return
		}
		reportOutcome(v.ctx, judgeFixWork(v.ctx, v.deps, v.worktree, v.branch, v.repoRoot, v.base, v.data))
	}()
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
	worktree, branch, repoRoot, base string,
	data map[string]string,
) outcome.Outcome {
	ctx, cancel := context.WithTimeout(ctx, verdictGitTimeout)
	defer cancel()

	counts, err := countWorktreeChanges(ctx, worktree, deps)
	if err != nil {
		return fixWorkException(data, "the worktree could not be read after the agent's turn: "+err.Error())
	}
	commits, known, err := countAgentCommits(ctx, deps, worktree, repoRoot, base)
	if err != nil {
		return fixWorkException(data, "the worktree could not be read after the agent's turn: "+err.Error())
	}
	counts.commits = len(commits)

	facts := maps.Clone(data)
	if facts == nil {
		facts = map[string]string{}
	}
	facts[KeyChanges] = fmt.Sprintf("%d", counts.total())
	facts[KeyPaths] = fmt.Sprintf("%d", counts.paths)
	facts[KeyStaged] = fmt.Sprintf("%d", counts.staged)
	facts[KeyUnstaged] = fmt.Sprintf("%d", counts.unstaged)
	facts[KeyUntracked] = fmt.Sprintf("%d", counts.untracked)
	facts[KeyCommits] = fmt.Sprintf("%d", counts.commits)

	if counts.total() == 0 {
		if !known {
			// The worktree is clean and the branch's commits could
			// not be counted: the agent may well have committed
			// everything.
			return fixWorkException(data, "the worktree is clean and the branch's history could not be counted")
		}
		return command.FailedOutcomeData(http.StatusInternalServerError,
			"the agent left the worktree unchanged", facts)
	}
	return outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: fmt.Sprintf("the agent produced %d change(s) on %s", counts.total(), branch),
		Data:       facts,
	}
}

// countAgentCommits counts the commits the coding agent made since
// the branch was cut from its baseline.
//
// The baseline is the branch's initial head; when it is unknown, the
// local default branch stands in — the same fallback /gtw commit
// uses. The second return value says whether the count is known at
// all: a clean worktree whose commits cannot be counted is not the
// same as a clean worktree with no commits.
func countAgentCommits(ctx context.Context, deps HandlerDeps, worktree, repoRoot, base string) ([]string, bool, error) {
	if base == "" {
		fallback, err := commitBaseline(ctx, Context{RepoRoot: repoRoot}, deps)
		if err != nil {
			return nil, false, nil
		}
		base = fallback
	}
	commits, err := agentCommits(ctx, worktree, base, deps)
	if err != nil {
		return nil, false, err
	}
	return commits, true, nil
}

// fixWorkException builds the outcome of a fix whose work could not
// be judged — the facts the fix produced ride along, the one line
// says what broke.
func fixWorkException(data map[string]string, what string) outcome.Outcome {
	facts := maps.Clone(data)
	if facts == nil {
		facts = map[string]string{}
	}
	return command.FailedOutcomeData(http.StatusInternalServerError, what, facts)
}
