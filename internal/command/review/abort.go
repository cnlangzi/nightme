// Package review — /review --abort: stop the review that is running
// in this chat.
//
// /review fans out over one agent session per review group and runs
// asynchronously: the command returns immediately and a goroutine
// drives the fan-out for minutes. The command layer holds no handle
// on those sessions — they belong to agent.RunSession /
// agent.ResumeSession — so the only thing an abort can reach is the
// run's context. Cancelling it closes the underlying agent sessions
// (the agent CLI children are terminated) and unwinds the fan-out.
//
// The aborted run stays resumable: its record holds the stopped
// groups with their session ids, so /review --continue reopens them
// and sends them the text "continue".
package review

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/chatsession"
	"github.com/cnlangzi/nightme/internal/command"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// errReviewAborted is the cancellation cause an abort installs on a
// running review's context.
//
// It is what tells "the user aborted this" apart from "the chat
// closed" (context.Canceled): only the sentinel produces the abort
// notice and the `cancelled` outcome.
var errReviewAborted = errors.New("review aborted")

// abortWait is how long the abort waits for the run to wind down
// before it answers.
//
// The wait makes the reply mean "the agent sessions are closed": a
// `--continue` sent right after is not refused as "a review is
// already running in this chat".
//
// A var, not a const: the test shortens it to cover the branch where
// the run does not wind down in time.
var abortWait = 5 * time.Second

// aborted reports whether ctx was cancelled by /review --abort.
//
// context.Cause walks to the root cause, so the sentinel survives
// every context derived from the run's own. A chat close reports
// context.Canceled — that is a close, not an abort.
func aborted(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errReviewAborted)
}

// handleAbort stops the running review of the chat.
//
// It needs neither a workspace nor a selected agent — it targets the
// run that is already in flight — so it is dispatched before the
// workspace / agent lookup.
//
// The reply names the runner of the aborted run, how long it had been
// running, the number of the chat's other runs that are still in
// flight, and whether the run is still winding down.
func (f *Factory) handleAbort(ctx context.Context, rt command.RuntimeServices, input command.SlashInput) (*command.SlashOutput, error) {
	h, others, ok := f.abortRun(input.ChatID)
	if !ok {
		chatsession.ReportOutcome(ctx, command.SuccessOutcome("no review is running in this chat", nil))
		return command.Reply(ctx, rt, "ℹ️ no review is running in this chat"), nil
	}

	winding := false
	timer := time.NewTimer(abortWait)
	defer timer.Stop()
	select {
	case <-h.done:
	case <-timer.C:
		winding = true
	}

	elapsed := time.Since(h.started)
	lines := []string{fmt.Sprintf("🛑 aborted the review (%s)", h.agent)}
	lines = append(lines, "→ was running: "+formatElapsed(elapsed))
	if others > 0 {
		lines = append(lines, "→ "+plural(others, "review")+" still running")
	}
	if winding {
		lines = append(lines, "⏳ the review is still winding down")
	}
	chatsession.ReportOutcome(ctx, outcome.Outcome{
		Success:    true,
		Status:     http.StatusOK,
		StatusText: fmt.Sprintf("aborted the review (%s)", h.agent),
		Data: map[string]string{
			KeyAgent:   h.agent,
			KeyKind:    h.kind,
			KeyElapsed: elapsed.String(),
		},
	})
	return command.Reply(ctx, rt, strings.Join(lines, "\n")), nil
}

// reportAborted reports an aborted run: the `cancelled` outcome and
// the abort notice on the run's own message.
//
// The notice tells the user what was stopped and how to pick the
// stopped groups up again. The merged review text is NOT injected
// into the chat session's agent session — the user chose to stop the
// run, and the continuation delivers the findings.
func reportAborted(ctx context.Context, work *chatsession.Work, emitter messages.Emitter,
	chatID, replyTo, runner, kind string, outcomes []reviewOutcome,
) {
	groups, unfinished := groupCounts(outcomes)
	data := unfinishedData(map[string]string{KeyAgent: runner, BoolKeyDelivered: "false"}, outcomes)
	work.Report(outcome.Outcome{
		Success:    false,
		Status:     outcome.StatusClientClosedRequest,
		StatusText: fmt.Sprintf("the %s was aborted (%d of %d groups unfinished)", kind, unfinished, groups),
		Data:       data,
	})
	if emitter == nil {
		return
	}
	// The notice rides the run's own message: the thread of the
	// review closes on the abort instead of on a failure. The run's
	// context is cancelled, so the send runs on a context that
	// carries its values but not its cancellation.
	_, _ = emitter.Send(context.WithoutCancel(ctx), messages.OutboundMessage{
		ChatID:  chatID,
		Kind:    messages.OutReply,
		ReplyTo: replyTo,
		Text:    abortNotice(unfinished, groups),
	})
}

// carrySessionIDs fills the session id of an outcome that has none
// from the job it resumed.
//
// A group the abort caught before it spawned reports no session id —
// the resume never got that far — but the session it was reopening is
// still the one the record holds. Without this the stopped group
// would look like it never reached the agent, and `--continue` would
// have nothing to resume.
func carrySessionIDs(outcomes []reviewOutcome, jobs []agent.ReviewJob) {
	for i := range outcomes {
		if outcomes[i].sessionID != "" {
			continue
		}
		for _, j := range jobs {
			// The pattern and the source together are the group's
			// identity — the same pair the record keys them by.
			if j.Pattern == outcomes[i].pattern && j.Source == outcomes[i].source {
				outcomes[i].sessionID = j.SessionID
				break
			}
		}
	}
}

// abortNotice renders the notice the aborted run sends on its own
// message: what was stopped, and how to pick the stopped groups up
// again.
func abortNotice(unfinished, groups int) string {
	notice := fmt.Sprintf("🛑 review aborted · %d of %d subagents unfinished",
		unfinished, groups)
	if unfinished > 0 {
		notice += " → /review --continue"
	}
	return notice
}

// formatElapsed renders a duration the way the abort receipt shows
// it: seconds below a minute, minutes + seconds below an hour,
// hours + minutes above that.
func formatElapsed(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm%02ds", int(d.Minutes()), int(d.Seconds())%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
}
