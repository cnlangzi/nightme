// Package command (commander.go) — the Commander dispatch
// surface. Gateway.WithCommander receives a DispatchFunc (not
// the Commander interface — see F-51 doc §1.2.7); the runtime
// in cmd/nightme/run.go wraps the Commander with a thin shim
// that translates *gateway.InboundMessage to SlashInput and
// *SlashOutput back to *gateway.CommandResult. This package
// never imports internal/gateway.
//
// 2026-08-06: Commander.Dispatch gained a third return value
// `handled bool` so the caller can distinguish "was a slash
// command attempt" from "no slash command here". A slash-text
// input whose command name does not match any registered
// factory now reports handled=true, output.Consumed=false
// (the gateway falls through to the agent loop, preserving
// the pre-F-51 passthrough characteristic — see ADR 0007 §
// "What stays" and §"Open follow-ups" #3).
package command

import (
	"context"
	"strings"
)

// Commander is the slash command dispatch surface. Constructed
// at startup with a Registry; Dispatch routes by the first
// whitespace-separated token of input.Text to the registered
// factory.
type Commander interface {
	// Dispatch runs the slash command implied by input.Text.
	//
	// Returns (output, handled, err) where:
	//
	//   handled=false, output=nil: input.Text does not start
	//     with "/" (or starts with "/" but the command name
	//     is empty). The gateway treats this as a plain
	//     message and falls through to the agent loop.
	//
	//   handled=true, output={Consumed: false}: input.Text
	//     was a slash command attempt but the command name
	//     did not match any registered factory. The gateway
	//     forwards the original text to the agent loop so
	//     paths like "/etc/passwd" still reach the agent
	//     (preserves the v1.2.x passthrough characteristic).
	//
	//   handled=true, output={Consumed: true, Reply: "..."}:
	//     a registered command handled the input. The gateway
	//     sends output.Reply to the channel and does NOT
	//     forward to the agent loop.
	//
	//   err != nil: the registered command's Handle returned
	//     an error. The gateway reports the error as a reply
	//     (handled=true, output={Consumed: true, Reply: "❌ ..."}).
	Dispatch(ctx context.Context, rt RuntimeServices, input SlashInput) (*SlashOutput, bool, error)
}

// NewCommander constructs a Commander backed by reg. The
// returned Commander is safe for concurrent use.
func NewCommander(reg *Registry) Commander {
	return &commander{reg: reg}
}

type commander struct {
	reg *Registry
}

// Dispatch implements Commander.
//
// Parsing rules:
//   - Trim leading/trailing whitespace from input.Text.
//   - Must start with "/" to be considered a slash command.
//   - The first whitespace-delimited token is the command name
//     (e.g. "/gtw"); the rest is the trailing argv.
//   - If input.Args is empty (the runtime shim's default), the
//     commander builds Args = [cmdName, ...trailing] so that
//     factories can read input.Args[1:] to find the first
//     trailing token, input.Args[2:] for the second, etc. —
//     matching the convention documented in each Factory.Handle.
//   - The commander always parses from Text; if a future caller
//     pre-populates input.Args, the trailing-token fields are
//     NOT updated (the commander does not know whether the
//     pre-parsed argv matches the Text-parse argv).
func (c *commander) Dispatch(ctx context.Context, rt RuntimeServices, input SlashInput) (*SlashOutput, bool, error) {
	cmdName, trailingArgs, isCommand := c.extractCommand(input)
	if !isCommand {
		// Not a slash command at all (no "/" prefix, or just "/").
		// Fall through to the agent loop.
		return nil, false, nil
	}

	cmd := c.reg.FindByName(cmdName)
	if cmd == nil {
		// Slash command attempt with no matching factory. Preserve
		// the pre-F-51 passthrough — the original text reaches
		// the agent loop unchanged (so "/etc/passwd" and similar
		// path-like inputs are still useful). The dispatcher
		// treats handled=true + Consumed=false as a fall-through
		// signal.
		return &SlashOutput{Consumed: false}, true, nil
	}

	// Build [cmdName, ...trailing] when the caller did not pre-parse.
	// Currently only the empty-Args case is exercised (the runtime
	// shim sets Args=[]); the else-if branch is kept as a safety net
	// for a future caller that pre-parses one token and leaves room
	// for the commander to fill trailing.
	if len(input.Args) == 0 {
		input.Args = append([]string{cmdName}, trailingArgs...)
	} else if len(input.Args) == 1 && len(trailingArgs) > 0 {
		input.Args = append(input.Args, trailingArgs...)
	}

	out, err := cmd.Handle(ctx, rt, input)
	if err != nil {
		// The command itself errored — surface as a reply so the
		// user gets feedback. Still handled=true (we know what
		// they tried).
		return &SlashOutput{Consumed: true, Reply: "❌ " + err.Error()}, true, nil
	}
	return out, true, nil
}

// extractCommand pulls the command name + trailing args out of
// input. Returns (name, args, isSlashCommand). isSlashCommand
// is false when the text doesn't start with "/" — the caller
// should fall through to the agent loop.
func (c *commander) extractCommand(input SlashInput) (cmdName string, args []string, isSlashCommand bool) {
	text := strings.TrimSpace(input.Text)
	if text == "" || !strings.HasPrefix(text, "/") {
		return "", nil, false
	}
	// Tokenize on whitespace; tokens[0] has the leading "/".
	tokens := strings.Fields(text)
	if len(tokens) == 0 {
		return "", nil, false
	}
	rawName := strings.TrimPrefix(tokens[0], "/")
	if rawName == "" {
		return "", nil, false
	}
	return rawName, tokens[1:], true
}