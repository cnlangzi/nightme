package decision

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/llm"
)

// neutralRequest is the request both protocols answer.
func neutralRequest() llm.DecisionRequest {
	return llm.DecisionRequest{
		State: "the run is halfway through the playbook",
		Questions: []llm.Question{
			{
				Name:         "next",
				Type:         llm.QuestionChoice,
				Instructions: "Which step comes next?",
				Options: []llm.Option{
					{Value: "/gtw commit", Description: "commit the work"},
					{Value: "/review", Description: "review the work"},
				},
			},
			{
				Name:         "needs_human",
				Type:         llm.QuestionYesNo,
				Instructions: "Does a human have to step in?",
				TrueMeans:    "a human has to answer",
				FalseMeans:   "the step can run on its own",
			},
			{
				Name:         "confidence",
				Type:         llm.QuestionScore,
				Instructions: "How sure are you?",
				Levels: []llm.Level{
					{Label: "low"},
					{Label: "high"},
				},
			},
		},
	}
}

// decisionsAnswer is the answer of the decisions protocol.
const decisionsAnswer = `{
  "model": "jev-1.14",
  "answers": {
    "next": {"type":"choice","choice":"/gtw commit","probabilities":{"/gtw commit":0.9,"/review":0.1},"confidence":0.9},
    "needs_human": {"type":"predicate","probability":0.05},
    "confidence": {"type":"score","probabilities":{"low":0.2,"high":0.8}}
  },
  "usage": {"input_tokens": 120, "output_tokens": 0}
}`

// systemoneAnswer is the answer of the systemone protocol.
const systemoneAnswer = `{
  "model": "jev-1.14",
  "answers": {
    "next": {"type":"choice","choice":"/gtw commit","probabilities":{"/gtw commit":0.9,"/review":0.1},"confidence":0.9},
    "needs_human": {"type":"noul","probability":0.05},
    "confidence": {"type":"score","probabilities":{"low":0.2,"high":0.8}}
  },
  "usage": {"input_tokens": 120, "output_tokens": 0}
}`

// assertAnswers checks the answers both protocols have to produce.
func assertAnswers(t *testing.T, resp *llm.DecisionResponse, model string) {
	t.Helper()
	if resp.Model != model {
		t.Fatalf("model = %q, want %q", resp.Model, model)
	}
	next, ok := resp.Answers["next"]
	if !ok {
		t.Fatalf("the answer has no next: %v", resp.Answers)
	}
	if next.Choice != "/gtw commit" {
		t.Fatalf("next = %q, want /gtw commit", next.Choice)
	}
	if next.Confidence != 0.9 {
		t.Fatalf("confidence = %v, want 0.9", next.Confidence)
	}
	human, ok := resp.Answers["needs_human"]
	if !ok {
		t.Fatalf("the answer has no needs_human: %v", resp.Answers)
	}
	if human.Probability != 0.05 {
		t.Fatalf("needs_human = %v, want 0.05", human.Probability)
	}
	score, ok := resp.Answers["confidence"]
	if !ok {
		t.Fatalf("the answer has no confidence: %v", resp.Answers)
	}
	// The levels are weighted: 0.2 * low + 0.8 * high = 0.8.
	if score.Score != 0.8 {
		t.Fatalf("score = %v, want 0.8", score.Score)
	}
	if resp.Usage.InputTokens != 120 {
		t.Fatalf("usage = %+v, want 120 input tokens", resp.Usage)
	}
}

func TestDecisions_TheRequestCarriesEveryQuestionType(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(decisionsAnswer))
	}))
	defer srv.Close()

	d, err := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "jev-1.14"})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	resp, err := d.Decide(context.Background(), neutralRequest())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	assertAnswers(t, resp, "jev-1.14")

	if r := body["input"].(map[string]any); r["role"] != "user" {
		t.Fatalf("the input is %v, want the state as a user message", r)
	}
	content, _ := body["input"].(map[string]any)["content"].([]any)
	if len(content) != 1 {
		t.Fatalf("the input has %d parts, want 1", len(content))
	}
	part, _ := content[0].(map[string]any)
	if part["type"] != "input_text" {
		t.Fatalf("the input part is %v, want input_text", part)
	}
	if !strings.Contains(part["text"].(string), "halfway") {
		t.Fatalf("the input lost the state: %v", part)
	}

	questions, _ := body["questions"].([]any)
	if len(questions) != 3 {
		t.Fatalf("the request carried %d questions, want 3", len(questions))
	}
	first, _ := questions[0].(map[string]any)
	if first["type"] != "choice" {
		t.Fatalf("the first question is %v, want a choice", first)
	}
	choices, _ := first["choices"].([]any)
	if len(choices) != 2 {
		t.Fatalf("the choice question has %d choices, want 2", len(choices))
	}
	second, _ := questions[1].(map[string]any)
	if second["type"] != "predicate" {
		t.Fatalf("the second question is %v, want a predicate", second)
	}
	third, _ := questions[2].(map[string]any)
	if third["type"] != "score" {
		t.Fatalf("the third question is %v, want a score", third)
	}
	levels, _ := third["levels"].([]any)
	if len(levels) != 2 {
		t.Fatalf("the score question has %d levels, want 2", len(levels))
	}
}

