package dsh

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/bridge/dsh/host"
	"github.com/cnlangzi/nightme/internal/messages"
)

func TestQuestionAnswerFor_MatchingLabel(t *testing.T) {
	qs := []questionPayload{
		{
			ID: "q-trigger",
			Options: []AskUserQuestionOption{
				{Label: "仅 REPL 启动(裸 nightme)"},
				{Label: "REPL + 所有 CLI 子命令"},
			},
		},
		{
			ID: "q-source",
			Options: []AskUserQuestionOption{
				{Label: "GitHub Releases API"},
				{Label: "go-github-selfupdate"},
			},
		},
	}
	got, err := questionAnswerFor(qs, "仅 REPL 启动(裸 nightme)")
	if err != nil {
		t.Fatalf("questionAnswerFor: %v", err)
	}
	if len(got.Answers) != 2 {
		t.Fatalf("answers = %d, want 2", len(got.Answers))
	}
	if got.Answers[0].ID != "q-trigger" || len(got.Answers[0].Selected) != 1 || got.Answers[0].Selected[0] != "仅 REPL 启动(裸 nightme)" {
		t.Errorf("answer[0] = %+v, want selected first option", got.Answers[0])
	}
	if got.Answers[0].Custom != "" {
		t.Errorf("answer[0].Custom = %q, want empty", got.Answers[0].Custom)
	}
	if got.Answers[1].ID != "q-source" || len(got.Answers[1].Selected) != 0 {
		t.Errorf("answer[1] = %+v, want empty selected", got.Answers[1])
	}
}

func TestQuestionAnswerFor_CustomFallback(t *testing.T) {
	qs := []questionPayload{
		{ID: "q1", Options: []AskUserQuestionOption{{Label: "A"}, {Label: "B"}}},
		{ID: "q2", Options: []AskUserQuestionOption{{Label: "C"}}},
	}
	got, err := questionAnswerFor(qs, "进启动REPL时")
	if err != nil {
		t.Fatalf("questionAnswerFor: %v", err)
	}
	if got.Answers[0].Custom != "进启动REPL时" {
		t.Errorf("custom = %q, want typed text", got.Answers[0].Custom)
	}
	if len(got.Answers[0].Selected) != 0 {
		t.Errorf("selected = %v, want empty when using custom", got.Answers[0].Selected)
	}
	if got.Answers[1].Custom != "" || len(got.Answers[1].Selected) != 0 {
		t.Errorf("answer[1] = %+v, want empty", got.Answers[1])
	}
}

func TestQuestionAnswerFor_CorruptBatch(t *testing.T) {
	qs := []questionPayload{{ID: "q1", Options: []AskUserQuestionOption{{Label: "A"}}}}
	_, err := questionAnswerFor(qs, messages.QuestionBatchPrefix+"{")
	if err == nil {
		t.Fatal("want error for prefix plus invalid JSON")
	}
}

func TestQuestionAnswerFor_MissingID(t *testing.T) {
	_, err := questionAnswerFor([]questionPayload{{Question: "no id"}}, "x")
	if err == nil {
		t.Fatal("want error for missing question id")
	}
}

func TestQuestionAnswerFor_BatchPicks(t *testing.T) {
	qs := []questionPayload{
		{ID: "q-trigger", Options: []AskUserQuestionOption{{Label: "仅 REPL 启动(裸 nightme)"}}},
		{ID: "q-source", Options: []AskUserQuestionOption{{Label: "GitHub Releases API"}}},
	}
	payload := messages.EncodeQuestionPicks([]messages.QuestionPick{
		{ID: "q-trigger", Selected: []string{"仅 REPL 启动(裸 nightme)"}},
		{ID: "q-source", Selected: []string{"GitHub Releases API"}},
	})
	got, err := questionAnswerFor(qs, payload)
	if err != nil {
		t.Fatalf("questionAnswerFor: %v", err)
	}
	if len(got.Answers) != 2 {
		t.Fatalf("answers = %d, want 2", len(got.Answers))
	}
	if got.Answers[0].ID != "q-trigger" || len(got.Answers[0].Selected) != 1 || got.Answers[0].Selected[0] != "仅 REPL 启动(裸 nightme)" {
		t.Errorf("answer[0] = %+v", got.Answers[0])
	}
	if got.Answers[1].ID != "q-source" || len(got.Answers[1].Selected) != 1 || got.Answers[1].Selected[0] != "GitHub Releases API" {
		t.Errorf("answer[1] = %+v", got.Answers[1])
	}
}

