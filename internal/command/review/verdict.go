package review

// The verdict /review judges itself by.
//
// A review finds problems and hands them to the coding agent; the
// fixes land in the turn the findings were injected into. The review
// therefore knows two facts: how many findings it collected, and
// what the workspace holds once those findings were acted on. The
// baseline is the workspace as it is right before the findings go
// out — only the injected turn's effect is measured, so a change
// the user made while the review ran is not credited to it.
//
// Nothing to fix is a normal result — the review did its job, there
// was nothing to change. A review whose findings were addressed is a
// normal result too: it left something behind, a new commit or a new
// change in the working tree. A review whose findings were not
// addressed is an exception: the review's work did not land.

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// verdictGitTimeout bounds the git calls a verdict makes. The
// judgment runs on its own goroutine, but a hung git must not keep
// the message from settling forever.
const verdictGitTimeout = 30 * time.Second

// baselineTimeout bounds the read of the workspace's baseline. The
// read happens right before the findings go out, so a slow git must
// not hold the findings back.
const baselineTimeout = 10 * time.Second

// selfVerdict judges the review once the turn its findings went into
// has ended.
type selfVerdict struct {
	work      *chatsession.Work
	unsub     func()
	ctx       context.Context
	workspace string
	before    workspaceState
	runner    string
	findings  int
	once      sync.Once
}

// watchSelfVerdict takes a share of the message's work and waits for
// the turn the findings were injected into to end.
//
// The share keeps the message alive until the verdict is on it: the
// settled signal goes out when the last participant releases its
// share.
//
// The subscription happens before the injection: the bus calls its
// subscribers in registration order, so this one is in place before
// the queue's own subscriber releases the queue's share.
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
	v.unsub = bus.Subscribe(func(e chatsession.PromptEndedEvent) bool {
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
	v.once.Do(func() {
		v.unsubscribe()
		v.work.Release()
	})
}

// unsubscribe drops the bus subscription. The handler holds the
// verdict — and through it the workspace and the message's work — so
// it must not outlive the verdict.
func (v *selfVerdict) unsubscribe() {
	if v.unsub != nil {
		v.unsub()
	}
}

// report judges the review and puts the verdict on the message's
// work.
//
// The judgment runs on its own goroutine: the bus publishes the
// prompt end on the chat's event pump, and the git calls the
// judgment makes must not hold that pump up.
//
// A workspace that cannot be read leaves the review's own verdict
// standing: nothing is known about what the fixes left behind.
func (v *selfVerdict) report() {
	go func() {
		defer v.unsubscribe()
		defer v.work.Release()
		if !v.before.known {
			// The workspace could not be read before the findings
			// went out: there is no baseline to judge against.
			return
		}
		ctx, cancel := context.WithTimeout(v.ctx, verdictGitTimeout)
		defer cancel()
		after, err := readWorkspaceState(ctx, v.workspace)
		if err != nil || !after.known {
			return
		}
		v.work.Report(judgeReview(v.before, after, v.findings, v.runner))
	}()
}

// judgeReview decides what the review produced.
//
// A review with no findings did its job — there was nothing to
// change. A review whose findings were addressed left something
// behind: the head moved, or the worktree carries changes that were
// not there before. A review whose findings were not addressed is an
// exception.
func judgeReview(before, after workspaceState, findings int, runner string) outcome.Outcome {
	data := map[string]string{KeyAgent: runner}
	if findings > 0 {
		data[KeyFindings] = strconv.Itoa(findings)
	}
	if findings == 0 {
		return outcome.Outcome{
			Success:    true,
			Status:     http.StatusOK,
			StatusText: "the review found nothing to fix",
			Data:       data,
		}
	}
	if after.head != before.head || after.status != before.status {
		return outcome.Outcome{
			Success:    true,
			Status:     http.StatusOK,
			StatusText: "the review's findings were addressed",
			Data:       data,
		}
	}
	return command.FailedOutcomeData(http.StatusConflict,
		"the review found "+strconv.Itoa(findings)+" finding(s) and the workspace carries neither a new commit nor a new change",
		data)
}
