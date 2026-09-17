package discord

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"

	"github.com/cnlangzi/nightme/internal/messages"
)

// TestChoiceState_ConcurrentPatchAndClickNoRace exercises the
// race fix: a single choiceState is mutated concurrently by
// patchChoice (runtime goroutine) and markSettled (callback
// goroutine). Without per-state locking the race detector
// reports a data race on state.Settled / state.Choice.
// Run with -race to verify the fix.
func TestChoiceState_ConcurrentPatchAndClickNoRace(t *testing.T) {
	a := newTestAdapter(&fakeREST{})
	state := &choiceState{
		RequestID: "req-1",
		ChannelID: "chan1",
		MessageID: "card-1",
		Choice:    &messages.Choice{RequestID: "req-1"},
	}
	_ = a.choiceStore.Put(state)

	var wg sync.WaitGroup
	const iterations = 200
	for i := 0; i < iterations; i++ {
		wg.Add(2)
		go func(i int) {
			defer wg.Done()
			_ = a.Send(context.Background(), messages.OutboundMessage{
				ChatID: "dc_chan1", Kind: messages.OutChoicePatch,
				Choice: &messages.Choice{
					RequestID: "req-1",
					Settled:   i%2 == 0,
					Options:   messages.ChoiceOptionsFromLabels([]string{"yes", "no"}),
				},
			})
		}(i)
		go func() {
			defer wg.Done()
			state.markSettledLocal("yes")
		}()
	}
	wg.Wait()
}

// TestLastResultMessageID_OneShotEviction verifies the map entry
// is deleted after the read so long-running daemons cannot grow
// the map unboundedly.
func TestLastResultMessageID_OneShotEviction(t *testing.T) {
	a := newTestAdapter(&fakeREST{})
	a.rememberResultMessageID("dc_chan1", "user-msg", "result-msg-1")

	if got := a.lastResultMessageID("dc_chan1", "user-msg"); got != "result-msg-1" {
		t.Errorf("first read = %q, want result-msg-1", got)
	}
	if got := a.lastResultMessageID("dc_chan1", "user-msg"); got != "" {
		t.Errorf("second read = %q, want empty (one-shot eviction)", got)
	}
	if got := a.lastResultMessageID("dc_chan1", "user-msg"); got != "" {
		t.Errorf("third read = %q, want empty (stable after eviction)", got)
	}
}

// TestAttachmentDownload_PathDisambiguation ensures two
// attachments with the same filename land at distinct LocalPaths
// so the second doesn't silently overwrite the first.
func TestAttachmentDownload_PathDisambiguation(t *testing.T) {
	tmp := t.TempDir()
	a := newTestAdapter(&fakeREST{downloadBody: []byte("content")})
	a.dataDir = tmp
	msg := &Message{
		ID:        "msg-1",
		ChannelID: "chan1",
		Attachments: []Attachment{
			{ID: "att-1", Filename: "file.txt", URL: "https://cdn/x", ContentType: "text/plain", Size: 7},
			{ID: "att-2", Filename: "file.txt", URL: "https://cdn/y", ContentType: "text/plain", Size: 7},
		},
	}
	atts := a.downloadAttachments(context.Background(), msg, "dc_chan1")
	if len(atts) != 2 {
		t.Fatalf("attachments = %d, want 2", len(atts))
	}
	if atts[0].LocalPath == atts[1].LocalPath {
		t.Errorf("both attachments share LocalPath %q (filename collision)", atts[0].LocalPath)
	}
	if atts[0].LocalPath == "" || atts[1].LocalPath == "" {
		t.Errorf("LocalPath empty: %+v / %+v", atts[0], atts[1])
	}
}

// TestAcknowledgeModal_DataMarshalsTitleAndComponents verifies
// the critical-bug fix: InteractionResponse.Data carries
// *ModalPayload directly (no json:"-" Modal field), so the
// modal envelope's title / custom_id / components land on the
// wire. Without the fix the modal ACK is empty and Discord
// rejects it as a malformed interaction callback.
func TestAcknowledgeModal_DataMarshalsTitleAndComponents(t *testing.T) {
	rest := &fakeREST{}
	a := newTestAdapter(rest)
	a.acknowledgeModal(Interaction{ID: "int-1", Token: "tok"}, "i:req-1")
	if len(rest.acks) != 1 {
		t.Fatalf("acks = %d, want 1", len(rest.acks))
	}
	body, err := json.Marshal(rest.acks[0].Body)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	bodyStr := string(body)
	for _, key := range []string{`"title":"Your answer"`, `"custom_id":"i:req-1"`, `"components"`} {
		if !strings.Contains(bodyStr, key) {
			t.Errorf("ack body missing %q: %s", key, bodyStr)
		}
	}
}