func TestQuestionAnswerFor_BatchCustom(t *testing.T) {
	qs := []questionPayload{
		{ID: "q1", Options: []AskUserQuestionOption{{Label: "A"}, {Label: "B"}}},
		{ID: "q2", Options: []AskUserQuestionOption{{Label: "C"}}},
	}
	payload := messages.EncodeQuestionPicks([]messages.QuestionPick{
		{ID: "q1", Selected: []string{}, Custom: "都不选，改用 daemon"},
		{ID: "q2", Selected: []string{}},
	})
	got, err := questionAnswerFor(qs, payload)
	if err != nil {
		t.Fatalf("questionAnswerFor: %v", err)
	}
	if got.Answers[0].Custom != "都不选，改用 daemon" || len(got.Answers[0].Selected) != 0 {
		t.Errorf("answer[0] = %+v, want custom with empty selected", got.Answers[0])
	}
	if got.Answers[1].Custom != "" || len(got.Answers[1].Selected) != 0 {
		t.Errorf("answer[1] = %+v, want skip", got.Answers[1])
	}
}

func TestQuestionAnswerFor_BatchSkip(t *testing.T) {
	qs := []questionPayload{
		{ID: "q1", Options: []AskUserQuestionOption{{Label: "A"}}},
		{ID: "q2", Options: []AskUserQuestionOption{{Label: "B"}}},
	}
	payload := messages.EncodeQuestionPicks([]messages.QuestionPick{
		{ID: "q1", Selected: []string{}},
		{ID: "q2", Selected: []string{"B"}},
	})
	got, err := questionAnswerFor(qs, payload)
	if err != nil {
		t.Fatalf("questionAnswerFor: %v", err)
	}
	if len(got.Answers[0].Selected) != 0 {
		t.Errorf("answer[0].Selected = %v, want skip", got.Answers[0].Selected)
	}
	if len(got.Answers[1].Selected) != 1 || got.Answers[1].Selected[0] != "B" {
		t.Errorf("answer[1] = %+v", got.Answers[1])
	}
}