func TestSystemOne_TheRequestCarriesEveryQuestionType(t *testing.T) {
	var body map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &body)
		w.Write([]byte(systemoneAnswer))
	}))
	defer srv.Close()

	d, err := NewSystemOne(llm.Config{Protocol: "systemone", BaseURL: srv.URL, Model: "jev-1.14"})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	resp, err := d.Decide(context.Background(), neutralRequest())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	assertAnswers(t, resp, "jev-1.14")

	if !strings.Contains(body["state"].(string), "halfway") {
		t.Fatalf("the request lost the state: %v", body["state"])
	}
	questions, _ := body["questions"].(map[string]any)
	next, _ := questions["next"].(map[string]any)
	if next["type"] != "choice" {
		t.Fatalf("the next question is %v, want a choice", next)
	}
	criteria, _ := next["criteria"].(map[string]any)
	if criteria["/gtw commit"] != "commit the work" {
		t.Fatalf("the criteria are %v, want the option descriptions", criteria)
	}
	human, _ := questions["needs_human"].(map[string]any)
	if human["type"] != "noul" {
		t.Fatalf("the needs_human question is %v, want a noul", human)
	}
	score, _ := questions["confidence"].(map[string]any)
	if score["type"] != "score" {
		t.Fatalf("the confidence question is %v, want a score", score)
	}
	levels, _ := score["criteria"].([]any)
	if len(levels) != 2 {
		t.Fatalf("the score question has %d levels, want 2", len(levels))
	}
}

func TestBothProtocols_AnswerTheSameRequestTheSameWay(t *testing.T) {
	decisionsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(decisionsAnswer))
	}))
	defer decisionsSrv.Close()
	systemoneSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(systemoneAnswer))
	}))
	defer systemoneSrv.Close()

	d1, err := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: decisionsSrv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	d2, err := NewSystemOne(llm.Config{Protocol: "systemone", BaseURL: systemoneSrv.URL, Model: "m"})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	r1, err := d1.Decide(context.Background(), neutralRequest())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	r2, err := d2.Decide(context.Background(), neutralRequest())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	for name, answer := range r1.Answers {
		other, ok := r2.Answers[name]
		if !ok {
			t.Fatalf("the second protocol has no answer for %q", name)
		}
		if answer.Choice != other.Choice || answer.Probability != other.Probability ||
			answer.Confidence != other.Confidence || answer.Score != other.Score {
			t.Fatalf("the answers for %q differ: %+v and %+v", name, answer, other)
		}
	}
}

func TestDecider_HandlesARefusal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"jev","answers":{"next":{"type":"choice","refused":true}}}`))
	}))
	defer srv.Close()

	d, _ := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "m"})
	resp, err := d.Decide(context.Background(), neutralRequest())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !resp.Answers["next"].Refused {
		t.Fatalf("the refusal was lost: %+v", resp.Answers["next"])
	}
}

func TestDecider_ValidatesBeforeItCalls(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		w.Write([]byte(decisionsAnswer))
	}))
	defer srv.Close()

	d, _ := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "m"})
	_, err := d.Decide(context.Background(), llm.DecisionRequest{State: "x"})
	if err == nil {
		t.Fatal("a request without questions was accepted")
	}
	if atomic.LoadInt32(&calls) != 0 {
		t.Fatal("an invalid request reached the network")
	}
}

func TestDecider_RetriesAfterARateLimit(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(http.StatusTooManyRequests)
			w.Write([]byte(`{"message":"slow down"}`))
			return
		}
		w.Write([]byte(decisionsAnswer))
	}))
	defer srv.Close()

	d, _ := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "m"})
	resp, err := d.Decide(context.Background(), neutralRequest())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	assertAnswers(t, resp, "jev-1.14")
}

func TestDecider_ReportsANonRetryableFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":{"message":"invalid key"}}`))
	}))
	defer srv.Close()

	d, _ := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "m"})
	_, err := d.Decide(context.Background(), neutralRequest())
	if err == nil {
		t.Fatal("the call was accepted")
	}
	if llm.Retryable(err) {
		t.Fatal("an authentication failure must not be retryable")
	}
	if !strings.Contains(err.Error(), "invalid key") {
		t.Fatalf("error = %q, want the provider's message", err)
	}
}

