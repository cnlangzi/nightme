package wiki

import "github.com/cnlangzi/nightme/internal/command"

// wikiArgs bundles the parsed argv tail for `/wiki`. After
// the init/update merge, `/wiki` is a single top-level
// command with no subcommands — the argv tail is the flag
// list directly.
type wikiArgs struct {
	// Agent is the optional LLM agent name, from
	// `-a <name>` / `--agent <name>`. Empty means "stub mode"
	// (the current behaviour — no LLM is invoked). The future
	// LLM-driven path will resolve this through the same
	// three-tier precedence as /gtw commit (CLI > yml > session).
	Agent string
}

// parseWikiArgs implements the CLI lexer for `/wiki`.
//
// Recognised flags:
//
//	-a / --agent <name>   override the LLM agent for this run
//	                      (reserved for the future LLM path;
//	                      currently accepted and ignored)
//
// Unknown flags and stray positionals are hard-rejected with
// the standard "unknown flag" / arity wording from
// command.ParseCmdArgs (issue #291 contract).
func parseWikiArgs(argv []string) (wikiArgs, error) {
	parsed, err := command.ParseCmdArgs(argv, command.CmdSpec{
		Name:  "/wiki",
		Usage: "/wiki [-a <agent>]",
		Flags: map[string]command.FlagSpec{
			"-a":      {Name: "agent", TakesValue: true},
			"--agent": {Name: "agent", TakesValue: true},
		},
		MinArgs: 0,
		MaxArgs: 0,
	})
	if err != nil {
		return wikiArgs{}, err
	}
	return wikiArgs{Agent: parsed.Value("agent")}, nil
}
