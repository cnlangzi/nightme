package feishu

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"github.com/cnlangzi/nightme/internal/messages"
)

type patchLog struct {
	mu     sync.Mutex
	id     string
	bodies []string
}

func (p *patchLog) hook() func(context.Context, string, string) error {
	return func(_ context.Context, messageID, content string) error {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.id = messageID
		p.bodies = append(p.bodies, content)
		return nil
	}
}

func (p *patchLog) wait(t *testing.T, n int) (id string, bodies []string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		p.mu.Lock()
		if len(p.bodies) >= n {
			id = p.id
			bodies = append([]string(nil), p.bodies...)
			p.mu.Unlock()
			return id, bodies
		}
		p.mu.Unlock()
		time.Sleep(5 * time.Millisecond)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	t.Fatalf("PATCH count = %d, want %d", len(p.bodies), n)
	return p.id, append([]string(nil), p.bodies...)
}

func decodePicks(t *testing.T, s string) []messages.QuestionPick {
	t.Helper()
	picks, err := messages.DecodeQuestionPicks(s)
	if err != nil {
		t.Fatalf("DecodeQuestionPicks: %v", err)
	}
	if picks == nil {
		t.Fatalf("Option = %q, want nm-q: batch", s)
	}
	return picks
}

func TestHandleCardAction_OptPushesInboundAction(t *testing.T) {
	a := testAdapter(t)

	const (
		wantChatID    = "oc_chat_perm"
		wantMessageID = "om_card_perm"
		wantUserID    = "ou_user_perm"
		wantOption    = "仅 REPL 启动(裸 nightme)"
	)

	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		return wantMessageID, nil
	}
	var patches patchLog
	a.updateFunc = patches.hook()

	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoice,
		ChatID: wantChatID,
		Choice: &messages.Choice{
			Title:     "Action Needed",
			Body:      "question: 何时检查版本? [仅 REPL 启动(裸 nightme) | REPL + 所有 CLI 子命令]",
			Options:   []string{wantOption, "REPL + 所有 CLI 子命令"},
			RequestID: "req-1",
		},
	}); err != nil {
		t.Fatalf("Send(OutChoice): %v", err)
	}

	resp, err := a.handleCardAction(context.Background(), &larkcallback.CardActionTriggerEvent{
		Event: &larkcallback.CardActionTriggerRequest{
			Operator: &larkcallback.Operator{OpenID: wantUserID},
			Action: &larkcallback.CallBackAction{
				Value: map[string]any{
					"action":     "opt:" + wantOption,
					"request_id": "req-1",
				},
			},
			Context: &larkcallback.Context{
				OpenChatID:    wantChatID,
				OpenMessageID: wantMessageID,
			},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction: %v", err)
	}
	if resp == nil || resp.Toast == nil {
		t.Fatal("want toast ack")
	}

	var got messages.InboundMessage
	select {
	case got = <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for inbound Action")
	}
	if got.ChatID != wantChatID {
		t.Errorf("ChatID = %q, want %q", got.ChatID, wantChatID)
	}
	if got.Action == nil {
		t.Fatal("Action is nil")
	}
	if got.Action.Option != wantOption {
		t.Errorf("Option = %q, want %q", got.Action.Option, wantOption)
	}
	if got.Action.RequestID != "req-1" {
		t.Errorf("RequestID = %q, want req-1", got.Action.RequestID)
	}
	if got.Reaction != nil {
		t.Error("Reaction should be nil for opt: clicks")
	}
	if got.Text != "" {
		t.Errorf("Text = %q, want empty", got.Text)
	}

	patchedID, patchedBodies := patches.wait(t, 1)
	patchedBody := patchedBodies[0]
	if patchedID != wantMessageID {
		t.Errorf("PATCH message id = %q, want %q", patchedID, wantMessageID)
	}
	if !strings.Contains(patchedBody, wantOption) {
		t.Errorf("PATCH body missing selected option; got %s", patchedBody)
	}
	if !strings.Contains(patchedBody, "何时检查版本?") {
		t.Errorf("PATCH body dropped original question; got %s", patchedBody)
	}
	if strings.Contains(patchedBody, `"tag":"button"`) || strings.Contains(patchedBody, "opt:") {
		t.Errorf("PATCH should hide option buttons; got %s", patchedBody)
	}
	if !strings.Contains(patchedBody, "👉 Action Needed") {
		t.Errorf("PATCH title = %s, want 👉 Action Needed", patchedBody)
	}
}

func TestBuildInteractiveCard_PermissionButtonsStacked(t *testing.T) {
	raw, err := buildInteractiveCard(&messages.Choice{
		Title:     "Action Needed",
		RequestID: "r1",
		Kind:      messages.ChoiceKindPermission,
		Questions: []messages.ChoiceQuestion{{
			ID:       "q1",
			Header:   "Trigger",
			Question: "何时检查版本?",
			Options: []string{
				"仅 REPL 启动(裸 nightme)",
				"REPL + 所有 CLI 子命令",
				"你指定别的",
			},
		}},
	})
	if err != nil {
		t.Fatalf("buildInteractiveCard: %v", err)
	}
	sets := stackedColumnSets(t, raw)
	if len(sets) != 3 {
		t.Fatalf("stacked rows = %d, want 3 (one option per row)", len(sets))
	}
	for i, set := range sets {
		cols, _ := set["columns"].([]any)
		if len(cols) != 1 {
			t.Errorf("row %d columns = %d, want 1 (not equal-width)", i, len(cols))
		}
		if set["flex_mode"] == "bisect" {
			t.Errorf("row %d must not use equal-width bisect", i)
		}
	}
	if !strings.Contains(raw, `"width":"fill"`) {
		t.Error("stacked buttons should fill the row")
	}
	if !strings.Contains(raw, "Type your answer") {
		t.Error("question card should include Type your answer input")
	}
	if !strings.Contains(raw, "Skip this question") {
		t.Error("question card should include Skip this question")
	}
	if !strings.Contains(raw, `"form_action_type":"submit"`) {
		t.Error("question card should include Submit for custom text")
	}
	if strings.Contains(raw, `"icon"`) || strings.Contains(raw, "edit_outlined") {
		t.Error("input must not set icon; Feishu rejects it (200621 unknown property)")
	}
	if !strings.Contains(raw, `"name":"question_form`) {
		t.Error("question options+input must live in one form so option clicks callback")
	}
	if !strings.Contains(raw, `"name":"opt_0"`) {
		t.Error("option buttons inside a form need a unique name")
	}
}

func TestBuildInteractiveCard_QuestionWizardTitle(t *testing.T) {
	raw, err := buildInteractiveCard(&messages.Choice{
		Title:     "Action Needed",
		RequestID: "r1",
		Kind:      messages.ChoiceKindPermission,
		Questions: []messages.ChoiceQuestion{
			{ID: "q1", Header: "Trigger", Question: "何时检查?", Options: []string{"A", "B"}},
			{ID: "q2", Header: "Source", Question: "怎么查?", Options: []string{"C"}},
		},
		Picks: make([]string, 2),
	})
	if err != nil {
		t.Fatalf("buildInteractiveCard: %v", err)
	}
	if !strings.Contains(raw, "👉 Action Needed · 1/2") {
		t.Errorf("title missing 1/2 pager; got %s", raw)
	}
	if !strings.Contains(raw, "何时检查?") {
		t.Errorf("body missing current question; got %s", raw)
	}
	if strings.Contains(raw, "怎么查?") {
		t.Errorf("body leaked next question; got %s", raw)
	}
	if !strings.Contains(raw, "Skip this question") {
		t.Error("wizard should show Skip this question")
	}
	if !strings.Contains(raw, "Type your answer") {
		t.Error("wizard should show Type your answer")
	}
}

func TestHandleCardAction_QuestionWizardBatchesOnLastClick(t *testing.T) {
	a := testAdapter(t)

	const (
		wantChatID    = "oc_chat_wiz"
		wantMessageID = "om_card_wiz"
		wantUserID    = "ou_user_wiz"
		opt1          = "仅 REPL 启动(裸 nightme)"
		opt2          = "GitHub Releases API"
	)

	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		return wantMessageID, nil
	}
	var patches patchLog
	a.updateFunc = patches.hook()

	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoice,
		ChatID: wantChatID,
		Choice: &messages.Choice{
			Title:     "Action Needed",
			RequestID: "req-wiz",
			Questions: []messages.ChoiceQuestion{
				{
					ID:       "q-trigger",
					Header:   "Trigger",
					Question: "何时检查版本?",
					Options:  []string{opt1, "REPL + 所有 CLI 子命令"},
				},
				{
					ID:       "q-source",
					Header:   "Source",
					Question: "怎么查?",
					Options:  []string{opt2, "go-github-selfupdate"},
				},
			},
			Picks: make([]string, 2),
		},
	}); err != nil {
		t.Fatalf("Send(OutChoice): %v", err)
	}

	click := func(option string) *larkcallback.CardActionTriggerResponse {
		t.Helper()
		resp, err := a.handleCardAction(context.Background(), &larkcallback.CardActionTriggerEvent{
			Event: &larkcallback.CardActionTriggerRequest{
				Operator: &larkcallback.Operator{OpenID: wantUserID},
				Action: &larkcallback.CallBackAction{
					Value: map[string]any{
						"action":     "opt:" + option,
						"request_id": "req-wiz",
					},
				},
				Context: &larkcallback.Context{
					OpenChatID:    wantChatID,
					OpenMessageID: wantMessageID,
				},
			},
		})
		if err != nil {
			t.Fatalf("handleCardAction(%s): %v", option, err)
		}
		return resp
	}

	if resp := click(opt1); resp == nil || resp.Toast == nil {
		t.Fatal("want toast on first wizard click")
	} else {
		got := callbackCardJSON(t, resp)
		if !strings.Contains(got, "👉 Action Needed · 2/2") {
			t.Errorf("callback card missing 2/2; got %s", got)
		}
		if !strings.Contains(got, `"name":"question_form_1"`) {
			t.Errorf("step-2 form must use a new name so Feishu re-enables it; got %s", got)
		}
	}
	select {
	case <-a.Incoming():
		t.Fatal("first wizard click must not inbound /api/respond")
	case <-time.After(50 * time.Millisecond):
	}
	_, patched := patches.wait(t, 1)
	if !strings.Contains(patched[0], "👉 Action Needed · 2/2") {
		t.Errorf("step-2 title missing; got %s", patched[0])
	}
	if !strings.Contains(patched[0], "✓ **Trigger**："+opt1) {
		t.Errorf("step-2 missing first pick summary; got %s", patched[0])
	}
	if !strings.Contains(patched[0], "怎么查?") {
		t.Errorf("step-2 missing second question; got %s", patched[0])
	}

	if resp := click(opt2); resp == nil || resp.Toast == nil {
		t.Fatal("want toast on last wizard click")
	} else if settled := callbackCardJSON(t, resp); strings.Contains(settled, `"tag":"form"`) {
		t.Errorf("last wizard click should settle the card; got %s", settled)
	}
	var got messages.InboundMessage
	select {
	case got = <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for batched inbound")
	}
	if got.Action == nil {
		t.Fatal("Action is nil on last click")
	}
	picks := decodePicks(t, got.Action.Option)
	if len(picks) != 2 || picks[0].ID != "q-trigger" || picks[1].ID != "q-source" {
		t.Errorf("picks = %+v", picks)
	}
	if len(picks[0].Selected) != 1 || picks[0].Selected[0] != opt1 {
		t.Errorf("pick[0] = %+v", picks[0])
	}
	if len(picks[1].Selected) != 1 || picks[1].Selected[0] != opt2 {
		t.Errorf("pick[1] = %+v", picks[1])
	}
	_, patched = patches.wait(t, 2)
	if strings.Contains(patched[1], `"tag":"button"`) || strings.Contains(patched[1], "opt:") {
		t.Errorf("final PATCH should hide buttons; got %s", patched[1])
	}
	if !strings.Contains(patched[1], "✓ **Trigger**："+opt1) || !strings.Contains(patched[1], "✓ **Source**："+opt2) {
		t.Errorf("final PATCH missing both summaries; got %s", patched[1])
	}
	if strings.Contains(patched[1], " · 2/2") {
		t.Errorf("completed card should drop pager; got %s", patched[1])
	}
}