func TestDecider_HonoursTheContext(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second)
		w.Write([]byte(decisionsAnswer))
	}))
	defer srv.Close()

	d, _ := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "m"})
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := d.Decide(ctx, neutralRequest())
	if err == nil {
		t.Fatal("the call was not cancelled")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %q, want the deadline", err)
	}
}

func TestNew_RequiresABaseURL(t *testing.T) {
	if _, err := NewDecisions(llm.Config{Protocol: "decisions", Model: "m"}); err == nil {
		t.Fatal("a decider without a base url was accepted")
	}
	if _, err := NewSystemOne(llm.Config{Protocol: "systemone", Model: "m"}); err == nil {
		t.Fatal("a decider without a base url was accepted")
	}
}

// officialDecisionsAnswer is the answer of the OpenAI decisions
// protocol: an array of answers, each naming its question, with the
// probabilities as an array of objects.
const officialDecisionsAnswer = `{
  "model": "gpt-6-luna",
  "answers": [
    {"type":"choice","name":"next","choice":"/gtw commit","confidence":0.9,"probabilities":[{"value":"/gtw commit","probability":0.9},{"value":"/review","probability":0.1}]},
    {"type":"predicate","name":"needs_human","probability":0.05},
    {"type":"score","name":"confidence","score":0.8,"confidence":0.8,"probabilities":[{"label":"low","value":0,"probability":0.2},{"label":"high","value":1,"probability":0.8}]}
  ],
  "usage": {"input_tokens": 120, "output_tokens": 0}
}`

// officialSystemOneAnswer is the answer of the TypeSafe systemone
// protocol: a map keyed by question name, with the probability of a
// noul under the name noul and the levels counted from zero.
const officialSystemOneAnswer = `{
  "model": "jev-1.14.0",
  "answers": {
    "next": {"type":"choice","choice":"/gtw commit","confidence":0.9,"probabilities":{"/gtw commit":0.9,"/review":0.1}},
    "needs_human": {"type":"noul","noul":0.05},
    "confidence": {"type":"score","score":0.8,"confidence":0.8,"legend":{"0":"low","1":"high"},"probabilities":{"0":0.2,"1":0.8}}
  },
  "usage": {"input_tokens": 120, "output_tokens": 0}
}`

func TestDecisions_ReadsTheOfficialAnswerShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(officialDecisionsAnswer))
	}))
	defer srv.Close()

	d, err := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "gpt-6-luna"})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	resp, err := d.Decide(context.Background(), neutralRequest())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	assertAnswers(t, resp, "gpt-6-luna")

	next := resp.Answers["next"]
	if next.Probabilities["/gtw commit"] != 0.9 || next.Probabilities["/review"] != 0.1 {
		t.Fatalf("the probabilities are %v, want the per-option chances", next.Probabilities)
	}
	score := resp.Answers["confidence"]
	if score.Probabilities["low"] != 0.2 || score.Probabilities["high"] != 0.8 {
		t.Fatalf("the probabilities are %v, want them keyed by level", score.Probabilities)
	}
}

func TestSystemOne_ReadsTheOfficialAnswerShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(officialSystemOneAnswer))
	}))
	defer srv.Close()

	d, err := NewSystemOne(llm.Config{Protocol: "systemone", BaseURL: srv.URL, Model: "jev-1.14.0"})
	if err != nil {
		t.Fatalf("new decider: %v", err)
	}
	resp, err := d.Decide(context.Background(), neutralRequest())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	assertAnswers(t, resp, "jev-1.14.0")

	score := resp.Answers["confidence"]
	if score.Probabilities["low"] != 0.2 || score.Probabilities["high"] != 0.8 {
		t.Fatalf("the probabilities are %v, want them keyed by level", score.Probabilities)
	}
}

