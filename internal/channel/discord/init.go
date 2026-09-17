// Package discord — registers the discord Channel Builder in the
// channel registry. cmd/nightme imports this package (directly or
// transitively) so init() runs at process start, making
// `nightme login discord` and the runtime's `channel.BuildAll` see
// the discord builder.
//
// The registry prefix ("dc_") is read by chatstore.New to validate
// chat_sessions.json keys WITHOUT having to construct the adapter —
// chatstore loads BEFORE BuildAll, and an adapter's Builder may
// legitimately fail (missing credentials) at startup even when
// valid entries already exist on disk. See internal/channel/registry.go
// for the full rationale.
package discord

import (
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
)

func init() {
	channel.Register("discord", chatIDPrefix, func(cfg *config.Config) (channel.Channel, error) {
		return NewAdapter(cfg)
	})
}
