// Package decision implements the decision protocols.
//
// Two wire formats are supported:
//
//   - decisions — POST {base}/v1/decisions, an ordered question list
//     of predicate, choice and score questions;
//   - systemone — POST {base}/v1/systemone, a question map keyed by
//     name with noul, choice and score questions.
//
// Both answer with a map keyed by question name, and both are
// normalised into the neutral llm.Answer, so the caller sees the same
// shape whichever protocol answered.
package decision

import (
	"strings"

	"github.com/cnlangzi/nightme/internal/llm"
)

// wireAnswer is the neutral shape both protocols answer with. The
// fields a protocol does not send stay at their zero value.
type wireAnswer struct {
	// Type is the question type the protocol answered.
	Type string `json:"type"`
	// Probability is P(yes) for a yes/no question.
	Probability *float64 `json:"probability"`
	// Choice is the value the model picked for a choice question.
	Choice string `json:"choice"`
	// Score is the level index of a score question.
	Score *float64 `json:"score"`
	// Probabilities holds the per-option or per-level
	// probabilities.
	Probabilities map[string]float64 `json:"probabilities"`
	// Confidence is how sure the model is.
	Confidence *float64 `json:"confidence"`
	// Refused is true when the model declined to answer.
	Refused bool `json:"refused"`
}

// normalise turns a wire answer into the neutral answer.
//
// The question is the reference: it says which shape the answer has,
// and it carries the levels a score is counted against.
func normalise(q llm.Question, w wireAnswer) llm.Answer {
	out := llm.Answer{Type: q.Type, Probabilities: w.Probabilities, Refused: w.Refused}
	if out.Probabilities == nil {
		out.Probabilities = map[string]float64{}
	}
	switch q.Type {
	case llm.QuestionYesNo:
		if w.Probability != nil {
			out.Probability = *w.Probability
		}
	case llm.QuestionChoice:
		out.Choice = w.Choice
		out.Probabilities = w.Probabilities
		out.Confidence = confidenceOf(w, out.Probabilities[out.Choice])
	case llm.QuestionScore:
		out.Score = scoreOf(q, w)
		out.Probabilities = w.Probabilities
		out.Confidence = confidenceOf(w, maxProbability(out.Probabilities))
	}
	return out
}

// confidenceOf returns the reported confidence, falling back to the
// probability of the picked option.
func confidenceOf(w wireAnswer, fallback float64) float64 {
	if w.Confidence != nil {
		return *w.Confidence
	}
	return fallback
}

// maxProbability returns the highest probability of a set.
func maxProbability(probabilities map[string]float64) float64 {
	highest := 0.0
	for _, p := range probabilities {
		if p > highest {
			highest = p
		}
	}
	return highest
}

// scoreOf returns the level index of a score answer: the reported one,
// or the probability-weighted index of the levels.
func scoreOf(q llm.Question, w wireAnswer) float64 {
	if w.Score != nil {
		return *w.Score
	}
	if len(w.Probabilities) == 0 {
		return 0
	}
	var weighted, total float64
	for i, level := range q.Levels {
		p := w.Probabilities[level.Label]
		weighted += p * float64(i)
		total += p
	}
	if total == 0 {
		return 0
	}
	return weighted / total
}

// wireType maps a neutral question type onto a protocol's question
// type.
func wireType(t llm.QuestionType, protocol string) string {
	if protocol == "systemone" {
		if t == llm.QuestionYesNo {
			return "noul"
		}
		return string(t)
	}
	if t == llm.QuestionYesNo {
		return "predicate"
	}
	return string(t)
}

// instructions returns the question's instruction, or the default.
func instructions(q llm.Question) string {
	if strings.TrimSpace(q.Instructions) == "" {
		return llm.DefaultInstructions
	}
	return q.Instructions
}