func TestHandleCardAction_QuestionWizardSkipThenAnswer(t *testing.T) {
	a := testAdapter(t)
	const wantMessageID = "om_card_skip"
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		return wantMessageID, nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoice,
		ChatID: "oc_skip",
		Choice: &messages.Choice{
			Title:     "Action Needed",
			RequestID: "req-skip",
			Questions: []messages.ChoiceQuestion{
				{ID: "q1", Header: "Q1", Question: "one", Options: []string{"A"}},
				{ID: "q2", Header: "Q2", Question: "two", Options: []string{"B"}},
			},
			Picks: make([]string, 2),
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	fire := func(action string) {
		t.Helper()
		if _, err := a.handleCardAction(context.Background(), &larkcallback.CardActionTriggerEvent{
			Event: &larkcallback.CardActionTriggerRequest{
				Operator: &larkcallback.Operator{OpenID: "ou"},
				Action:   &larkcallback.CallBackAction{Value: map[string]any{"action": action, "request_id": "req-skip"}},
				Context:  &larkcallback.Context{OpenChatID: "oc_skip", OpenMessageID: wantMessageID},
			},
		}); err != nil {
			t.Fatalf("handleCardAction(%s): %v", action, err)
		}
	}

	fire("skip:")
	select {
	case <-a.Incoming():
		t.Fatal("skip of first question must not inbound")
	case <-time.After(50 * time.Millisecond):
	}
	fire("opt:B")
	var got messages.InboundMessage
	select {
	case got = <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for inbound after last pick")
	}
	if got.Action == nil {
		t.Fatal("Action is nil after last pick")
	}
	picks := decodePicks(t, got.Action.Option)
	if len(picks[0].Selected) != 0 {
		t.Errorf("skipped q1 selected = %v, want empty", picks[0].Selected)
	}
	if len(picks[1].Selected) != 1 || picks[1].Selected[0] != "B" {
		t.Errorf("q2 = %+v", picks[1])
	}
}

func stackedColumnSets(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var env map[string]any
	if err := json.Unmarshal([]byte(raw), &env); err != nil {
		t.Fatalf("decode card: %v", err)
	}
	body, _ := env["body"].(map[string]any)
	elements, _ := body["elements"].([]any)
	var sets []map[string]any
	var walk func([]any)
	walk = func(els []any) {
		for _, el := range els {
			m, ok := el.(map[string]any)
			if !ok {
				continue
			}
			switch m["tag"] {
			case "form":
				inner, _ := m["elements"].([]any)
				walk(inner)
			case "column_set":
				if m["flex_mode"] == "bisect" {
					continue
				}
				sets = append(sets, m)
			}
		}
	}
	walk(elements)
	return sets
}

func TestSend_OutChoicePatch_EmptyReplyToSettlesLastCard(t *testing.T) {
	a := testAdapter(t)
	const (
		chatID = "oc_settle"
		msgID  = "om_settle"
	)
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		return msgID, nil
	}
	var patches patchLog
	a.updateFunc = patches.hook()
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoice,
		ChatID: chatID,
		Choice: &messages.Choice{
			Title:     "Waiting for approval",
			Body:      "Bash: escalate sandbox",
			Options:   []string{"Allow once", "Reject"},
			RequestID: "req-settle",
		},
	}); err != nil {
		t.Fatalf("Send OutChoice: %v", err)
	}
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoicePatch,
		ChatID: chatID,
		Choice: &messages.Choice{
			Title: "Waiting for approval",
			Body:  "✓ **allowed-once**（dashboard）",
			Kind:  messages.ChoiceKindPermission,
		},
	}); err != nil {
		t.Fatalf("Send OutChoicePatch: %v", err)
	}
	_, patchedBodies := patches.wait(t, 1)
	patched := patchedBodies[0]
	if strings.Contains(patched, `"tag":"button"`) {
		t.Errorf("settled PATCH should hide buttons; got %s", patched)
	}
	if !strings.Contains(patched, "allowed-once") {
		t.Errorf("settled PATCH missing outcome; got %s", patched)
	}
}

