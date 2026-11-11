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
	llm.RegisterDecider("decisions", NewDecisions)
}

// Decisions talks to a /v1/decisions endpoint.
type Decisions struct {
	baseURL string
	model   string
	apiKey  string
	client  *http.Client
}

// NewDecisions builds a decider for the decisions protocol.
func NewDecisions(cfg llm.Config) (llm.Decider, error) {
	base := strings.TrimRight(strings.TrimSpace(cfg.BaseURL), "/")
	if base == "" {
		return nil, fmt.Errorf("llm/decision: base_url is required")
	}
	key, err := llm.APIKey(cfg.APIKeyEnv)
	if err != nil {
		return nil, err
	}
	return &Decisions{
		baseURL: base,
		model:   strings.TrimSpace(cfg.Model),
		apiKey:  key,
		client:  &http.Client{Timeout: 5 * time.Minute},
	}, nil
}

// decisionsRequest is the body of a decisions call.
type decisionsRequest struct {
	Model     string              `json:"model"`
	Input     decisionsInput      `json:"input"`
	Questions []decisionsQuestion `json:"questions"`
}

// decisionsInput carries the state the model judges.
type decisionsInput struct {
	Role    string             `json:"role"`
	Content []decisionsContent `json:"content"`
}

// decisionsContent is one part of the input.
type decisionsContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// decisionsQuestion is one question on the wire.
type decisionsQuestion struct {
	Name         string            `json:"name"`
	Type         string            `json:"type"`
	Instructions string            `json:"instructions"`
	TrueMeans    string            `json:"true_means,omitempty"`
	FalseMeans   string            `json:"false_means,omitempty"`
	Choices      []decisionsChoice `json:"choices,omitempty"`
	Levels       []decisionsLevel  `json:"levels,omitempty"`
}

// decisionsChoice is one option of a choice question.
type decisionsChoice struct {
	Value       string `json:"value"`
	Description string `json:"description,omitempty"`
}

// decisionsLevel is one rung of a score question.
type decisionsLevel struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// decisionsResponse is the answer of a decisions call.
type decisionsResponse struct {
	Model   string      `json:"model"`
	Answers wireAnswers `json:"answers"`
	Usage   *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// Decide implements llm.Decider.
func (d *Decisions) Decide(ctx context.Context, req llm.DecisionRequest) (*llm.DecisionResponse, error) {
	if err := req.Validate(); err != nil {
		return nil, err
	}
	body := decisionsRequest{
		Model: d.model,
		Input: decisionsInput{
			Role: "user",
			Content: []decisionsContent{
				{Type: "input_text", Text: req.State},
			},
		},
	}
	for _, q := range req.Questions {
		body.Questions = append(body.Questions, toDecisionsQuestion(q))
	}

	var answer decisionsResponse
	headers := map[string]string{}
	if d.apiKey != "" {
		headers["Authorization"] = "Bearer " + d.apiKey
	}
	if err := llm.PostJSON(ctx, d.client, endpoint(d.baseURL, "/decisions"), headers, body, &answer); err != nil {
		return nil, err
	}

	out := &llm.DecisionResponse{
		Model:   llm.FirstNonEmpty(answer.Model, d.model),
		Answers: map[string]llm.Answer{},
	}
	answers := pairAnswers(req.Questions, answer.Answers)
	for _, q := range req.Questions {
		w, ok := answers[q.Name]
		if !ok {
			out.Answers[q.Name] = emptyAnswer(q.Type)
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

// toDecisionsQuestion maps a neutral question onto the wire.
func toDecisionsQuestion(q llm.Question) decisionsQuestion {
	out := decisionsQuestion{
		Name:         q.Name,
		Type:         wireType(q.Type, "decisions"),
		Instructions: instructions(q),
		TrueMeans:    q.TrueMeans,
		FalseMeans:   q.FalseMeans,
	}
	for _, o := range q.Options {
		out.Choices = append(out.Choices, decisionsChoice{Value: o.Value, Description: o.Description})
	}
	for _, l := range q.Levels {
		out.Levels = append(out.Levels, decisionsLevel{Label: l.Label, Description: l.Description})
	}
	return out
}
