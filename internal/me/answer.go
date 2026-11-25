// answer.go — what the operator's commands answer.
//
// The commands of the operator — `/me pause`, `/me resume`, `/me next`
// — answer on the card of the message they were typed in: the answer
// and the state of the run are two parts of one card, and the operator
// reads them in one place.
//
// The wording lives here, and not in the command layer: the service
// knows what the command did with the step the run is on, and the
// answer has to come before the state on the card.
package me

import (
	"context"
	"fmt"
)

// The marks the answers carry.
//
// The mark says what the command did with the step the run is on.
const (
	// pauseMark marks a pause in the chat: the answer of `/me pause`,
	// the phase line of a paused run and the notice the run sends when
	// it stops itself carry it.
	pauseMark = "⏸ "
	// resumeMark marks the answer of `/me resume`.
	resumeMark = "▶ "
	// nextMark marks the answer of `/me next`.
	nextMark = "⏭ "
)

// pauseAnswer is what `/me pause` answers: the run stops after the step
// it is on.
const pauseAnswer = pauseMark + "The run pauses after the current step."

// resumeAnswer renders what `/me resume` answers.
//
// The step runs again when there is something to run again. A step the
// command cannot act on is named, with the reason: the operator can
// tell that their command did nothing.
func resumeAnswer(d OperatorDecision) string {
	if !d.Act {
		return resumeMark + "The step " + d.Step + " " + d.Why + " — it was not run again."
	}
	return resumeMark + "The step runs again: " + d.Step + "."
}

// nextAnswer renders what `/me next` answers.
//
// The step is skipped when there is a step to pass over. A step the
// command cannot skip is named, with the reason.
//
// A command that named a step passes the steps of the round over up to
// and including it: the answer names the last step that was passed
// over.
func nextAnswer(d OperatorDecision) string {
	if !d.Act {
		return nextMark + "The step " + d.Step + " " + d.Why + " — there was nothing to skip."
	}
	if d.Through {
		return nextMark + "The steps up to and including " + d.Step + " are skipped. The run goes on with the workflow."
	}
	return nextMark + "The step is skipped: " + d.Step + ". The run goes on with the workflow."
}

// decisionAnswer renders what the operator's command answers.
//
// The kind says which command it was: `/me resume` or `/me next`.
func decisionAnswer(kind string, d OperatorDecision) string {
	if kind == directiveNext {
		return nextAnswer(d)
	}
	return resumeAnswer(d)
}

// answerCommand answers the operator's command and shows the state of
// the run on the same card.
//
// The card is the one the chat already holds for the message the
// command was typed in: the operator is looking right at it, and a card
// of its own would leave two cards under one command.
//
// The answer comes first: it is the answer to the command, and the state
// says where the run stands after it.
//
// A run that reports its state on its own has no such message: the
// state then opens a card of its own, the first line of that card being
// the state of the run.
func (s *Service) answerCommand(ctx context.Context, r *run, messageID, answer string) {
	it, inRound := r.roundIteration()
	// A card of the run's own is opened for a pause: the run stops and
	// shows where it stands. A card the operator asked from is where the
	// operator is looking.
	own := messageID == ""
	sess := r.stateSession(it, inRound)
	if messageID == "" {
		// The state opens a card of its own: the marker goes out
		// without an anchor, the chat answers with the id of the card
		// it wrote, and everything the state carries lands on that
		// card from now on.
		//
		// The card of the state is a session of its own, and it names
		// itself after what it is: the state. The name of a session is
		// what the chat shows in the footer of its card, and the card
		// of the round keeps its own name.
		sess.id = r.stateSessionID()
		sess.anchor = s.publishReply(ctx, r, sess, stateMarker(r.phaseLine()))
		s.publishReply(ctx, r, sess, answer)
		// The identity comes after the marker: the marker is the line
		// that opens the card, and the identity belongs to the card it
		// opened.
		s.publishInit(ctx, r, sess)
		s.adoptState(r, sess, it, inRound, own)
		s.publishStateBody(ctx, r, sess, it, inRound)
		return
	}
	sess.anchor = messageID
	// The session names itself before it speaks: a channel that shows
	// the identity as a message of its own leads the card with it.
	s.publishInit(ctx, r, sess)
	s.publishReply(ctx, r, sess, answer)
	s.adoptState(r, sess, it, inRound, own)
	s.publishState(ctx, r, sess, it, inRound)
}

// stateMarker renders the line that opens the card of the state.
//
// The line is the state of the run itself: the card is read on its own,
// and the first line says where the run stands. It carries the phase
// line in bold, the way the card of a decision opens with its takeover
// in bold: the state has to be seen at once among the entries of the
// card.
func stateMarker(phase string) string {
	return fmt.Sprintf("**%s**", phase)
}
