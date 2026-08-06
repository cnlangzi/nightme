package services

import (
	"context"
	"testing"
)

func TestReactionRouter_EmptyRouterReturnsFalse(t *testing.T) {
	r := NewReactionRouter()
	got := r.Handle(context.Background(), "c1", ReactionEvent{TargetMsgID: "m1", Emoji: "✅"})
	if got {
		t.Errorf("empty router should return false, got true")
	}
}

func TestReactionRouter_RegisterAndHandleExactMatch(t *testing.T) {
	r := NewReactionRouter()
	called := 0
	r.Register("c1", func(_ context.Context, ev ReactionEvent) bool {
		called++
		if ev.Emoji != "✅" {
			t.Errorf("expected emoji ✅, got %q", ev.Emoji)
		}
		return true
	})
	got := r.Handle(context.Background(), "c1", ReactionEvent{TargetMsgID: "m1", Emoji: "✅"})
	if !got {
		t.Errorf("expected handler consumed, got false")
	}
	if called != 1 {
		t.Errorf("expected handler called once, got %d", called)
	}
}

func TestReactionRouter_WildcardFallsThrough(t *testing.T) {
	r := NewReactionRouter()
	wild := 0
	r.Register("*", func(_ context.Context, _ ReactionEvent) bool {
		wild++
		return true
	})

	// No exact match -> wildcard should fire.
	got := r.Handle(context.Background(), "chatA", ReactionEvent{TargetMsgID: "m1", Emoji: "✅"})
	if !got {
		t.Errorf("expected wildcard to consume, got false")
	}
	if wild != 1 {
		t.Errorf("expected wildcard called once, got %d", wild)
	}
}

func TestReactionRouter_ExactMatchWinsOverWildcard(t *testing.T) {
	r := NewReactionRouter()
	wild := 0
	specific := 0
	r.Register("*", func(_ context.Context, _ ReactionEvent) bool {
		wild++
		return true
	})
	r.Register("c1", func(_ context.Context, _ ReactionEvent) bool {
		specific++
		return true
	})

	got := r.Handle(context.Background(), "c1", ReactionEvent{Emoji: "✅"})
	if !got {
		t.Errorf("expected consumed, got false")
	}
	if specific != 1 || wild != 0 {
		t.Errorf("expected specific=1 wild=0, got specific=%d wild=%d", specific, wild)
	}
}

func TestReactionRouter_NoMatchNoWildcardReturnsFalse(t *testing.T) {
	r := NewReactionRouter()
	got := r.Handle(context.Background(), "c1", ReactionEvent{Emoji: "✅"})
	if got {
		t.Errorf("expected false for no handler, got true")
	}
}

func TestReactionRouter_HandlerReturnsFalse(t *testing.T) {
	r := NewReactionRouter()
	r.Register("c1", func(_ context.Context, _ ReactionEvent) bool { return false })
	got := r.Handle(context.Background(), "c1", ReactionEvent{Emoji: "✅"})
	if got {
		t.Errorf("expected false when handler returns false, got true")
	}
}

func TestReactionRouter_HandlerPanicRecovers(t *testing.T) {
	r := NewReactionRouter()
	r.Register("c1", func(_ context.Context, _ ReactionEvent) bool {
		panic("boom")
	})
	// Should NOT panic; should return false.
	got := r.Handle(context.Background(), "c1", ReactionEvent{Emoji: "✅"})
	if got {
		t.Errorf("expected false after panic, got true")
	}
}

func TestReactionRouter_RegisterNilIgnored(t *testing.T) {
	r := NewReactionRouter()
	r.Register("c1", nil)
	got := r.Handle(context.Background(), "c1", ReactionEvent{Emoji: "✅"})
	if got {
		t.Errorf("expected nil handler = no consume, got true")
	}
}

func TestReactionRouter_RegisterEmptyIDIgnored(t *testing.T) {
	r := NewReactionRouter()
	called := 0
	r.Register("", func(_ context.Context, _ ReactionEvent) bool {
		called++
		return true
	})
	r.Handle(context.Background(), "", ReactionEvent{Emoji: "✅"})
	if called != 0 {
		t.Errorf("expected empty-id Register to be ignored, got %d calls", called)
	}
}

func TestReactionRouter_OverwriteLastWins(t *testing.T) {
	r := NewReactionRouter()
	first := 0
	second := 0
	r.Register("c1", func(_ context.Context, _ ReactionEvent) bool { first++; return true })
	r.Register("c1", func(_ context.Context, _ ReactionEvent) bool { second++; return true })
	r.Handle(context.Background(), "c1", ReactionEvent{Emoji: "✅"})
	if first != 0 || second != 1 {
		t.Errorf("expected second handler to win, got first=%d second=%d", first, second)
	}
}
