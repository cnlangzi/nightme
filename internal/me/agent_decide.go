package me

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/cnlangzi/nightme/internal/llm"
	"github.com/cnlangzi/nightme/internal/wfe"
)

// The options the decision model may pick besides a command.
const (
	// optionDone ends the run: the playbook is complete.
	optionDone = "done"
	// optionEscalate hands the next step to the chat model.
	optionEscalate = "escalate"
)

// The questions System 1 asks in one request.
const (
	questionNext          = "next"
	questionNeedsHuman    = "needs_human"
	questionStepSucceeded = "step_succeeded"
)

// syntheticCallID is the id of the tool call a System 1 decision is
// recorded as.
const syntheticCallID = "system1"

// transcriptBytes is how much of a transcript the models see.
const transcriptBytes = 4 << 10

// decision is what the decision model answered.
type decision struct {
	// Next is the command the model picked, or done or escalate.
	Next string
	// NextConfidence is how sure the model was about the pick.
	NextConfidence float64
	// NeedsHuman is the probability that a human has to step in.
	NeedsHuman float64
	// StepSucceeded is the model's answer about the last step, when
	// it was asked.
	StepSucceeded *bool
	// Model is the model that answered.
	Model string
	// Options are the choices the model was offered.
	Options []llm.Option
}

// decide asks the decision model what comes next.
//
// The request carries the rendered playbook, the remaining budget, the
// history of the run as outcomes, and — for the last action — its
// full outcome and the tail of its transcript.
func (s *Service) decide(ctx context.Context, r *run) (decision, error) {
	questions := s.decisionQuestions(r)
	resp, err := s.cfg.Decider.Decide(ctx, llm.DecisionRequest{
		State:     s.decisionState(r),
		Questions: questions,
	})
	if err != nil {
		return decision{}, fmt.Errorf("the decision model failed: %w", err)
	}
	if resp == nil {
		return decision{}, fmt.Errorf("the decision model answered nothing")
	}

	r.mu.Lock()
	r.system1Decisions++
	r.system1Usage.Add(resp.Usage)
	if resp.Model != "" {
		r.system1Model = resp.Model
	}
	r.mu.Unlock()

	dec := decision{Model: resp.Model, Options: s.decisionOptions(r)}
	if answer, ok := resp.Answers[questionNext]; ok {
		dec.Next = answer.Choice
		dec.NextConfidence = answer.Confidence
		if dec.NextConfidence == 0 {
			dec.NextConfidence = answer.Probabilities[answer.Choice]
		}
	}
	if answer, ok := resp.Answers[questionNeedsHuman]; ok {
		dec.NeedsHuman = answer.Probability
	}
	if answer, ok := resp.Answers[questionStepSucceeded]; ok {
		succeeded := answer.Probability >= 0.5
		dec.StepSucceeded = &succeeded
	}
	return dec, nil
}

// decisionQuestions builds the questions of one decision request.
//
// The questions are asked in one request, so the model sees the whole
// situation at once: what comes next, whether a human has to step in,
// and — only when the last outcome is unknown — whether the last step
// worked.
func (s *Service) decisionQuestions(r *run) []llm.Question {
	questions := []llm.Question{
		{
			Name:         questionNext,
			Type:         llm.QuestionChoice,
			Instructions: "Which step of the playbook comes next?",
			Options:      s.decisionOptions(r),
		},
		{
			Name:         questionNeedsHuman,
			Type:         llm.QuestionYesNo,
			Instructions: "Does the next step need the user's judgement or credentials?",
			TrueMeans:    "a human has to answer before the step can run",
			FalseMeans:   "the step can run on its own",
		},
	}
	if s.lastOutcomeUnknown(r) {
		questions = append(questions, llm.Question{
			Name:         questionStepSucceeded,
			Type:         llm.QuestionYesNo,
			Instructions: "Did the last step succeed?",
			TrueMeans:    "the last step did what it was asked to do",
			FalseMeans:   "the last step did not finish its work",
		})
	}
	return questions
}

// decisionOptions returns the choices the decision model is offered:
// the commands the policy accepts now, plus done and escalate.
//
// The description of a command is the note of the step that owns it,
// so the model reads why a step is there.
func (s *Service) decisionOptions(r *run) []llm.Option {
	options := make([]llm.Option, 0, 8)
	for _, cmd := range r.allowedCommands() {
		desc := ""
		if idx, err := r.checkRun(cmd); err == nil && r.playbook != nil && idx >= 0 && idx < len(r.playbook.Steps) {
			desc = r.playbook.Steps[idx].Note
		}
		options = append(options, llm.Option{Value: cmd, Description: desc})
	}
	options = append(options,
		llm.Option{Value: optionDone, Description: "the playbook is complete"},
		llm.Option{Value: optionEscalate, Description: "let the chat model decide and act"},
	)
	return options
}

