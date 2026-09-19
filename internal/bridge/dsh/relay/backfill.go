// backfill.go — central history reader for the relay.
//
// Replaces the per-driver runBackfillLoop that polled
// /api/session.history (a non-existent endpoint — every call
// 404'd silently, every 2 s, for every session, indefinitely).
//
// This file owns the only path that calls dsh session/page.
// Read flow on Bridge.History(ctx, id, sinceSeq):
//
//  1. Read the per-session ring buffer for events with
//     seq > sinceSeq. Buffer is bounded at ringBufferSize; for
//     recent reads this is the entire answer.
//  2. If sinceSeq < oldest buffered seq (the buffer has rolled),
//     or if the buffer is empty AND sinceSeq >= 0, page through
//     session/page from sinceSeq+1 to fetch the gap.
//  3. Stop when the page response says hasMore=false OR when
//     the caller's sinceSeq is satisfied.
//
// Phase 3 returns wire-shape Event (the translation pipeline
// stays in driver; this path returns the raw page records
// plus the ring). Drivers consume these through their own
// translator/wireState/dispatcher.
package relay

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/cnlangzi/nightme/internal/bridge/dsh/api"
)

type backfiller struct {
	r  *Relay
	mu sync.Mutex
}

func newBackfiller(r *Relay) *backfiller {
	return &backfiller{r: r}
}

func (b *backfiller) stop() {}

func (b *backfiller) fetch(ctx context.Context, s *sessionState, sinceSeq int64) ([]api.Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()

	ring := s.ringSnapshot()

	if sinceSeq < 0 {
		if len(ring) == 0 {
			records, err := b.pageThrough(ctx, s.id, -1)
			if err != nil {
				return nil, err
			}
			return recordsToEvents(records), nil
		}
		return ring, nil
	}

	if sinceSeq >= s.lastSeqSeen() {
		if len(ring) == 0 {
			records, err := b.pageThrough(ctx, s.id, sinceSeq)
			if err != nil {
				return nil, err
			}
			return recordsToEvents(records), nil
		}
		return filterAfter(ring, sinceSeq), nil
	}

	oldest := int64(0)
	if len(ring) > 0 {
		oldest = ring[0].Seq
	}
	if oldest > 0 && sinceSeq >= oldest {
		return filterAfter(ring, sinceSeq), nil
	}

	records, err := b.pageThrough(ctx, s.id, sinceSeq)
	if err != nil {
		return nil, err
	}
	return recordsToEvents(records), nil
}

func (b *backfiller) pageThrough(ctx context.Context, sessionID string, sinceSeq int64) ([]pageRecord, error) {
	var (
		out       []pageRecord
		beforeSeq *int64
	)
	for {
		ts := sinceSeq + 1
		records, hasMore, err := b.r.fetchPage(ctx, sessionID, ts, beforeSeq, nil)
		if err != nil {
			return nil, err
		}
		out = append(out, records...)
		if !hasMore {
			return out, nil
		}
		if len(records) == 0 {
			return out, nil
		}
		last := records[len(records)-1].Seq
		beforeSeq = &last
	}
}

func filterAfter(events []api.Event, sinceSeq int64) []api.Event {
	if len(events) == 0 {
		return nil
	}
	for i, ev := range events {
		if ev.Seq > sinceSeq {
			out := make([]api.Event, len(events)-i)
			copy(out, events[i:])
			return out
		}
	}
	return nil
}

func recordsToEvents(records []pageRecord) []api.Event {
	if len(records) == 0 {
		return nil
	}
	out := make([]api.Event, len(records))
	for i, r := range records {
		out[i] = api.Event{
			Seq:  r.Seq,
			Type: r.Type,
			Time: r.Time,
			Data: json.RawMessage(r.Data),
		}
	}
	return out
}
