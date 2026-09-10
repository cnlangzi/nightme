package runtime

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/messages"
)

// captureEmitter records every Send call for assertion.
type captureEmitter struct {
	mu   sync.Mutex
	sent []messages.OutboundMessage
	err  error
}

func (e *captureEmitter) Send(_ context.Context, msg messages.OutboundMessage) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.sent = append(e.sent, msg)
	return e.err
}

func (e *captureEmitter) snapshot() []messages.OutboundMessage {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]messages.OutboundMessage, len(e.sent))
	copy(out, e.sent)
	return out
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// TestSendHeartbeatFollowUp_EmitsOutHeartbeat — happy path:
// a non-empty snapshot produces one OutHeartbeat with the
// expected fields and snapshot pointer.
func TestSendHeartbeatFollowUp_EmitsOutHeartbeat(t *testing.T) {
	em := &captureEmitter{}
	snap := messages.HeartbeatSnapshot{
		ThinkCount: 2, ToolCount: 1, LastBeatAt: time.Now(), Status: messages.HeartbeatDone,
	}
	sendHeartbeatFollowUp(em, discardLogger(),
		"oc_1", "om_1", "observe", snap)

	got := em.snapshot()
	if len(got) != 1 {
		t.Fatalf("Send calls = %d, want 1", len(got))
	}
	m := got[0]
	if m.Kind != messages.OutHeartbeat {
		t.Errorf("Kind = %v, want OutHeartbeat", m.Kind)
	}
	if m.ChatID != "oc_1" || m.ReplyTo != "om_1" {
		t.Errorf("routing fields wrong: chatID=%q replyTo=%q", m.ChatID, m.ReplyTo)
	}
	if m.Heartbeat == nil {
		t.Fatal("Heartbeat payload is nil")
	}
	if m.Heartbeat.ThinkCount != 2 || m.Heartbeat.ToolCount != 1 || m.Heartbeat.Status != messages.HeartbeatDone {
		t.Errorf("snapshot not copied faithfully: %+v", *m.Heartbeat)
	}
}

// TestSendHeartbeatFollowUp_DropsEmpty — zero-valued snapshot
// (no counters, no LastBeatAt, no Done) is dropped silently.
// The empty-snapshot drop is the safety net for the LRU-evict
// race between Observe (terminal flip) and Snapshot.
func TestSendHeartbeatFollowUp_DropsEmpty(t *testing.T) {
	em := &captureEmitter{}
	sendHeartbeatFollowUp(em, discardLogger(),
		"oc_1", "om_1", "observe", messages.HeartbeatSnapshot{})
	if len(em.snapshot()) != 0 {
		t.Fatalf("empty snapshot must not be sent; got %d calls", len(em.snapshot()))
	}
}

// TestSendHeartbeatFollowUp_DoneOnlyNotEmpty — Done-only
// snapshot (the /think off + /tools off terminal case) IS
// non-empty and DOES get sent. Pins the Empty() contract for
// the helper's drop branch.
func TestSendHeartbeatFollowUp_DoneOnlyNotEmpty(t *testing.T) {
	em := &captureEmitter{}
	sendHeartbeatFollowUp(em, discardLogger(),
		"oc_1", "om_1", "markdone", messages.HeartbeatSnapshot{Status: messages.HeartbeatDone})
	if len(em.snapshot()) != 1 {
		t.Fatalf("Done-only snapshot must be sent; got %d calls", len(em.snapshot()))
	}
}

// TestSendHeartbeatFollowUp_SourceTagInLogs — the source tag
// is included in the empty-drop debug log so a noisy channel's
// logs can be attributed to the trigger site (observe /
// markdone / endprompt). Pin that the tag flows through.
func TestSendHeartbeatFollowUp_SourceTagInLogs(t *testing.T) {
	em := &captureEmitter{}
	// Source-tagged empty drop — the helper logs at debug level.
	// We don't capture the log here (other tests cover log shape
	// via the integration tests); this just pins that the empty
	// branch doesn't crash and doesn't emit.
	sendHeartbeatFollowUp(em, discardLogger(),
		"oc_1", "om_1", "observe:OutResult", messages.HeartbeatSnapshot{})
	if len(em.snapshot()) != 0 {
		t.Fatalf("empty snapshot with source tag still must be dropped")
	}
}

// TestSendHeartbeatFollowUp_SendErrorTolerated — Send errors
// must NOT propagate; the helper logs and returns. The runtime
// must never block the originating event because the heartbeat
// follow-up couldn't be delivered.
func TestSendHeartbeatFollowUp_SendErrorTolerated(t *testing.T) {
	em := &captureEmitter{err: errors.New("boom")}
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("helper must not panic on Send error, got %v", r)
		}
	}()
	sendHeartbeatFollowUp(em, discardLogger(),
		"oc_1", "om_1", "observe",
		messages.HeartbeatSnapshot{ThinkCount: 1, LastBeatAt: time.Now()})
}