// decisionState renders the state the decision model judges.
//
// The history is a list of outcomes — the status and the summary of
// every action — so the model reads facts instead of prose. The last
// action gets its full outcome and the tail of its transcript, because
// that is where the evidence for the next decision is.
func (s *Service) decisionState(r *run) string {
	r.mu.Lock()
	history := append([]wfe.ActionRecord(nil), r.policy.History...)
	remaining := r.policy.Remaining()
	lastTranscript := r.lastTranscript
	r.mu.Unlock()

	var b strings.Builder
	b.WriteString(r.renderPolicy())
	fmt.Fprintf(&b, "remaining actions: %d\n", remaining.Actions)
	for i, n := range remaining.Steps {
		if n < 0 {
			fmt.Fprintf(&b, "remaining actions for step %d: no limit of its own\n", i+1)
			continue
		}
		fmt.Fprintf(&b, "remaining actions for step %d: %d\n", i+1, n)
	}
	b.WriteString("history:\n")
	if len(history) == 0 {
		b.WriteString("  (nothing yet)\n")
	}
	for i, rec := range history {
		fmt.Fprintf(&b, "  %d. %s %s -> %s: %s\n", i+1, rec.Kind, rec.Command, rec.Outcome.Status, rec.Outcome.Summary)
	}
	if len(history) > 0 {
		last := history[len(history)-1]
		fmt.Fprintf(&b, "last action:\n  status: %s\n  summary: %s\n", last.Outcome.Status, last.Outcome.Summary)
		if len(last.Outcome.Data) > 0 {
			b.WriteString("  data:\n")
			for k, v := range last.Outcome.Data {
				fmt.Fprintf(&b, "    %s: %s\n", k, v)
			}
		}
	}
	if lastTranscript != "" {
		b.WriteString("last transcript (tail):\n")
		b.WriteString(tailText(lastTranscript, transcriptBytes))
		b.WriteString("\n")
	}
	return b.String()
}

// lastOutcomeUnknown reports whether the last action's outcome is
// unknown — the case where nobody said whether it worked.
func (s *Service) lastOutcomeUnknown(r *run) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	history := r.policy.History
	if len(history) == 0 {
		return false
	}
	return history[len(history)-1].Outcome.Status == wfe.OutcomeUnknown
}

// recordSynthetic appends a decision to the conversation as a
// synthetic assistant tool call.
//
// The chat model reads it as one more step of the run, so the history
// stays consistent when it takes over.
func (s *Service) recordSynthetic(r *run, tool string, args map[string]any, dec decision) {
	raw, err := json.Marshal(args)
	if err != nil {
		raw = json.RawMessage("{}")
	}
	note := fmt.Sprintf("decided by %s, p=%.2f", dec.Model, dec.NextConfidence)
	r.appendMessage(llm.Message{
		Role: llm.RoleAssistant,
		Parts: []llm.Part{{
			Type:       llm.PartToolCall,
			ToolCallID: syntheticCallID,
			ToolName:   tool,
			Args:       raw,
			Text:       note,
		}},
	})
}

// escalateQuestion renders the question a run asks the user when the
// decision model is not sure enough to act.
func escalateQuestion(dec decision) string {
	var b strings.Builder
	b.WriteString("Which step should run next?")
	for _, o := range dec.Options {
		fmt.Fprintf(&b, "\n- %s", o.Value)
	}
	b.WriteString("\nReply with the command to run.")
	return b.String()
}

// matchOption returns the option the user's reply names.
//
// The match ignores case and surrounding space. A reply that names no
// option returns an empty string.
func matchOption(options []llm.Option, reply string) string {
	reply = strings.TrimSpace(reply)
	if reply == "" {
		return ""
	}
	for _, o := range options {
		if strings.EqualFold(strings.TrimSpace(o.Value), reply) {
			return o.Value
		}
	}
	return ""
}

// tailText keeps the last n bytes of a text, cut on a line boundary
// when it can be.
func tailText(text string, n int) string {
	if len(text) <= n {
		return text
	}
	tail := text[len(text)-n:]
	if idx := strings.IndexByte(tail, '\n'); idx >= 0 && idx+1 < len(tail) {
		tail = tail[idx+1:]
	}
	return tail
}
