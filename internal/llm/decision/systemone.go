package decision

import (
	"context"

	"github.com/cnlangzi/nightme/internal/llm"
)

func init() {
	llm.RegisterDecisionProtocol(protocolSystemOne, systemonePath, NewSystemOne)
}

// protocolSystemOne is the name the systemone protocol is registered
// under.
const protocolSystemOne = "systemone"

// systemonePath is the path the protocol appends to the base URL.
const systemonePath = "/systemone"

// SystemOne talks to a /systemone endpoint.
type SystemOne struct {
	settings
}

// NewSystemOne builds a decider for the systemone protocol.
func NewSystemOne(cfg llm.DeciderConfig) (llm.Decider, error) {
	s, err := newSettings(cfg)
	if err != nil {
		return nil, err
	}
	return &SystemOne{settings: s}, nil
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

	var answer decisionResponse
	headers := map[string]string{}
	if s.apiKey != "" {
		headers["Authorization"] = "Bearer " + s.apiKey
	}
	url := llm.Endpoint(s.baseURL, systemonePath)
	if err := llm.PostJSON(ctx, s.client, url, s.requestHeaders(headers), body, &answer); err != nil {
		return nil, err
	}
	return buildResponse(req.Questions, answer, s.model), nil
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
