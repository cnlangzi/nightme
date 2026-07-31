package gateway

import (
	"errors"
	"strings"
)

// ErrParseFailure is returned by ParseCommand when the input is not a
// valid slash command. Callers fall back to the underlying agent (see
// spec F-20 §3).
var ErrParseFailure = errors.New("gateway: parse failure")

// ParseCommand splits a slash command into its name and arguments.
//
// The grammar is intentionally minimal for v0.1:
//
//	"/cmd"               -> name="cmd", args=[]
//	"/cmd arg1 arg2"     -> name="cmd", args=["arg1","arg2"]
//	"/cmd  arg  spaced"  -> leading whitespace collapsed; args split on whitespace
//	"not a command"      -> ErrParseFailure
//	""                   -> ErrParseFailure
//	"/"                  -> ErrParseFailure (no command name)
//
// Quoted arguments (e.g. `/run /path with space`) are not supported in
// v0.1 (see F-20 §3). The parser also rejects multi-line input; the
// first line is the command, anything after the first "\n" is the
// agent's problem.
func ParseCommand(text string) (name string, args []string, err error) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return "", nil, ErrParseFailure
	}
	if !strings.HasPrefix(trimmed, "/") {
		return "", nil, ErrParseFailure
	}
	// Take only the first line; multi-line input is not a command.
	if idx := strings.IndexByte(trimmed, '\n'); idx >= 0 {
		trimmed = trimmed[:idx]
	}
	trimmed = strings.TrimSpace(trimmed)
	if trimmed == "/" {
		return "", nil, ErrParseFailure
	}
	// Strip the leading slash.
	trimmed = trimmed[1:]
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return "", nil, ErrParseFailure
	}
	name = fields[0]
	if len(fields) > 1 {
		args = append(args, fields[1:]...)
	}
	return name, args, nil
}