func TestSendPermission_QuestionUsesQuestionResponsePayload(t *testing.T) {
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")
	d.sessionID = "session-6292d9e4-5039-40ee-9a53-a408bddb1773"
	t.Cleanup(func() { close(d.closed) })

	d.handleQuestionRequested("rpc-q-1", muxQuestionRequested{
		SessionID: d.sessionID,
		Questions: []questionPayload{
			{
				ID:       "q-trigger",
				Header:   "Trigger",
				Question: "何时检查版本?",
				Options: []AskUserQuestionOption{
					{Label: "仅 REPL 启动(裸 nightme)"},
					{Label: "REPL + 所有 CLI 子命令"},
				},
			},
			{
				ID:       "q-source",
				Header:   "Source",
				Question: "怎么查?",
				Options:  []AskUserQuestionOption{{Label: "GitHub Releases API"}},
			},
		},
	})

	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentPermission {
			t.Fatalf("kind = %v, want EventAgentPermission", ev.Kind)
		}
		if len(ev.Permission.Options) != 2 {
			t.Errorf("options = %v, want first question labels only", ev.Permission.Options)
		}
		if len(ev.Permission.Questions) != 2 {
			t.Errorf("questions = %d, want 2", len(ev.Permission.Questions))
		}
		if ev.Permission.Questions[0].ID != "q-trigger" || ev.Permission.Questions[1].ID != "q-source" {
			t.Errorf("question ids = %q / %q", ev.Permission.Questions[0].ID, ev.Permission.Questions[1].ID)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for permission event")
	}

	if err := d.SendPermission("仅 REPL 启动(裸 nightme)"); err != nil {
		t.Fatalf("SendPermission: %v", err)
	}
	if mock.count.Load() != 1 {
		t.Fatalf("respond calls = %d, want 1", mock.count.Load())
	}

	var env struct {
		Type   string `json:"type"`
		RPCID  string `json:"rpcId"`
		Result struct {
			OK    bool                  `json:"ok"`
			Value host.QuestionResponse `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mock.body(), &env); err != nil {
		t.Fatalf("decode respond: %v body=%s", err, mock.body())
	}
	if env.Type != "client-response" {
		t.Errorf("type = %q, want client-response", env.Type)
	}
	if env.RPCID != "rpc-q-1" {
		t.Errorf("rpcId = %q, want rpc-q-1", env.RPCID)
	}
	if env.Result.Value.SessionID != d.sessionID {
		t.Errorf("sessionId = %q, want %s", env.Result.Value.SessionID, d.sessionID)
	}
	answers := env.Result.Value.Answer.Answers
	if len(answers) != 2 {
		t.Fatalf("answers = %d, want 2", len(answers))
	}
	if answers[0].ID != "q-trigger" || len(answers[0].Selected) != 1 || answers[0].Selected[0] != "仅 REPL 启动(裸 nightme)" {
		t.Errorf("answers[0] = %+v", answers[0])
	}
	if answers[1].ID != "q-source" || answers[1].Selected == nil || len(answers[1].Selected) != 0 {
		t.Errorf("answers[1] = %+v, want empty selected slice", answers[1])
	}
	if answers[0].Custom != "" {
		t.Errorf("answers[0].Custom = %q, want omitted/empty", answers[0].Custom)
	}
}

func TestSendPermission_ApprovalStillUsesOutcome(t *testing.T) {
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")
	d.sessionID = "session-approval"
	t.Cleanup(func() { close(d.closed) })

	d.handleApprovalRequested("rpc-appr-1", muxApprovalRequested{
		SessionID:  d.sessionID,
		ApprovalID: "appr-1",
		ToolName:   "Bash",
		Reason:     "run ls",
	})
	select {
	case <-d.events:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for approval event")
	}

	if err := d.SendPermission("approved"); err != nil {
		t.Fatalf("SendPermission: %v", err)
	}
	var env struct {
		Result struct {
			Value host.ApprovalResponse `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mock.body(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.Result.Value.Outcome != "allowed-once" {
		t.Errorf("outcome = %q, want allowed-once", env.Result.Value.Outcome)
	}
	if env.Result.Value.ApprovalID != "appr-1" {
		t.Errorf("approvalId = %q, want appr-1", env.Result.Value.ApprovalID)
	}
}

func TestSendPermission_AllowOnceLabel(t *testing.T) {
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")
	d.sessionID = "session-allow-once"
	t.Cleanup(func() { close(d.closed) })

	d.handleApprovalRequested("rpc-appr-2", muxApprovalRequested{
		SessionID:  d.sessionID,
		ApprovalID: "appr-2",
		ToolName:   "Bash",
		Reason:     "escalate sandbox",
	})
	select {
	case ev := <-d.events:
		if ev.Permission == nil || ev.Permission.Kind != agent.PermissionKindApproval {
			t.Fatalf("kind = %+v, want approval", ev.Permission)
		}
		if len(ev.Permission.Options) != 2 || ev.Permission.Options[0] != approvalAllowOnce {
			t.Errorf("options = %v, want Allow once / Reject", ev.Permission.Options)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for approval event")
	}
	if err := d.SendPermission(approvalAllowOnce); err != nil {
		t.Fatalf("SendPermission: %v", err)
	}
	var env struct {
		RPCID  string `json:"rpcId"`
		Result struct {
			Value host.ApprovalResponse `json:"value"`
		} `json:"result"`
	}
	if err := json.Unmarshal(mock.body(), &env); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if env.RPCID != "rpc-appr-2" {
		t.Errorf("rpcId = %q, want frame rpcId rpc-appr-2 (not approvalId)", env.RPCID)
	}
	if env.Result.Value.Outcome != "allowed-once" {
		t.Errorf("outcome = %q, want allowed-once", env.Result.Value.Outcome)
	}
}

func TestApprovalResolved_DropsPendingWithoutRespond(t *testing.T) {
	mock := newRespondMock(t)
	cli := mock.installGlobal(t)
	d := newTestDriver(cli, "/tmp/ws")
	d.sessionID = "session-dash"
	t.Cleanup(func() { close(d.closed) })

	d.handleApprovalRequested("rpc-appr-3", muxApprovalRequested{
		SessionID:  d.sessionID,
		ApprovalID: "appr-3",
		ToolName:   "Bash",
		Reason:     "git add",
	})
	select {
	case <-d.events:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for approval event")
	}

	d.handleApprovalResolved(muxApprovalResolved{
		SessionID:  d.sessionID,
		ApprovalID: "appr-3",
		Outcome:    "allowed-once",
	})
	select {
	case ev := <-d.events:
		if ev.Kind != agent.EventAgentPermissionSettled {
			t.Fatalf("kind = %v, want settled", ev.Kind)
		}
		if ev.PermissionSettled == nil || ev.PermissionSettled.Outcome != "allowed-once" {
			t.Errorf("settled = %+v", ev.PermissionSettled)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for settled event")
	}
	if mock.count.Load() != 0 {
		t.Fatalf("respond calls = %d, want 0 (dashboard already answered)", mock.count.Load())
	}
	if err := d.SendPermission(approvalAllowOnce); err == nil {
		t.Fatal("SendPermission after dashboard resolve should fail (no pending)")
	}
}

type respondMock struct {
	server *httptest.Server
	count  atomic.Int64
	raw    atomic.Value
}

func newRespondMock(t *testing.T) *respondMock {
	t.Helper()
	m := &respondMock{}
	mux := http.NewServeMux()
	mux.HandleFunc("/api/respond", func(w http.ResponseWriter, r *http.Request) {
		m.count.Add(1)
		body, _ := io.ReadAll(r.Body)
		_ = r.Body.Close()
		m.raw.Store(append([]byte(nil), body...))
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accepted":true}`))
	})
	m.server = httptest.NewServer(mux)
	t.Cleanup(m.server.Close)
	return m
}

func (m *respondMock) installGlobal(t *testing.T) *host.Client {
	t.Helper()
	cli := host.New(m.server.URL, nil)
	host.UnsetGlobal()
	host.SetGlobal(cli)
	t.Cleanup(func() {
		cli.Close()
		host.UnsetGlobal()
	})
	return cli
}

func (m *respondMock) body() []byte {
	v, _ := m.raw.Load().([]byte)
	return v
}
