// Package slack — registers the slack Channel Builder in the channel
// registry. cmd/nightme imports this package (directly or
// transitively) so init() runs at process start, making
// `nightme login slack` and the runtime's channel.BuildAll see the
// slack builder.
package slack

import (
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
)

func init() {
	channel.Register("slack", chatIDPrefix, func(cfg *config.Config) (channel.Channel, error) {
		return NewAdapter(cfg)
	})
}
