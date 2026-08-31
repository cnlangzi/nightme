// Package feishu — registers the feishu Channel Builder in the
// channel registry. cmd/nightme imports this package (directly or
// transitively) so init() runs at process start, making
// `nightme login feishu` and the runtime's `channel.BuildAll` see
// the feishu builder.
package feishu

import (
	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
)

func init() {
	channel.Register("feishu", "oc_", func(cfg *config.Config) (channel.Channel, error) {
		return NewAdapter(cfg)
	})
}
