// control_projection_test.go — unit tests for the host's
// session/control projection store. Verifies:
//
//   - baseline seeds entries + fires fresh watchers
//   - projection deltas update entries + fire change watchers
//   - projection deltas with the same value do not fire (no spam)
//   - sessions that vanish from a new baseline get a fresh="" fire
//   - non-modelSelection keys are no-ops
//   - GetSessionModel returns the resolved (next ?? lastUsed) value
//   - WatchSessionModel returns a stable unsubscribe handle

package host

import (
	"sync"
	"testing"
)

// TestControlProjection_BaselineSeedsEntries verifies the first
// baseline populates entries and fires fresh watchers with the
// resolved model id.
func TestControlProjection_BaselineSeedsEntries(t *testing.T) {
	p := newControlProjection()
	p.ApplyBaseline(map[string]string{
		"sess-A": "minimax",
		"sess-B": "claude-opus",
	})
	if got := p.GetSessionModel("sess-A"); got != "minimax" {
		t.Errorf("sess-A = %q; want minimax", got)
	}
	if got := p.GetSessionModel("sess-B"); got != "claude-opus" {
		t.Errorf("sess-B = %q; want claude-opus", got)
	}
	if got := p.GetSessionModel("sess-missing"); got != "" {
		t.Errorf("sess-missing = %q; want \"\"", got)
	}
	if !p.baseline {
		t.Errorf("baseline flag false after ApplyBaseline; want true")
	}
}

// TestControlProjection_WatchFreshCallback verifies a watcher
// registered before baseline fires with fresh=true when the
// baseline lands.
func TestControlProjection_WatchFreshCallback(t *testing.T) {
	p := newControlProjection()
	var got string
	var fresh bool
	var fired bool
	unsub := p.WatchSessionModel("sess-A", func(model string, isFresh bool) {
		got = model
		fresh = isFresh
		fired = true
	})
	defer unsub()

	if !fired {
		t.Fatal("fresh callback did not fire on register (current value = \"\")")
	}
	if got != "" {
		t.Errorf("initial model = %q; want \"\" (no baseline yet)", got)
	}
	if !fresh {
		t.Errorf("initial fresh = false; want true")
	}

	// Apply a baseline with a fresh session id (sess-A is
	// pre-registered so its baseline fire is fresh=false; sess-B
	// is new so its baseline fire is fresh=true).
	p.ApplyBaseline(map[string]string{"sess-A": "minimax", "sess-B": "claude-opus"})
	if got != "minimax" {
		t.Errorf("post-baseline model = %q; want minimax", got)
	}
	if fresh {
		t.Errorf("post-baseline fresh = true; want false (entry pre-existed)")
	}
}

// TestControlProjection_ProjectionDeltaFiresChange verifies a
// projection delta with a new model fires the watcher with
// fresh=false.
func TestControlProjection_ProjectionDeltaFiresChange(t *testing.T) {
	p := newControlProjection()
	p.ApplyBaseline(map[string]string{"sess-A": "minimax"})

	type fire struct {
		model string
		fresh bool
	}
	var mu sync.Mutex
	var fires []fire
	unsub := p.WatchSessionModel("sess-A", func(model string, isFresh bool) {
		mu.Lock()
		fires = append(fires, fire{model, isFresh})
		mu.Unlock()
	})
	defer unsub()

	// Wipe the initial-fresh fires so we can assert just the delta.
	mu.Lock()
	fires = nil
	mu.Unlock()

	p.ApplyProjection("sess-A", "modelSelection", "claude-opus")
	mu.Lock()
	defer mu.Unlock()
	if len(fires) != 1 {
		t.Fatalf("len(fires) = %d; want 1", len(fires))
	}
	if fires[0].model != "claude-opus" {
		t.Errorf("fires[0].model = %q; want claude-opus", fires[0].model)
	}
	if fires[0].fresh {
		t.Errorf("fires[0].fresh = true; want false on delta")
	}
	if got := p.GetSessionModel("sess-A"); got != "claude-opus" {
		t.Errorf("GetSessionModel after delta = %q; want claude-opus", got)
	}
}

// TestControlProjection_SameValueDoesNotFire verifies the dedup —
// dsh can echo the same projection value; we should not spam the
// watcher (which would re-emit EventAgentReady).
func TestControlProjection_SameValueDoesNotFire(t *testing.T) {
	p := newControlProjection()
	p.ApplyBaseline(map[string]string{"sess-A": "minimax"})

	count := 0
	unsub := p.WatchSessionModel("sess-A", func(model string, isFresh bool) {
		if !isFresh {
			count++
		}
	})
	defer unsub()

	// Initial fresh=true fire consumed; counter starts at 0.
	p.ApplyProjection("sess-A", "modelSelection", "minimax") // same value
	p.ApplyProjection("sess-A", "modelSelection", "minimax") // same value
	if count != 0 {
		t.Errorf("dedup count = %d; want 0 (no change should fire)", count)
	}
}

