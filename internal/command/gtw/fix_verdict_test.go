package gtw

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/messages"
	"github.com/cnlangzi/nightme/pkg/outcome"
)

// TestFixVerdict_AnUpstreamFailureIsTheVerdict — a turn that died on
// a rate limit is a rate limit, not an empty worktree.
//
// The worktree a dead turn leaves behind carries no change, so judging
// it would report "the agent left the worktree unchanged" — a 500
// that hides the 429 the program branches on.
func TestFixVerdict_AnUpstreamFailureIsTheVerdict(t *testing.T) {
	p := newOutcomeProbe(t)
	v := &fixVerdict{
		ctx:  p.ctx,
		deps: HandlerDeps{},
		work: p.work,
		data: map[string]string{"issue": "42"},
		once: sync.Once{},
	}

	v.report(agent.PromptEndClean, &agent.APIError{
		Status:  http.StatusTooManyRequests,
		Message: "Request rejected",
	})

	// The verdict is reported on its own goroutine and it releases the
	// work, so the settled message goes out when it lands.
	deadline := time.Now().Add(5 * time.Second)
	var got *outcome.Outcome
	for time.Now().Before(deadline) {
		p.ch.mu.Lock()
		for _, m := range p.ch.sends {
			if m.Kind == messages.OutMessageSettled {
				got = m.Outcome
			}
		}
		p.ch.mu.Unlock()
		if got != nil {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got == nil {
		t.Fatal("the verdict settled no outcome")
	}
	if got.Status != http.StatusTooManyRequests {
		t.Errorf("settled status = %d, want %d", got.Status, http.StatusTooManyRequests)
	}
	if got.StatusText != "Request rejected" {
		t.Errorf("settled words = %q, want the upstream's own", got.StatusText)
	}
	if got.Data["issue"] != "42" {
		t.Errorf("the facts did not ride along: %v", got.Data)
	}
}
