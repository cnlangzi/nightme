package decision

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
)

func init() {
	llm.RegisterDecider("systemone", NewSystemOne)
}

// SystemOne talks to a /v1/systemone endpoint.
type SystemOne struct {
	baseURL string
	model   string
	apiKey  string
	client  *http.Client
}

// NewSystemOne builds a decider for the systemone protocol.
func NewSystemOne(cfg llm.Config) (llm.Decider, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("llm/decision: base_url is required")
	}
	key, err := llm.APIKey(cfg.APIKeyEnv)
	if err != nil {
		return nil, err
	}
	return &SystemOne{
		baseURL: base,
		model:   strings.TrimSpace(cfg.Model),
		apiKey:  key,
		client:  &http.Client{Timeout: 5 * time.Minute},
	}, nil
}

// systemoneRequest is the body of a systemone call.
type systemoneRequest struct {
	Model     string                       `json:"model"`
	State     string                       `json:"state"`
	Questions map[string]systemoneQuestion `json:"questions"`
}

// systemoneQuestion is one question on the wire. The criteria hold
// the options of a choice question and the levels of a score one.
type systemoneQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

// systemoneResponse is the answer of a systemone call.
type systemoneResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
	Usage   *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Decide implements llm.Decider.
func (s *SystemOne) Decide(ctx context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	body := systemoneRequest{
		Model:     s.model,
		State:     req.State,
		Questions: map[string]systemoneQuestion{},
	}
	for _, q := range req.Questions {
		body.Questions[q.Name] = toSystemOneQuestion(q)
	}

	var answer systemoneResponse
	headers := map[string]string{}
	if s.apiKey != "" {
		headers["Authorization"] = "Bearer " + s.apiKey
	}
	if err := llm.PostJSON(ctx, s.client, s.baseURL+"/v1/systemone", headers, body, &answer); err != nil {
		return nil, err
	}

	out := &llm.DecisionResponse{
		Model:   llm.FirstNonEmpty(answer.Model, s.model),
		Answers: map[string]llm.Answer{},
	}
	for _, q := range req.Questions {
		w, ok := answer.Answers[q.Name]
		if !ok {
			out.Answers[q.Name] = llm.Answer{Type: q.Type}
			continue
		}
		out.Answers[q.Name] = normalise(q, w)
	}
	if answer.Usage != nil {
		out.Usage = llm.Usage{
			InputTokens:  answer.Usage.InputTokens,
			OutputTokens: answer.Usage.OutputTokens,
		}
	}
	return out, nil
}

// toSystemOneQuestion maps a neutral question onto the wire. A choice
// question carries a criteria map, a score question a criteria array.
func toSystemOneQuestion(q llm.Question) systemoneQuestion {
	out := systemoneQuestion{
		Type:         wireType(q.Type, "systemone"),
		Instructions: instructions(q),
	}
	switch q.Type {
	case llm.QuestionChoice:
		criteria := make(map[string]string, len(q.Options))
		for _, o := range q.Options {
			criteria[o.Value] = o.Description
		}
		out.Criteria = criteria
	case llm.QuestionScore:
		levels := make([]string, 0, len(q.Levels))
		for _, l := range q.Levels {
			levels = append(levels, l.Label)
		}
		out.Criteria = levels
	}
	return out
}
