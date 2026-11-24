package review

// The verdict /review judges itself by.
//
// A review finds problems and hands them to the coding agent; the
// fixes land in the turn the findings were injected into. The
// review therefore knows two facts: how many findings it collected, and
// what the workspace holds once those findings were acted on. The
// baseline is the workspace as it is right before the findings go
// out — only the injected turn's effect is measured, so a change
// the user made while the review ran is not credited to it.
//
// A review that ran is a normal result: its groups finished, its
// findings reached the agent, and the agent's turn did not break. What
// came of the findings is a fact, not a branch — the `addressed` fact
// says whether the workspace carries a new commit or a new change
// since the findings went out. An agent that read the findings and
// judged them unfounded leaves the workspace alone, and that is a
// normal result too.
//
// The verdict speaks only when the turn the findings went into ended
// cleanly. A turn that broke carries its own result, and a workspace
// without a change is what a broken turn looks like — not a fact
// about the findings.

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
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
		v.once.Do(func() {
			if e.Reason != agent.PromptEndClean {
				// The turn did not end cleanly: the turn's own
				// result is the verdict, and a workspace that
				// carries no change is what a broken turn looks
				// like — not a fact about the findings. Give the
				// share back without saying anything.
				v.releaseNow()
				return
			}
			v.report()
		})
		return false
	})
	return v
}

// release gives the share of the message's work back without
// reporting a verdict — the path where no turn will run, so there is
// nothing to judge.
func (v *selfVerdict) release() {
	v.once.Do(v.releaseNow)
}

// releaseNow gives the share of the message's work back and drops the
// bus subscription.
func (v *selfVerdict) releaseNow() {
	v.unsubscribe()
	v.work.Release()
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
		defer v.releaseNow()
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
// not there before. A review whose findings were not addressed is a
// normal result too: the agent read the findings and left the
// workspace alone. The two cases differ in a fact, not in the status.
//
// The fact is what a consumer reads: the settled outcome keeps the
// words the delivery reported, so the `addressed` fact is the only
// thing that says what came of the findings.
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
	addressed := after.head != before.head || after.status != before.status
	data[BoolKeyAddressed] = strconv.FormatBool(addressed)
	text := "the review's findings were not addressed"
	if addressed {
		text = "the review's findings were addressed"
	}
	return outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: text,
		Data:       data,
	}
}
