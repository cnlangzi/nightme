package feishu

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	larkcallback "github.com/larksuite/oapi-sdk-go/v3/event/dispatcher/callback"

	"github.com/cnlangzi/nightme/internal/messages"
)

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
	var patchedID, patchedBody string
	a.updateFunc = func(_ context.Context, messageID, content string) error {
		patchedID = messageID
		patchedBody = content
		return nil
	}

	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutCard,
		ChatID: wantChatID,
		Card: &messages.Card{
			Title:     "Action Needed",
			Body:      "question: 何时检查版本? [仅 REPL 启动(裸 nightme) | REPL + 所有 CLI 子命令]",
			Options:   []string{wantOption, "REPL + 所有 CLI 子命令"},
			RequestID: "req-1",
		},
	}); err != nil {
		t.Fatalf("Send(OutCard): %v", err)
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
	raw, err := buildInteractiveCard(&messages.Card{
		Title:     "Action Needed",
		RequestID: "r1",
		Kind:      messages.CardKindPermission,
		Questions: []messages.CardQuestion{{
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
	raw, err := buildInteractiveCard(&messages.Card{
		Title:     "Action Needed",
		RequestID: "r1",
		Kind:      messages.CardKindPermission,
		Questions: []messages.CardQuestion{
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
	var patched []string
	a.updateFunc = func(_ context.Context, _, content string) error {
		patched = append(patched, content)
		return nil
	}

	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutCard,
		ChatID: wantChatID,
		Card: &messages.Card{
			Title:     "Action Needed",
			RequestID: "req-wiz",
			Questions: []messages.CardQuestion{
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
		t.Fatalf("Send(OutCard): %v", err)
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
	if len(patched) != 1 {
		t.Fatalf("PATCH count after first click = %d, want 1", len(patched))
	}
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
	picks, ok := messages.DecodeQuestionPicks(got.Action.Option)
	if !ok {
		t.Fatalf("Option = %q, want nm-q: batch", got.Action.Option)
	}
	if len(picks) != 2 || picks[0].ID != "q-trigger" || picks[1].ID != "q-source" {
		t.Errorf("picks = %+v", picks)
	}
	if len(picks[0].Selected) != 1 || picks[0].Selected[0] != opt1 {
		t.Errorf("pick[0] = %+v", picks[0])
	}
	if len(picks[1].Selected) != 1 || picks[1].Selected[0] != opt2 {
		t.Errorf("pick[1] = %+v", picks[1])
	}
	if len(patched) != 2 {
		t.Fatalf("PATCH count after last click = %d, want 2", len(patched))
	}
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
		Kind:   messages.OutCard,
		ChatID: "oc_skip",
		Card: &messages.Card{
			Title:     "Action Needed",
			RequestID: "req-skip",
			Questions: []messages.CardQuestion{
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
	picks, ok := messages.DecodeQuestionPicks(got.Action.Option)
	if !ok {
		t.Fatalf("Option = %q, want nm-q: batch", got.Action.Option)
	}
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

func TestSend_OutCardPatch_EmptyReplyToSettlesLastCard(t *testing.T) {
	a := testAdapter(t)
	const (
		chatID = "oc_settle"
		msgID  = "om_settle"
	)
	a.sendFunc = func(_ context.Context, _, _, _, _ string, _ bool) (string, error) {
		return msgID, nil
	}
	var patched string
	a.updateFunc = func(_ context.Context, _, content string) error {
		patched = content
		return nil
	}
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutCard,
		ChatID: chatID,
		Card: &messages.Card{
			Title:     "Waiting for approval",
			Body:      "Bash: escalate sandbox",
			Options:   []string{"Allow once", "Reject"},
			RequestID: "req-settle",
		},
	}); err != nil {
		t.Fatalf("Send OutCard: %v", err)
	}
	if err := a.Send(context.Background(), messages.OutboundMessage{
		Kind:   messages.OutCardPatch,
		ChatID: chatID,
		Card: &messages.Card{
			Title: "Waiting for approval",
			Body:  "✓ **allowed-once**（dashboard）",
			Kind:  messages.CardKindPermission,
		},
	}); err != nil {
		t.Fatalf("Send OutCardPatch: %v", err)
	}
	if patched == "" {
		t.Fatal("expected PATCH of last opt card")
	}
	if strings.Contains(patched, `"tag":"button"`) {
		t.Errorf("settled PATCH should hide buttons; got %s", patched)
	}
	if !strings.Contains(patched, "allowed-once") {
		t.Errorf("settled PATCH missing outcome; got %s", patched)
	}
}

func TestBuildInteractiveCard_ApprovalHasNoCustomInput(t *testing.T) {
	raw, err := buildInteractiveCard(&messages.Card{
		Title:     "Waiting for approval",
		Body:      "Bash: escalate sandbox",
		Options:   []string{"Allow once", "Reject"},
		RequestID: "r-appr",
		Kind:      messages.CardKindPermission,
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
		Kind:   messages.OutCard,
		ChatID: "oc_oneshot_skip",
		Card: &messages.Card{
			Title:     "Action Needed",
			RequestID: "req-oneshot-skip",
			Questions: []messages.CardQuestion{
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
	picks, ok := messages.DecodeQuestionPicks(got.Action.Option)
	if !ok {
		t.Fatalf("Option = %q, want nm-q: skip batch", got.Action.Option)
	}
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
		Kind:   messages.OutCard,
		ChatID: "oc_oneshot_custom",
		Card: &messages.Card{
			Title:     "Action Needed",
			RequestID: "req-oneshot-custom",
			Questions: []messages.CardQuestion{
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
	picks, ok := messages.DecodeQuestionPicks(got.Action.Option)
	if !ok {
		t.Fatalf("Option = %q, want nm-q: custom batch", got.Action.Option)
	}
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
		Kind:   messages.OutCard,
		ChatID: "oc_empty_custom",
		Card: &messages.Card{
			Title:     "Action Needed",
			RequestID: "req-empty-custom",
			Questions: []messages.CardQuestion{
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
		Kind:   messages.OutCard,
		ChatID: "oc_custom_skip",
		Card: &messages.Card{
			Title:     "Action Needed",
			RequestID: "req-custom-skip",
			Questions: []messages.CardQuestion{
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
	picks, ok := messages.DecodeQuestionPicks(got.Action.Option)
	if !ok {
		t.Fatalf("Option = %q, want nm-q: batch", got.Action.Option)
	}
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
		Kind:   messages.OutCard,
		ChatID: wantChatID,
		Card: &messages.Card{
			Title:     "Action Needed",
			RequestID: "req-form-opt",
			Questions: []messages.CardQuestion{{
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
	card := &messages.Card{
		Questions: []messages.CardQuestion{{
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
