package outbound

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestDispatchSinkEvent_LogsDroppedEvent covers the one-shot path's
// drop branch: a dropped event never reaches the channel, so the
// debug log is the only trace it leaves.
func TestDispatchSinkEvent_LogsDroppedEvent(t *testing.T) {
	t.Run("dropped event is logged", func(t *testing.T) {
		var buf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug}))

		dispatchSinkEvent(context.Background(), &fakeChannel{name: "test"}, nil, logger,
			"oc_test", "om_test_1", "pi", agent.AgentEvent{Kind: agent.EventAgentDone}, nil)

		out := buf.String()
		if !strings.Contains(out, "Translate dropped event") {
			t.Errorf("expected a dropped-event log record, got %q", out)
		}
		if !strings.Contains(out, "chat_id=oc_test") {
			t.Errorf("expected chat_id=oc_test in log record, got %q", out)
		}
		if !strings.Contains(out, "kind=done") {
			t.Errorf("expected kind=done in log record, got %q", out)
		}
	})

	t.Run("nil logger does not panic", func(t *testing.T) {
		dispatchSinkEvent(context.Background(), &fakeChannel{name: "test"}, nil, nil,
			"oc_test", "om_test_1", "pi", agent.AgentEvent{Kind: agent.EventAgentDone}, nil)
	})
}
