package llm

import (
	"context"
	"fmt"
	"strings"
)

// QuestionType is the shape of a decision question.
type QuestionType string

const (
	// QuestionYesNo is a boolean question. The answer carries the
	// probability that the answer is yes.
	QuestionYesNo QuestionType = "yesno"
	// QuestionChoice is a question with a fixed set of options. The
	// answer names one of them.
	QuestionChoice QuestionType = "choice"
	// QuestionScore is a question with an ordered scale of levels.
	// The answer carries the probability-weighted level index.
	QuestionScore QuestionType = "score"
)

// Option is one choice of a choice question.
type Option struct {
	// Value is the value the answer names.
	Value string `json:"value"`
	// Description tells the model when to pick this option.
	Description string `json:"description,omitempty"`
}

// Level is one rung of a score question's scale.
type Level struct {
	// Label names the level.
	Label string `json:"label"`
	// Description tells the model what the level means.
	Description string `json:"description,omitempty"`
}

// Question is one typed question about a state.
type Question struct {
	// Name keys the answer.
	Name string `json:"name"`
	// Type is the shape of the answer.
	Type QuestionType `json:"type"`
	// Instructions tells the model what to judge.
	Instructions string `json:"instructions,omitempty"`
	// Options are the choices of a choice question.
	Options []Option `json:"options,omitempty"`
	// Levels are the rungs of a score question, lowest first.
	Levels []Level `json:"levels,omitempty"`
	// TrueMeans and FalseMeans describe what yes and no mean for a
	// yes/no question. They are optional.
	TrueMeans  string `json:"true_means,omitempty"`
	FalseMeans string `json:"false_means,omitempty"`
}

// DecisionRequest is one call to a decision model.
type DecisionRequest struct {
	// State is the situation the model judges. It is text or JSON.
	State string `json:"state"`
	// Questions are the questions to answer, in order.
	Questions []Question `json:"questions"`
}

// Answer is the model's answer to one question.
type Answer struct {
	// Type is the shape of the answer.
	Type QuestionType `json:"type"`
	// Probability is P(yes) for a yes/no question.
	Probability float64 `json:"probability,omitempty"`
	// Choice is the value the model picked for a choice question.
	Choice string `json:"choice,omitempty"`
	// Score is the probability-weighted level index of a score
	// question, counted from zero.
	Score float64 `json:"score,omitempty"`
	// Probabilities holds the per-option or per-level
	// probabilities.
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	// Confidence is how sure the model is about a choice or a score.
	Confidence float64 `json:"confidence,omitempty"`
	// Refused is true when the model declined to answer.
	Refused bool `json:"refused,omitempty"`
}

// DecisionResponse is the model's answer to a decision request.
type DecisionResponse struct {
	// Model is the model that actually answered. It is always
	// recorded: a decision is only as good as the model behind it.
	Model string `json:"model"`
	// Answers holds one answer per question, keyed by question name.
	Answers map[string]Answer `json:"answers"`
	// Usage counts the tokens the call consumed.
	Usage Usage `json:"usage"`
}

// Decider is a model that answers typed questions about a state.
//
// A decider cannot generate text and cannot call tools, so it is a
// separate capability from Provider.
type Decider interface {
	// Decide answers every question of the request.
	Decide(ctx context.Context, req DecisionRequest) (*DecisionResponse, error)
}

// The bounds the decision protocols impose on a request. They are
// checked before anything is sent, so a malformed request never
// reaches the network.
const (
	// MinQuestions is the smallest number of questions a request may
	// carry.
	MinQuestions = 1
	// MaxQuestions is the largest number of questions a request may
	// carry.
	MaxQuestions = 128
	// MinOptions is the smallest number of options a choice question
	// may carry.
	MinOptions = 2
	// MaxOptions is the largest number of options a choice question
	// may carry.
	MaxOptions = 255
	// MinLevels is the smallest number of levels a score question may
	// carry.
	MinLevels = 2
	// MaxLevels is the largest number of levels a score question may
	// carry.
	MaxLevels = 10
)

// DefaultInstructions is the instruction a question without one gets.
const DefaultInstructions = "Which of these applies?"

// Validate checks a decision request against the protocol bounds.
// Every problem is reported, so one round of validation is enough.
func (r DecisionRequest) Validate() error {
	if len(r.Questions) < MinQuestions || len(r.Questions) > MaxQuestions {
		return fmt.Errorf("llm: a decision request carries %d questions, it must carry %d to %d",
			len(r.Questions), MinQuestions, MaxQuestions)
	}
	seen := make(map[string]bool, len(r.Questions))
	for i, q := range r.Questions {
		name := strings.TrimSpace(q.Name)
		if name == "" {
			return fmt.Errorf("llm: question %d has no name", i+1)
		}
		if seen[name] {
			return fmt.Errorf("llm: question %q is asked twice", name)
		}
		seen[name] = true
		if err := q.validate(); err != nil {
			return fmt.Errorf("llm: question %q: %w", name, err)
		}
	}
	return nil
}

// validate checks one question against the protocol bounds.
func (q Question) validate() error {
	switch q.Type {
	case QuestionYesNo:
		return nil
	case QuestionChoice:
		if len(q.Options) < MinOptions || len(q.Options) > MaxOptions {
			return fmt.Errorf("a choice question carries %d options, it must carry %d to %d",
				len(q.Options), MinOptions, MaxOptions)
		}
		seen := make(map[string]bool, len(q.Options))
		for _, o := range q.Options {
			if strings.TrimSpace(o.Value) == "" {
				return fmt.Errorf("an option has no value")
			}
			if seen[o.Value] {
				return fmt.Errorf("option %q is listed twice", o.Value)
			}
			seen[o.Value] = true
		}
		return nil
	case QuestionScore:
		if len(q.Levels) < MinLevels || len(q.Levels) > MaxLevels {
			return fmt.Errorf("a score question carries %d levels, it must carry %d to %d",
				len(q.Levels), MinLevels, MaxLevels)
		}
		return nil
	default:
		return fmt.Errorf("unknown question type %q", q.Type)
	}
}
