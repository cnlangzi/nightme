// Package decision implements the decision protocols.
//
// Two wire formats are supported:
//
//   - decisions — POST {base}/v1/decisions, an ordered question list
//     of predicate, choice and score questions;
//   - systemone — POST {base}/v1/systemone, a question map keyed by
//     name with noul, choice and score questions.
//
// The two protocols answer in different shapes — the decisions
// protocol with an array of answers, the systemone protocol with a
// map keyed by question name. Both are normalised into the neutral
// llm.Answer and keyed by question name, so the caller sees the same
// shape whichever protocol answered.
package decision

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/cnlangzi/nightme/internal/llm"
)

// typeRefusal is the answer type a refusal carries.
const typeRefusal = "refusal"

// wireAnswer is the neutral shape both protocols answer with. The
// fields a protocol does not send stay at their zero value.
type wireAnswer struct {
	// Name is the question the answer belongs to. A protocol that
	// answers with a map carries it in the key instead.
	Name string `json:"name"`
	// Type is the question type the protocol answered. A refusal is
	// its own type.
	Type string `json:"type"`
	// Probability is P(yes) for a yes/no question.
	Probability *float64 `json:"probability"`
	// Noul is P(yes) for a yes/no question, under the name the
	// systemone protocol gives it.
	Noul *float64 `json:"noul"`
	// Choice is the value the model picked for a choice question.
	Choice string `json:"choice"`
	// Score is the level index of a score question.
	Score *float64 `json:"score"`
	// Probabilities holds the per-option or per-level probabilities.
	Probabilities wireProbabilities `json:"probabilities"`
	// Confidence is how sure the model is.
	Confidence *float64 `json:"confidence"`
	// Refused is true when the model declined to answer.
	Refused bool `json:"refused"`
}

// wireProbabilities holds the per-option or per-level probabilities.
//
// The protocols report them either as a map of key to probability or
// as an array of objects, so both are read.
type wireProbabilities map[string]float64

// probabilityItem is one entry of the array shape of the
// probabilities.
type probabilityItem struct {
	// Key names the option or the level.
	Key string `json:"key"`
	// Value is the option, or the index of the level.
	Value any `json:"value"`
	// Label names the level of a score question.
	Label string `json:"label"`
	// Probability is the chance of the option or the level.
	Probability float64 `json:"probability"`
}

// UnmarshalJSON reads the probabilities in either shape.
func (p *wireProbabilities) UnmarshalJSON(raw []byte) error {
	var asMap map[string]float64
	if err := json.Unmarshal(raw, &asMap); err == nil {
		*p = asMap
		return nil
	}
	var asList []probabilityItem
	if err := json.Unmarshal(raw, &asList); err != nil {
		return err
	}
	out := make(wireProbabilities, len(asList))
	for _, item := range asList {
		out[item.name()] = item.Probability
	}
	*p = out
	return nil
}

// name returns the name an item of the probability list is filed
// under.
func (i probabilityItem) name() string {
	if i.Label != "" {
		return i.Label
	}
	if i.Key != "" {
		return i.Key
	}
	return textOf(i.Value)
}

// textOf renders a choice value as text.
func textOf(v any) string {
	switch t := v.(type) {
	case string:
		return t
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	default:
		return ""
	}
}

// wireAnswers holds the answers of a decision call.
//
// The decisions protocol answers with an array, one answer per
// question, in the order the questions were asked, each naming its
// question. The systemone protocol answers with a map keyed by the
// question name. Both are read into the same shape.
type wireAnswers struct {
	// named holds the answers that named their question.
	named map[string]wireAnswer
	// ordered holds the answers that did not, in the order they came.
	ordered []wireAnswer
}

// UnmarshalJSON reads the answers in either shape.
func (a *wireAnswers) UnmarshalJSON(raw []byte) error {
	var asList []wireAnswer
	if err := json.Unmarshal(raw, &asList); err == nil {
		a.named = make(map[string]wireAnswer, len(asList))
		for _, ans := range asList {
			if ans.Name != "" {
				a.named[ans.Name] = ans
				continue
			}
			a.ordered = append(a.ordered, ans)
		}
		return nil
	}
	var asMap map[string]wireAnswer
	if err := json.Unmarshal(raw, &asMap); err != nil {
		return err
	}
	a.named = asMap
	return nil
}

// pairAnswers pairs the questions with the answers of the protocol.
//
// An answer that names its question is paired by that name. An answer
// that does not is paired with the question that has no named answer
// yet, in the order the questions were asked.
func pairAnswers(questions []llm.Question, answers wireAnswers) map[string]wireAnswer {
	out := make(map[string]wireAnswer, len(questions))
	cursor := 0
	for _, q := range questions {
		if ans, ok := answers.named[q.Name]; ok {
			out[q.Name] = ans
			continue
		}
		if cursor < len(answers.ordered) {
			out[q.Name] = answers.ordered[cursor]
			cursor++
		}
	}
	return out
}

// normalise turns a wire answer into the neutral answer.
//
// The question is the reference: it says which shape the answer has,
// and it carries the levels a score is counted against.
func normalise(q llm.Question, w wireAnswer) llm.Answer {
	out := llm.Answer{
		Type:    q.Type,
		Refused: w.Refused || w.Type == typeRefusal,
	}
	switch q.Type {
	case llm.QuestionYesNo:
		out.Probability = probability(w)
	case llm.QuestionChoice:
		out.Choice = w.Choice
		out.Probabilities = w.Probabilities
		out.Confidence = confidenceOf(w, out.Probabilities[out.Choice])
	case llm.QuestionScore:
		out.Score = scoreOf(q, w)
		out.Probabilities = levelProbabilities(q, w.Probabilities)
		out.Confidence = confidenceOf(w, maxProbability(out.Probabilities))
	}
	if out.Probabilities == nil {
		out.Probabilities = map[string]float64{}
	}
	return out
}

// emptyAnswer is the answer of a question the protocol did not answer.
func emptyAnswer(t llm.QuestionType) llm.Answer {
	return llm.Answer{Type: t, Probabilities: map[string]float64{}}
}

// probability returns the probability of a yes/no answer.
func probability(w wireAnswer) float64 {
	if w.Noul != nil {
		return *w.Noul
	}
	if w.Probability != nil {
		return *w.Probability
	}
	return 0
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
	probabilities := levelProbabilities(q, w.Probabilities)
	var weighted, total float64
	for i, level := range q.Levels {
		p := probabilities[level.Label]
		weighted += p * float64(i)
		total += p
	}
	if total == 0 {
		return 0
	}
	return weighted / total
}

// levelProbabilities keys the per-level probabilities by level label.
//
// A server that counts the levels from zero keys them by their
// position; the neutral shape names the level instead, so the caller
// reads the same key whichever server answered.
func levelProbabilities(q llm.Question, probabilities wireProbabilities) map[string]float64 {
	out := make(map[string]float64, len(probabilities))
	for key, p := range probabilities {
		out[levelLabel(q, key)] = p
	}
	return out
}

// levelLabel returns the label of the level a probability key names.
func levelLabel(q llm.Question, key string) string {
	idx, err := strconv.Atoi(key)
	if err != nil || idx < 0 || idx >= len(q.Levels) {
		return key
	}
	return q.Levels[idx].Label
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

// endpoint returns the URL of a versioned endpoint.
//
// The base of a gateway usually already carries the version segment,
// so the version is added only when it is missing.
func endpoint(base, path string) string {
	base = strings.TrimRight(base, "/")
	if strings.HasSuffix(base, "/v1") {
		return base + path
	}
	return base + "/v1" + path
}
