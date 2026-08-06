// Package command (commander.go) — the Commander dispatch
// surface. Gateway.WithCommander receives a DispatchFunc (not
// the Commander interface — see F-51 doc §1.2.7); the runtime
// in cmd/nightme/run.go wraps the Commander with a thin shim
// that translates *gateway.InboundMessage to SlashInput and
// *SlashOutput back to *gateway.CommandResult. This package
// never imports internal/gateway.
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
	// Returns (SlashOutput{Consumed: false}, nil) when:
	//   - input.Text does not start with "/"
	//   - the command name is empty after the "/"
	//   - the command name does not match any registered factory
	// (all three are fall-through to the agent loop).
	//
	// Empty / unknown command: replies with a usage hint
	// (SlashOutput.Consumed = true) when at least the leading
	// "/" was present, so the user gets feedback. Without the
	// "/" prefix, fall through silently.
	Dispatch(ctx context.Context, rt RuntimeServices, input SlashInput) (*SlashOutput, error)
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
//     (e.g. "/gtw" or "/gtw"); the rest is passed through as
//     input.Args[1:] (input.Args[0] is the bare command name
//     if the gateway pre-parsed; the commander does NOT
//     re-parse if Args is already populated).
//   - If Args is non-empty, use Args[0] as the command name.
//   - Otherwise split Text on whitespace and take element 0,
//     strip the leading "/".
func (c *commander) Dispatch(ctx context.Context, rt RuntimeServices, input SlashInput) (*SlashOutput, error) {
	cmdName, args, isCommand := c.extractCommand(input)
	if !isCommand {
		return &SlashOutput{Consumed: false}, nil
	}

	cmd := c.reg.FindByName(cmdName)
	if cmd == nil {
		return unknownCommandReply(cmdName, c.reg), nil
	}

	// Make sure Args is populated; if the gateway didn't
	// pre-parse, fill it from the post-name text.
	if len(input.Args) == 0 {
		input.Args = append([]string{cmdName}, args...)
	} else if len(input.Args) == 1 && len(args) > 0 {
		// Gateway parsed but the runtime wants the trailing
		// args — append them.
		input.Args = append(input.Args, args...)
	}

	return cmd.Handle(ctx, rt, input)
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

// unknownCommandReply builds a "command not found" reply with
// a hint listing the registered command names. SlashOutput is
// non-nil with Consumed=true so the user sees feedback; the
// gateway does NOT forward to the agent loop.
func unknownCommandReply(name string, reg *Registry) *SlashOutput {
	helpLine := ""
	if specs := reg.Specs(); len(specs) > 0 {
		names := make([]string, 0, len(specs))
		for _, s := range specs {
			names = append(names, s.Name)
		}
		helpLine = " (known: " + strings.Join(names, ", ") + ")"
	}
	return &SlashOutput{
		Reply:    "Unknown command: /" + name + helpLine + "\nSend /help for usage.",
		Consumed: true,
	}
}