func TestSend_OutChoicePatch_ByRequestID_NotLastCard(t *testing.T) {
	a := testAdapter(t)
	const chatID = "oc_two"
	ids := []string{"om_first", "om_second"}
	n := 0
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		id := ids[n]
		n++
		return id, nil
	}
	var patches patchLog
	a.updateFunc = patches.hook()
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoice,
		ChatID: chatID,
		Choice: &messages.Choice{
			Title:     "first",
			Body:      "card one",
			Options:   []string{"A"},
			RequestID: "req-first",
		},
	}); err != nil {
		t.Fatalf("Send first OutChoice: %v", err)
	}
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoice,
		ChatID: chatID,
		Choice: &messages.Choice{
			Title:     "second",
			Body:      "card two",
			Options:   []string{"B"},
			RequestID: "req-second",
		},
	}); err != nil {
		t.Fatalf("Send second OutChoice: %v", err)
	}
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoicePatch,
		ChatID: chatID,
		Choice: &messages.Choice{
			Title:     "first",
			Body:      "patched first",
			RequestID: "req-first",
			Disabled:  true,
		},
	}); err != nil {
		t.Fatalf("Send OutChoicePatch: %v", err)
	}
	patchedID, patchedBodies := patches.wait(t, 1)
	if patchedID != "om_first" {
		t.Errorf("PATCH target = %q, want om_first (RequestID, not last card)", patchedID)
	}
	if !strings.Contains(patchedBodies[0], "patched first") {
		t.Errorf("PATCH body missing outcome; got %s", patchedBodies[0])
	}
}

