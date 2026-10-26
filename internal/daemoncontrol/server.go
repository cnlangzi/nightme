// Package daemoncontrol: cross-platform daemon lifecycle.
//
// This file holds the types and methods that are identical on
// Unix and Windows — primarily the wire-format protocols
// (already in protocol.go) and the health-source contract.
//
// Platform-specific behaviour (listener creation, accept loop,
// cleanup) lives in server_unix.go and server_windows.go, which
// each carry the full Server struct definition (Go allows the
// same type to appear in different files when build tags
// guarantee only one compiles per build).
package daemoncontrol

import "encoding/json"

// HealthSource is one running channel's contribution to the "health"
// RPC.
//
// The daemon registers one source per started channel; the server
// calls each of them on every "health" RPC and assembles the results
// into a HealthPayload. Snapshot and Summary are independent: a
// channel whose raw payload can't be produced still contributes its
// normalized summary, and the other channels are unaffected.
//
// Both payloads are raw JSON: daemoncontrol doesn't import the
// channel packages (which would create an import cycle), so the
// shapes stay loosely coupled.
type HealthSource struct {
	// Channel names the channel ("feishu", "telegram", …).
	Channel string
	// Snapshot returns the channel's raw HealthSnapshot() payload.
	Snapshot func() (json.RawMessage, error)
	// Summary returns the channel's normalized health summary.
	Summary func() (json.RawMessage, error)
}

// AddHealthSource registers one channel's health source. Called once
// per started channel: the sources accumulate, so the "health" RPC
// reports every running channel instead of only the last one
// registered.
//
// A channel name that is already registered is replaced — one process
// runs one instance per channel, so a repeat registration means the
// channel was restarted and the newer source is the live one.
func (s *Server) AddHealthSource(src HealthSource) {
	s.healthMu.Lock()
	defer s.healthMu.Unlock()
	for i := range s.healthSources {
		if s.healthSources[i].Channel == src.Channel {
			s.healthSources[i] = src
			return
		}
	}
	s.healthSources = append(s.healthSources, src)
}

// healthPayload collects every registered channel's health. A source
// that fails contributes its error instead of its payload; the other
// channels are still reported.
func (s *Server) healthPayload() HealthPayload {
	s.healthMu.Lock()
	sources := make([]HealthSource, len(s.healthSources))
	copy(sources, s.healthSources)
	s.healthMu.Unlock()

	payload := HealthPayload{Channels: make([]ChannelHealth, 0, len(sources))}
	for _, src := range sources {
		entry := ChannelHealth{Channel: src.Channel}
		if src.Summary != nil {
			summary, err := src.Summary()
			if err != nil {
				entry.Error = err.Error()
			} else {
				entry.Summary = summary
			}
		}
		if src.Snapshot != nil {
			snapshot, err := src.Snapshot()
			if err != nil {
				if entry.Error == "" {
					entry.Error = err.Error()
				}
			} else {
				entry.Health = snapshot
			}
		}
		payload.Channels = append(payload.Channels, entry)
	}
	return payload
}
