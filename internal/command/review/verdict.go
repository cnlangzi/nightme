package review

// The verdict /review judges itself by.
//
// A review finds problems and hands them to the coding agent; the
// fixes land in the turn the findings were injected into. The review
// therefore knows two facts: how many findings it collected, and
// what the workspace holds once those findings were acted on. The
// head is recorded when the command starts and compared when the
// review ends.
//
// Nothing to fix is a normal result — the review did its job, there
// was nothing to change. A review whose findings were addressed is a
// normal result too: it left something behind, a new commit or a new
// change in the working tree. A review whose findings were not
// addressed is an exception: the review's work did not land.

import (
	"context"
	"strconv"
	"sync"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
)

// selfVerdict judges the review once the turn its findings went
// into has ended.
type selfVerdict struct {
	work      *chatsession.Work
	ctx       context.Context
	workspace string
	before    workspaceState
	runner    string
	findings  int
	once      sync.Once
}

// watchSelfVerdict takes a share of the message's work and waits
// for the turn the findings were injected into to end.
//
// The share keeps the message alive until the verdict is on it: the
// settled signal goes out when the last participant releases its
// share.
//
// The subscription happens before the injection: the bus calls its
// subscribers in registration order, so this one judges before the
// queue's own subscriber releases its share.
func watchSelfVerdict(
	ctx context.Context,
	cs *chatsession.ChatSession,
	replyTo, workspace, runner string,
	before workspaceState,
	findings int,
) *selfVerdict {
	v := &selfVerdict{
		// The context keeps the review's values but drops its
		// cancellation: the verdict is judged long after the review
		// goroutine returned, and the git calls it makes must not
		// fail because the review's context is done.
		ctx:       context.WithoutCancel(ctx),
		workspace: workspace,
		before:    before,
		runner:    runner,
		findings:  findings,
	}
	bus := cs.PromptEndBus
	if bus == nil {
		// No prompt-end will ever be published: the turn the
		// findings went into cannot end. There is nothing to wait
		// for, so no share is taken.
		return v
	}
	v.work = cs.Hold(replyTo)
	bus.Subscribe(func(e chatsession.PromptEndedEvent) bool {
		if e.UserMsgID != replyTo {
			return false
		}
		v.once.Do(func() { v.report() })
		return false
	})
	return v
}

// release gives the share of the message's work back without
// reporting a verdict — the path where no turn will run, so there is
// nothing to judge.
func (v *selfVerdict) release() {
	v.once.Do(func() { v.work.Release() })
}

// report judges the review and puts the verdict on the message's
// work.
//
// A workspace that cannot be read leaves the review's own verdict
// standing: nothing is known about what the fixes left behind.
func (v *selfVerdict) report() {
	defer v.work.Release()
	if !v.before.known {
		// The workspace could not be read when the review started:
		// there is no baseline to judge against.
		return
	}
	after, err := readWorkspaceState(v.ctx, v.workspace)
	if err != nil || !after.known {
		return
	}
	v.work.Report(judgeReview(v.before, after, v.findings, v.runner))
}

// judgeReview decides what the review produced.
//
// A review with no findings did its job — there was nothing to
// change. A review whose findings were addressed left something
// behind: the head moved, or the worktree carries changes that were
// not there before. A review whose findings were not addressed is an
// exception.
func judgeReview(before, after workspaceState, findings int, runner string) messages.Outcome {
	data := map[string]string{"agent": runner}
	if findings > 0 {
		data["findings"] = strconv.Itoa(findings)
	}
	if findings == 0 {
		return messages.Outcome{
			Status:  messages.OutcomeOK,
			Summary: "the review found nothing to fix",
			Data:    data,
		}
	}
	if after.head != before.head || after.status != before.status {
		return messages.Outcome{
			Status:  messages.OutcomeOK,
			Summary: "the review's findings were addressed",
			Data:    data,
		}
	}
	return command.FailedOutcomeData("not_fixed", "the review's findings were not addressed", data,
		"the review found "+strconv.Itoa(findings)+" finding(s) and the workspace carries neither a new commit nor a new change")
}

// countFindings counts the findings the review's groups reported.
//
// The count comes from the parsed reports the fan-out handed the
// collector — never from the review text.
func countFindings(outcomes []reviewOutcome) int {
	n := 0
	for _, o := range outcomes {
		n += len(o.report.Findings)
	}
	return n
}