func TestBuildInteractiveCard_ApprovalHasNoCustomInput(t *testing.T) {
	raw, err := buildInteractiveCard(&messages.Choice{
		Title:     "Waiting for approval",
		Body:      "Bash: escalate sandbox",
		Options:   []string{"Allow once", "Reject"},
		RequestID: "r-appr",
		Kind:      messages.ChoiceKindPermission,
	})
	if err != nil {
		t.Fatalf("buildInteractiveCard: %v", err)
	}
	if strings.Contains(raw, "Type your answer") {
		t.Error("approval card must not include Type your answer")
	}
	if strings.Contains(raw, "Skip this question") {
		t.Error("approval card must not include Skip this question")
	}
	if strings.Contains(raw, `"tag":"form"`) {
		t.Error("approval card must not include a form")
	}
}

func TestHandleCardAction_OneShotSkip(t *testing.T) {
	a := testAdapter(t)
	const wantMessageID = "om_card_oneshot_skip"
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		return wantMessageID, nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoice,
		ChatID: "oc_oneshot_skip",
		Choice: &messages.Choice{
			Title:     "Action Needed",
			RequestID: "req-oneshot-skip",
			Questions: []messages.ChoiceQuestion{
				{ID: "q1", Header: "Q1", Question: "which?", Options: []string{"A", "B"}},
			},
			Picks: make([]string, 1),
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if _, err := a.handleCardAction(context.Background(), &larkcallback.CardActionTriggerEvent{
		Event: &larkcallback.CardActionTriggerRequest{
			Operator: &larkcallback.Operator{OpenID: "ou"},
			Action:   &larkcallback.CallBackAction{Value: map[string]any{"action": "skip:", "request_id": "req-oneshot-skip"}},
			Context:  &larkcallback.Context{OpenChatID: "oc_oneshot_skip", OpenMessageID: wantMessageID},
		},
	}); err != nil {
		t.Fatalf("handleCardAction: %v", err)
	}

	var got messages.InboundMessage
	select {
	case got = <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for skip inbound")
	}
	if got.Action == nil {
		t.Fatal("Action is nil")
	}
	picks := decodePicks(t, got.Action.Option)
	if len(picks) != 1 || picks[0].ID != "q1" || len(picks[0].Selected) != 0 || picks[0].Custom != "" {
		t.Errorf("skip pick = %+v, want empty selected and custom", picks)
	}
}

func TestHandleCardAction_OneShotCustomSubmit(t *testing.T) {
	a := testAdapter(t)
	const wantMessageID = "om_card_oneshot_custom"
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		return wantMessageID, nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoice,
		ChatID: "oc_oneshot_custom",
		Choice: &messages.Choice{
			Title:     "Action Needed",
			RequestID: "req-oneshot-custom",
			Questions: []messages.ChoiceQuestion{
				{ID: "q1", Question: "which?", Options: []string{"A", "B"}},
			},
			Picks: make([]string, 1),
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	const typed = "都不选，daemon 本轮不做"
	resp, err := a.handleCardAction(context.Background(), &larkcallback.CardActionTriggerEvent{
		Event: &larkcallback.CardActionTriggerRequest{
			Operator: &larkcallback.Operator{OpenID: "ou"},
			Action: &larkcallback.CallBackAction{
				Value:     map[string]any{"action": "custom:", "request_id": "req-oneshot-custom"},
				FormValue: map[string]any{"custom": typed},
				Name:      "submit_custom",
			},
			Context: &larkcallback.Context{OpenChatID: "oc_oneshot_custom", OpenMessageID: wantMessageID},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction: %v", err)
	}
	if resp == nil || resp.Toast == nil {
		t.Fatal("want toast on custom submit")
	}
	if settled := callbackCardJSON(t, resp); strings.Contains(settled, `"tag":"form"`) {
		t.Errorf("one-shot submit should settle the card; got %s", settled)
	}

	var got messages.InboundMessage
	select {
	case got = <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for custom inbound")
	}
	if got.Action == nil {
		t.Fatal("Action is nil")
	}
	picks := decodePicks(t, got.Action.Option)
	if len(picks) != 1 || picks[0].Custom != typed || len(picks[0].Selected) != 0 {
		t.Errorf("custom pick = %+v, want custom=%q empty selected", picks, typed)
	}
}

func TestHandleCardAction_CustomSubmitEmptyToasts(t *testing.T) {
	a := testAdapter(t)
	const wantMessageID = "om_card_empty_custom"
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		return wantMessageID, nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoice,
		ChatID: "oc_empty_custom",
		Choice: &messages.Choice{
			Title:     "Action Needed",
			RequestID: "req-empty-custom",
			Questions: []messages.ChoiceQuestion{
				{ID: "q1", Question: "which?", Options: []string{"A"}},
			},
			Picks: make([]string, 1),
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	resp, err := a.handleCardAction(context.Background(), &larkcallback.CardActionTriggerEvent{
		Event: &larkcallback.CardActionTriggerRequest{
			Operator: &larkcallback.Operator{OpenID: "ou"},
			Action: &larkcallback.CallBackAction{
				Value:     map[string]any{"action": "custom:", "request_id": "req-empty-custom"},
				FormValue: map[string]any{"custom": "  "},
				Name:      "submit_custom",
			},
			Context: &larkcallback.Context{OpenChatID: "oc_empty_custom", OpenMessageID: wantMessageID},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction: %v", err)
	}
	if resp == nil || resp.Toast == nil || resp.Toast.Type != "warning" {
		t.Fatalf("want warning toast for empty custom, got %+v", resp)
	}
	select {
	case <-a.Incoming():
		t.Fatal("empty custom must not inbound")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestHandleCardAction_QuestionWizardCustomThenSkip(t *testing.T) {
	a := testAdapter(t)
	const wantMessageID = "om_card_custom_skip"
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		return wantMessageID, nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoice,
		ChatID: "oc_custom_skip",
		Choice: &messages.Choice{
			Title:     "Action Needed",
			RequestID: "req-custom-skip",
			Questions: []messages.ChoiceQuestion{
				{ID: "q1", Header: "Q1", Question: "one", Options: []string{"A"}},
				{ID: "q2", Header: "Q2", Question: "two", Options: []string{"B"}},
			},
			Picks: make([]string, 2),
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	resp, err := a.handleCardAction(context.Background(), &larkcallback.CardActionTriggerEvent{
		Event: &larkcallback.CardActionTriggerRequest{
			Operator: &larkcallback.Operator{OpenID: "ou"},
			Action: &larkcallback.CallBackAction{
				Value:     map[string]any{"action": "custom:", "request_id": "req-custom-skip"},
				FormValue: map[string]any{"custom": "free text"},
			},
			Context: &larkcallback.Context{OpenChatID: "oc_custom_skip", OpenMessageID: wantMessageID},
		},
	})
	if err != nil {
		t.Fatalf("custom: %v", err)
	}
	gotCard := callbackCardJSON(t, resp)
	if !strings.Contains(gotCard, "👉 Action Needed · 2/2") {
		t.Errorf("Submit must return 2/2 in the callback card; got %s", gotCard)
	}
	if !strings.Contains(gotCard, `"name":"question_form_1"`) {
		t.Errorf("step-2 form must use a new name; got %s", gotCard)
	}
	select {
	case <-a.Incoming():
		t.Fatal("custom on first question must not inbound")
	case <-time.After(50 * time.Millisecond):
	}

	if _, err := a.handleCardAction(context.Background(), &larkcallback.CardActionTriggerEvent{
		Event: &larkcallback.CardActionTriggerRequest{
			Operator: &larkcallback.Operator{OpenID: "ou"},
			Action:   &larkcallback.CallBackAction{Value: map[string]any{"action": "skip:", "request_id": "req-custom-skip"}},
			Context:  &larkcallback.Context{OpenChatID: "oc_custom_skip", OpenMessageID: wantMessageID},
		},
	}); err != nil {
		t.Fatalf("skip: %v", err)
	}

	var got messages.InboundMessage
	select {
	case got = <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for inbound after last skip")
	}
	if got.Action == nil {
		t.Fatal("Action is nil after last skip")
	}
	picks := decodePicks(t, got.Action.Option)
	if picks[0].Custom != "free text" || len(picks[0].Selected) != 0 {
		t.Errorf("q1 = %+v, want custom", picks[0])
	}
	if picks[1].Custom != "" || len(picks[1].Selected) != 0 {
		t.Errorf("q2 = %+v, want skip", picks[1])
	}
}

func TestHandleCardAction_FormSubmitOptIndexName(t *testing.T) {
	a := testAdapter(t)
	const (
		wantChatID    = "oc_form_opt"
		wantMessageID = "om_form_opt"
		wantOption    = "你手动操作 (你自己 commit)"
	)
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		return wantMessageID, nil
	}
	a.updateFunc = func(_ context.Context, _, _ string) error { return nil }

	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutChoice,
		ChatID: wantChatID,
		Choice: &messages.Choice{
			Title:     "Action Needed",
			RequestID: "req-form-opt",
			Questions: []messages.ChoiceQuestion{{
				ID:       "q-commit",
				Question: "怎么办?",
				Options: []string{
					wantOption,
					"接受 endpoint 改名 commit message",
					"放手,改用顶层 fixup commit",
				},
			}},
			Picks: make([]string, 1),
		},
	}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	resp, err := a.handleCardAction(context.Background(), &larkcallback.CardActionTriggerEvent{
		Event: &larkcallback.CardActionTriggerRequest{
			Operator: &larkcallback.Operator{OpenID: "ou"},
			Action: &larkcallback.CallBackAction{
				Name: "opt_0",
			},
			Context: &larkcallback.Context{
				OpenChatID:    wantChatID,
				OpenMessageID: wantMessageID,
			},
		},
	})
	if err != nil {
		t.Fatalf("handleCardAction: %v", err)
	}
	if resp == nil || resp.Toast == nil {
		t.Fatal("want toast")
	}
	if strings.Contains(resp.Toast.Content, "Recorded:") {
		t.Fatalf("form submit opt_0 must not fall through to Recorded toast; got %q", resp.Toast.Content)
	}

	var got messages.InboundMessage
	select {
	case got = <-a.Incoming():
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for inbound")
	}
	if got.Action == nil {
		t.Fatal("Action is nil")
	}
	if got.Action.Option != wantOption {
		t.Errorf("Option = %q, want %q", got.Action.Option, wantOption)
	}
}

func TestResolveCardAction_FormNames(t *testing.T) {
	card := &messages.Choice{
		Questions: []messages.ChoiceQuestion{{
			Options: []string{"A", "B"},
		}},
	}
	got := resolveCardAction(&larkcallback.CardActionTriggerRequest{
		Action: &larkcallback.CallBackAction{Name: "opt_1"},
	}, card)
	if got != "opt:B" {
		t.Errorf("opt_1 = %q, want opt:B", got)
	}
	got = resolveCardAction(&larkcallback.CardActionTriggerRequest{
		Action: &larkcallback.CallBackAction{Name: "skip_question"},
	}, card)
	if got != "skip:" {
		t.Errorf("skip_question = %q, want skip:", got)
	}
	got = resolveCardAction(&larkcallback.CardActionTriggerRequest{
		Action: &larkcallback.CallBackAction{
			Name:  "opt_0",
			Value: map[string]any{"action": "opt:override"},
		},
	}, card)
	if got != "opt:override" {
		t.Errorf("value.action should win over name; got %q", got)
	}
}

func callbackCardJSON(t *testing.T, resp *larkcallback.CardActionTriggerResponse) string {
	t.Helper()
	if resp == nil || resp.Card == nil {
		t.Fatal("want card in card.action.trigger response so Feishu redraws the form")
	}
	if resp.Card.Type != "raw" {
		t.Fatalf("card.type = %q, want raw", resp.Card.Type)
	}
	b, err := json.Marshal(resp.Card.Data)
	if err != nil {
		t.Fatalf("marshal callback card: %v", err)
	}
	return string(b)
}

func TestRememberOptCard_KeepsPreviousForStaleClick(t *testing.T) {
	a := testAdapter(t)
	oldCard := &messages.Choice{Title: "old", RequestID: "r1", Questions: []messages.ChoiceQuestion{{ID: "q1", Question: "a"}}}
	newCard := &messages.Choice{Title: "new", RequestID: "r2", Questions: []messages.ChoiceQuestion{{ID: "q2", Question: "b"}}}
	a.rememberOptCard("oc_1", "om_old", oldCard)
	a.rememberOptCard("oc_1", "om_new", newCard)
	if got := a.getOptCard("om_old"); got == nil || got.Title != "old" {
		t.Fatal("previous Action Needed card must stay so a late click still pages")
	}
	if got := a.getOptCard("om_new"); got == nil || got.Title != "new" {
		t.Fatalf("new card = %+v", got)
	}
	if a.lastOptMsgID("oc_1") != "om_new" {
		t.Fatalf("lastOptMsgID = %q, want om_new", a.lastOptMsgID("oc_1"))
	}
}

func TestPatchCardForOtherClients_LatestWins(t *testing.T) {
	a := testAdapter(t)
	started := make(chan struct{})
	release := make(chan struct{})
	var patches patchLog
	var first bool
	a.updateFunc = func(_ context.Context, messageID, content string) error {
		patches.mu.Lock()
		isFirst := !first
		if isFirst {
			first = true
		}
		patches.mu.Unlock()
		if isFirst {
			close(started)
			<-release
		}
		return patches.hook()(context.Background(), messageID, content)
	}

	a.patchCardForOtherClients("om_seq", `{"step":"1"}`)
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first PATCH did not start")
	}
	a.patchCardForOtherClients("om_seq", `{"step":"2"}`)
	close(release)
	_, bodies := patches.wait(t, 2)
	if bodies[len(bodies)-1] != `{"step":"2"}` {
		t.Fatalf("last PATCH = %q, want step 2", bodies[len(bodies)-1])
	}
}