// TestControlProjection_NonModelSelectionKeyIsNoop verifies the
// bridge does not act on projection keys it does not know. Add a
// new key on the dsh server side and the bridge keeps working.
func TestControlProjection_NonModelSelectionKeyIsNoop(t *testing.T) {
	p := newControlProjection()
	p.ApplyBaseline(map[string]string{"sess-A": "minimax"})

	count := 0
	unsub := p.WatchSessionModel("sess-A", func(model string, isFresh bool) {
		if !isFresh {
			count++
		}
	})
	defer unsub()

	p.ApplyProjection("sess-A", "some-future-key", "ignored-value")
	p.ApplyProjection("sess-A", "inbox", `{"foo":"bar"}`)
	if count != 0 {
		t.Errorf("non-modelSelection fire count = %d; want 0", count)
	}
	if got := p.GetSessionModel("sess-A"); got != "minimax" {
		t.Errorf("model after noop = %q; want minimax", got)
	}
}

// TestControlProjection_SessionVanishesFromBaseline verifies a
// session that no longer appears in a new baseline fires its
// watchers with model="". fresh=false so the driver treats it as
// a model change and re-emits EventAgentReady (the runtime's
// SetModel is a no-op for "" but the persist side effect is the
// same; a subsequent non-empty projection will overwrite).
func TestControlProjection_SessionVanishesFromBaseline(t *testing.T) {
	p := newControlProjection()
	p.ApplyBaseline(map[string]string{"sess-A": "minimax"})

	var got string
	var fresh bool
	var fired bool
	unsub := p.WatchSessionModel("sess-A", func(model string, isFresh bool) {
		got = model
		fresh = isFresh
		fired = true
	})
	defer unsub()

	fired, fresh, got = false, false, ""
	p.ApplyBaseline(map[string]string{"sess-B": "claude-opus"})

	if !fired {
		t.Fatal("vanish fire did not reach watcher")
	}
	if got != "" {
		t.Errorf("vanished model = %q; want \"\"", got)
	}
	if fresh {
		t.Errorf("vanished fresh = true; want false (treat as model change)")
	}
}

// TestControlProjection_UnsubscribeStopsFires verifies the
// unsubscribe handle stops the watcher from receiving future
// changes.
func TestControlProjection_UnsubscribeStopsFires(t *testing.T) {
	p := newControlProjection()
	p.ApplyBaseline(map[string]string{"sess-A": "minimax"})

	count := 0
	unsub := p.WatchSessionModel("sess-A", func(model string, isFresh bool) {
		if !isFresh {
			count++
		}
	})
	unsub() // before any delta

	p.ApplyProjection("sess-A", "modelSelection", "claude-opus")
	if count != 0 {
		t.Errorf("post-unsubscribe fire count = %d; want 0", count)
	}

	// Idempotency: calling unsub again is a no-op.
	unsub()
}

// TestControlProjection_NilOrEmptyArgsAreSafe verifies the public
// API does not panic on nil receiver / empty sessionID / nil cb.
func TestControlProjection_NilOrEmptyArgsAreSafe(t *testing.T) {
	var p *controlProjection // nil
	if got := p.GetSessionModel("any"); got != "" {
		t.Errorf("nil.GetSessionModel = %q; want \"\"", got)
	}
	unsub := p.WatchSessionModel("any", func(string, bool) {})
	if unsub == nil {
		t.Errorf("nil.WatchSessionModel returned nil unsub; want a no-op func")
	}
	unsub()

	p = newControlProjection()
	unsub = p.WatchSessionModel("", func(string, bool) {})
	if unsub == nil {
		t.Errorf("empty-sessionID.WatchSessionModel returned nil unsub")
	}
	unsub = p.WatchSessionModel("sess-A", nil)
	if unsub == nil {
		t.Errorf("nil-cb.WatchSessionModel returned nil unsub")
	}
}

