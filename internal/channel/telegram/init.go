// Package telegram — registers the telegram Channel Builder in the
// channel registry. cmd/nightme imports this package (directly or
// transitively) so init() runs at process start, making
// `nightme login telegram` and the runtime's `channel.BuildAll`
// see the telegram builder.
package telegram

import (
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
)

func init() {
	channel.Register("telegram", "tg_", func(cfg *config.Config) (channel.Channel, error) {
		return NewAdapter(cfg)
	})
}
