// Package daemoncontrol: cross-platform daemon lifecycle.
//
// This file holds the types and methods that are identical on
// Unix and Windows — primarily the wire-format protocols
// (already in protocol.go) and the health-provider contract.
//
// Platform-specific behaviour (listener creation, accept loop,
// cleanup) lives in server_unix.go and server_windows.go, which
// each carry the full Server struct definition (Go allows the
// same type to appear in different files when build tags
// guarantee only one compiles per build).
package daemoncontrol

import "encoding/json"

// HealthProvider supplies the live WS lifecycle snapshot for
// the "health" RPC. Set via Server.SetHealthProvider after
// Listen. Optional — when nil, "health" returns an error to
// the caller.
type HealthProvider func() (channel string, snapshot json.RawMessage, err error)