// TestControlProjection_TranslateControlFrame verifies the wire
// decode for both baseline and projection frames yields the right
// resolved model ids.
func TestControlProjection_TranslateControlFrame(t *testing.T) {
	baseline := []byte(`{
		"type":"baseline",
		"value":{
			"queues":{},
			"jobs":{},
			"projections":{
				"sess-A":{
					"asOfSeq":5,
					"values":{"modelSelection":{"lastUsed":{"provider":"minimax-cn","model":"minimax"},"next":{"provider":"minimax-cn","model":"minimax"}}}
				},
				"sess-B":{
					"asOfSeq":3,
					"values":{"modelSelection":{"next":{"provider":"minimax-cn","model":"minimax-M3"}}}
				},
				"sess-C":{
					"asOfSeq":0,
					"values":{"modelSelection":{"lastUsed":{"provider":"minimax-cn","model":"minimax"},"next":null}}
				},
				"sess-D":{
					"asOfSeq":0,
					"values":{"modelSelection":{}}
				}
			}
		}
	}`)
	frame, ok := translateControlFrame(baseline)
	if !ok {
		t.Fatal("translateControlFrame(baseline) = false")
	}
	if frame.Kind != ControlBaseline {
		t.Errorf("Kind = %q; want baseline", frame.Kind)
	}
	wantModels := map[string]string{
		"sess-A": "minimax",
		"sess-B": "minimax-M3",
		"sess-C": "minimax", // lastUsed fallback when next is null
		"sess-D": "",        // both nil
	}
	for sid, want := range wantModels {
		got := frame.Models[sid]
		if got != want {
			t.Errorf("Models[%q] = %q; want %q", sid, got, want)
		}
	}

	projection := []byte(`{
		"type":"projection",
		"sessionId":"sess-A",
		"key":"modelSelection",
		"seq":7,
		"value":{"next":{"provider":"minimax-cn","model":"claude-opus"}}
	}`)
	frame, ok = translateControlFrame(projection)
	if !ok {
		t.Fatal("translateControlFrame(projection) = false")
	}
	if frame.Kind != ControlProjection {
		t.Errorf("Kind = %q; want projection", frame.Kind)
	}
	if frame.SessionID != "sess-A" {
		t.Errorf("SessionID = %q; want sess-A", frame.SessionID)
	}
	if frame.Key != "modelSelection" {
		t.Errorf("Key = %q; want modelSelection", frame.Key)
	}
	if frame.Model != "claude-opus" {
		t.Errorf("Model = %q; want claude-opus", frame.Model)
	}

	// Non-modelSelection projection rides through undecoded.
	otherProj := []byte(`{
		"type":"projection",
		"sessionId":"sess-A",
		"key":"inbox",
		"seq":8,
		"value":{"foo":"bar"}
	}`)
	frame, ok = translateControlFrame(otherProj)
	if !ok {
		t.Fatal("translateControlFrame(inbox-projection) = false")
	}
	if frame.Key != "inbox" {
		t.Errorf("Key = %q; want inbox", frame.Key)
	}
	if frame.Model != "" {
		t.Errorf("Model on non-modelSelection projection = %q; want \"\"", frame.Model)
	}

	// queue / jobs frames.
	queue := []byte(`{"type":"queue","sessionId":"sess-A","seq":9,"items":[{"kind":"text","content":"text"}]}`)
	frame, ok = translateControlFrame(queue)
	if !ok || frame.Kind != ControlQueue {
		t.Errorf("queue frame decode failed: ok=%v kind=%q", ok, frame.Kind)
	}
	if frame.SessionID != "sess-A" {
		t.Errorf("queue SessionID = %q; want sess-A", frame.SessionID)
	}

	jobs := []byte(`{"type":"jobs","sessionId":"sess-A","seq":10,"jobs":[]}`)
	frame, ok = translateControlFrame(jobs)
	if !ok || frame.Kind != ControlJobs {
		t.Errorf("jobs frame decode failed: ok=%v kind=%q", ok, frame.Kind)
	}

	// Malformed: a baseline with a non-object `value` fails to
	// decode and returns ok=false.
	bad := []byte(`{"type":"baseline","value":"not-an-object"}`)
	if _, ok := translateControlFrame(bad); ok {
		t.Errorf("translateControlFrame(bad baseline) = true; want false")
	}
	empty := []byte(`{"type":"unknown-future-type"}`)
	if _, ok := translateControlFrame(empty); ok {
		t.Errorf("translateControlFrame(unknown type) = true; want false")
	}
}

// TestControlProjection_DispatchAppliesFrames verifies the
// controlProjection.dispatch entrypoint correctly routes both
// baseline and projection frames into the store.
func TestControlProjection_DispatchAppliesFrames(t *testing.T) {
	p := newControlProjection()

	p.dispatch(ControlFrame{
		Kind:   ControlBaseline,
		Models: map[string]string{"sess-A": "minimax"},
	})
	if got := p.GetSessionModel("sess-A"); got != "minimax" {
		t.Fatalf("after baseline, sess-A = %q; want minimax", got)
	}

	p.dispatch(ControlFrame{
		Kind:      ControlProjection,
		SessionID: "sess-A",
		Key:       "modelSelection",
		Model:     "claude-opus",
	})
	if got := p.GetSessionModel("sess-A"); got != "claude-opus" {
		t.Fatalf("after projection, sess-A = %q; want claude-opus", got)
	}

	// Non-modelSelection projection is a no-op for the store.
	p.dispatch(ControlFrame{
		Kind:      ControlProjection,
		SessionID: "sess-A",
		Key:       "inbox",
	})
	if got := p.GetSessionModel("sess-A"); got != "claude-opus" {
		t.Fatalf("after inbox projection, sess-A = %q; want claude-opus (unchanged)", got)
	}
}
