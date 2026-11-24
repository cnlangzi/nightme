package me

import (
	"regexp"
	"strings"
)

// The nightme commands the run may not issue.
//
// A command that ends, switches or resets the chat would take the run
// with it; a command that only shows something does not move the work
// on.
var excludedCommands = map[string]bool{
	"me":     true,
	"close":  true,
	"new":    true,
	"stop":   true,
	"cwd":    true,
	"use":    true,
	"resume": true,
	"help":   true,
	"think":  true,
	"tools":  true,
	"watch":  true,
	"format": true,
	"steer":  true,
	"wiki":   true,
}

// The subcommands the run may not issue.
//
// They shape the project instead of moving the work on, and the run
// has no say over the project's shape. The ones that tear the run's
// own workspace down, rewrite what it holds or end the command it
// waits for are off limits for the same reason: the run works on what
// it finds, it does not reshape it.
//
// `/gtw back` is not among them: it is the way out of a fix worktree
// the run is stuck in.
var excludedSubcommands = map[string]map[string]bool{
	"gtw": {
		"config": true,
		"plan":   true,
		"new":    true,
		"close":  true,
		"sync":   true,
		"abort":  true,
	},
}

// usableCommands returns the commands the run may issue: the ones the
// registry lists, minus the ones that are off limits.
func usableCommands(cmds []CommandInfo) []CommandInfo {
	out := make([]CommandInfo, 0, len(cmds))
	seen := map[string]bool{}
	for _, c := range cmds {
		name := strings.TrimSpace(c.Name)
		if name == "" || excludedCommands[name] {
			continue
		}
		tool := toolName(name)
		// Two commands whose names clean to the same tool would give
		// the model two tools of the same name: the first one wins.
		if tool == "" || seen[tool] {
			continue
		}
		seen[tool] = true
		c.Name = name
		c.Subcommands = usableSubcommands(name, c.Subcommands)
		out = append(out, c)
	}
	return out
}

// usableSubcommands returns the subcommands of a command the run may
// issue.
func usableSubcommands(command string, subs []CommandInfo) []CommandInfo {
	if len(subs) == 0 {
		return nil
	}
	excluded := excludedSubcommands[command]
	out := make([]CommandInfo, 0, len(subs))
	for _, s := range subs {
		name := strings.TrimSpace(s.Name)
		if name == "" || excluded[name] {
			continue
		}
		s.Name = name
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// commandNames returns the names of the commands.
func commandNames(cmds []CommandInfo) []string {
	out := make([]string, 0, len(cmds))
	for _, c := range cmds {
		out = append(out, c.Name)
	}
	return out
}

// invalidToolChars is every character a tool name may not carry.
var invalidToolChars = regexp.MustCompile(`[^a-zA-Z0-9_-]`)

// toolNameLimit is the longest name a tool may carry.
const toolNameLimit = 64

// toolName returns the tool name of a command: "ntm_" and the
// command's name, cleaned to the shape a tool name may take.
//
// An empty name means the command cannot be a tool.
func toolName(command string) string {
	clean := invalidToolChars.ReplaceAllString(command, "")
	if clean == "" {
		return ""
	}
	name := "ntm_" + clean
	if len(name) > toolNameLimit {
		name = name[:toolNameLimit]
	}
	return name
}
