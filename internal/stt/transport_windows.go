//go:build windows

package stt

import (
	"context"
	"fmt"
)

// On Windows the endpoint is a named-pipe name. We do not
// require \\.\pipe\ — the runtime prepends it.

func defaultEndpoint(dataDir string) (Endpoint, error) {
	if dataDir == "" {
		return "", fmt.Errorf("stt: empty data dir")
	}
	return Endpoint("nightme-stt"), nil
}

type windowsTransport struct{}

func DefaultTransport() Transport { return windowsTransport{} }

func (windowsTransport) Dial(ctx context.Context, endpoint Endpoint) (Conn, error) {
	return nil, fmt.Errorf("stt: windows named-pipe transport is a stub; " +
		"nightme-stt integration is verified on Unix per issue #381 §20")
}

func (windowsTransport) Listen(ctx context.Context, endpoint Endpoint) (Listener, error) {
	return nil, fmt.Errorf("stt: windows named-pipe transport is a stub")
}