func TestDecider_ReadsARefusalOfTheOfficialShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"gpt-6-luna","answers":[{"type":"refusal","name":"next"}]}`))
	}))
	defer srv.Close()

	d, _ := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "m"})
	resp, err := d.Decide(context.Background(), neutralRequest())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if !resp.Answers["next"].Refused {
		t.Fatalf("the refusal was lost: %+v", resp.Answers["next"])
	}
}

func TestDecisions_PairsUnnamedAnswersWithTheQuestions(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"m","answers":[
			{"type":"predicate","probability":0.2},
			{"type":"predicate","probability":0.8}
		]}`))
	}))
	defer srv.Close()

	d, _ := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "m"})
	resp, err := d.Decide(context.Background(), llm.DecisionRequest{
		State: "s",
		Questions: []llm.Question{
			{Name: "first", Type: llm.QuestionYesNo},
			{Name: "second", Type: llm.QuestionYesNo},
		},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	if resp.Answers["first"].Probability != 0.2 {
		t.Fatalf("the first answer is %v, want the first answer of the array", resp.Answers["first"])
	}
	if resp.Answers["second"].Probability != 0.8 {
		t.Fatalf("the second answer is %v, want the second answer of the array", resp.Answers["second"])
	}
}

func TestDecider_RetriesAfterAnOverload(t *testing.T) {
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&calls, 1) == 1 {
			w.WriteHeader(529)
			w.Write([]byte(`{"message":"overloaded"}`))
			return
		}
		w.Write([]byte(decisionsAnswer))
	}))
	defer srv.Close()

	d, _ := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "m"})
	resp, err := d.Decide(context.Background(), neutralRequest())
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	assertAnswers(t, resp, "jev-1.14")
	if atomic.LoadInt32(&calls) != 2 {
		t.Fatalf("the server was asked %d times, want 2", calls)
	}
}

func TestEndpoint_AddsTheVersionOnce(t *testing.T) {
	cases := map[string]string{
		"https://api.openai.com":        "https://api.openai.com/v1/decisions",
		"https://api.openai.com/":       "https://api.openai.com/v1/decisions",
		"https://openrouter.ai/api/v1":  "https://openrouter.ai/api/v1/decisions",
		"https://openrouter.ai/api/v1/": "https://openrouter.ai/api/v1/decisions",
	}
	for base, want := range cases {
		if got := endpoint(base, "/decisions"); got != want {
			t.Fatalf("endpoint(%q) = %q, want %q", base, got, want)
		}
	}
}

func TestDecisions_KeepsLabelKeyedProbabilitiesOverNumericLabels(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"m","answers":{"s":{"type":"score","score":0.5,"probabilities":{"1":0.7,"2":0.2,"3":0.1}}}}`))
	}))
	defer srv.Close()

	d, _ := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "m"})
	resp, err := d.Decide(context.Background(), llm.DecisionRequest{
		State: "s",
		Questions: []llm.Question{{
			Name:   "s",
			Type:   llm.QuestionScore,
			Levels: []llm.Level{{Label: "1"}, {Label: "2"}, {Label: "3"}},
		}},
	})
	if err != nil {
		t.Fatalf("decide: %v", err)
	}
	want := map[string]float64{"1": 0.7, "2": 0.2, "3": 0.1}
	got := resp.Answers["s"].Probabilities
	if len(got) != len(want) {
		t.Fatalf("the probabilities are %v, want %v", got, want)
	}
	for key, p := range want {
		if got[key] != p {
			t.Fatalf("the probabilities are %v, want %v", got, want)
		}
	}
}

func TestDecider_ReportsAnUnknownProbabilityShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"model":"m","answers":{"next":{"type":"choice","choice":"/gtw commit","probabilities":[{"option":"/gtw commit","p":0.9}]}}}`))
	}))
	defer srv.Close()

	d, _ := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "m"})
	_, err := d.Decide(context.Background(), neutralRequest())
	if err == nil {
		t.Fatal("a probability that names no option was accepted")
	}
	if !strings.Contains(err.Error(), "/gtw commit") {
		t.Fatalf("error = %q, want the provider's payload", err)
	}
}

func TestDecider_CallsTheVersionedPath(t *testing.T) {
	var path string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		w.Write([]byte(decisionsAnswer))
	}))
	defer srv.Close()

	d, _ := NewDecisions(llm.Config{Protocol: "decisions", BaseURL: srv.URL, Model: "m"})
	if _, err := d.Decide(context.Background(), neutralRequest()); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if path != "/v1/decisions" {
		t.Fatalf("the server was asked for %q, want /v1/decisions", path)
	}
}
