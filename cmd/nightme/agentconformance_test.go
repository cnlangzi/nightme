package main

import (
	"context"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
)

// This file holds the registry-driven conformance suite: the
// contract every registered agent has to satisfy, checked for
// every starter in agent.Builtins.
//
// The list of agents is derived from the registry, so an agent
// added to cmd/nightme/agents.go is covered the moment it is
// registered — nothing in this file needs editing. A per-bridge
// copy of these checks would drift, and a check that only one
// bridge opted into covers only that bridge.
//
// Lives in cmd/nightme because that is where the built-ins
// register themselves via init() (see agentregistry_test.go for
// the same reasoning).
//
// The driver-interface half of the contract is enforced by the
// compiler, not here: agent.NewAgent's type parameter rejects a
// driver that does not implement the package-private agent.driver
// interface, at every call site in every bridge. That check runs
// in CI on every platform with no binary installed. What is left
// for a runtime suite is what the compiler cannot see.

// knownModes is the set of transports the agent package defines.
// A bridge whose Info() reports anything else is misreporting how
// it talks to its CLI.
var knownModes = map[agent.Mode]bool{
	agent.ModeACP:    true,
	agent.ModeSDK:    true,
	agent.ModePTY:    true,
	agent.ModeJSONIO: true,
}

// workingPathTimeout bounds one agent's start. An agent whose CLI
// needs setup this machine does not have (auth, a first-run
// wizard) burns the whole budget and is skipped rather than
// failed — the bridge is not what is missing there.
const workingPathTimeout = 30 * time.Second

// sharedHostAgents are the bridges that do not spawn the configured
// command — they talk to a host process that is already running.
// They have no "working path" to drive from here, and starting them
// would open real sessions on the user's host for no assertion.
var sharedHostAgents = map[string]bool{
	"dsh": true,
}

// TestRegisteredAgents_InfoContract checks the static metadata
// contract for every registered agent: a non-empty unique name, a
// command to run, a known transport, and defensively copied argv /
// env so a caller mutating the returned slices cannot corrupt the
// starter for the next caller.
func TestRegisteredAgents_InfoContract(t *testing.T) {
	starters := agent.Builtins.List()
	if len(starters) == 0 {
		t.Fatal("no agents registered in agent.Builtins")
	}

	seen := map[string]bool{}
	for _, s := range starters {
		name := s.Info().Name
		if name == "" {
			t.Errorf("registered agent with empty name")
			continue
		}
		if seen[name] {
			t.Errorf("agent %q registered twice", name)
		}
		seen[name] = true

		info := s.Info()
		if info.Command == "" {
			t.Errorf("agent %q: Info().Command is empty", name)
		}
		if !knownModes[info.Mode] {
			t.Errorf("agent %q: Info().Mode = %v, want a known transport", name, info.Mode)
		}

		// Info() documents that Args / Env are defensively copied
		// on every call. Mutating the returned slice must not
		// reach the starter's own storage.
		args := s.Info().Args
		if len(args) > 0 {
			args[0] = "nightme-mutated"
			if got := s.Info().Args[0]; got == "nightme-mutated" {
				t.Errorf("agent %q: Info().Args is not defensively copied", name)
			}
		}
		env := s.Info().Env
		if len(env) > 0 {
			env[0] = "NIGHTME_MUTATED=1"
			if got := s.Info().Env[0]; got == "NIGHTME_MUTATED=1" {
				t.Errorf("agent %q: Info().Env is not defensively copied", name)
			}
		}
	}
}

// TestRegisteredAgents_DetectContract checks that Detect agrees
// with the command it was given: it succeeds exactly when the
// command resolves. Detect is the gate /run puts in front of a
// spawn, so an agent that reports a missing binary as available
// (or the reverse) sends the user into a spawn that cannot work.
func TestRegisteredAgents_DetectContract(t *testing.T) {
	for _, s := range agent.Builtins.List() {
		s := s
		t.Run(s.Info().Name, func(t *testing.T) {
			t.Parallel()
			resolveErr := agent.ResolveCommand(s.Info().Command)
			detectErr := s.Detect()
			if resolveErr == nil && detectErr != nil {
				t.Errorf("Detect() = %v, want nil: the command %q resolves", detectErr, s.Info().Command)
			}
			if resolveErr != nil && detectErr == nil {
				t.Errorf("Detect() = nil, want an error: the command %q does not resolve", s.Info().Command)
			}
		})
	}
}

// TestRegisteredAgents_CommandIsOverridable checks that every
// registered starter honours the Init override the config layer
// applies. cfg.Agents lets a user point an agent at a different
// binary; a starter that ignores Init silently keeps using the
// built-in command.
func TestRegisteredAgents_CommandIsOverridable(t *testing.T) {
	snapshotBuiltinCommands(t)
	bin := fakeBinary(t, t.TempDir(), "agent-conformance-override")

	for _, s := range agent.Builtins.List() {
		s := s
		t.Run(s.Info().Name, func(t *testing.T) {
			t.Parallel()
			c, ok := s.(commandHolder)
			if !ok {
				t.Skipf("starter does not expose Command/Init")
			}
			original := c.Command()
			c.Init(bin)
			if got := c.Command(); got != bin {
				t.Errorf("Command() = %q, want the override %q", got, bin)
			}
			c.Init("")
			if got := c.Command(); got != original {
				t.Errorf("Command() after Init(\"\") = %q, want the original %q", got, original)
			}
		})
	}
}

// TestRegisteredAgents_WorkingPath starts every installed agent
// and checks that the spawn path behaves.
//
// An agent whose CLI is not installed is skipped — the point is to
// exercise the bridge, and a missing binary is an environment fact
// rather than a bridge defect. An agent whose CLI is installed but
// cannot complete its handshake (auth, first-run setup) is skipped
// for the same reason.
//
// What must never happen is a panic: a panic on the spawn path
// unwinds past the child process, leaks it, and leaves the chat
// turn without a terminal event, which the channel renders as a
// permanent "Working" card with no error to show.
//
// The subtests run one at a time. Seven agents spawning their CLIs
// at once starves each other's handshake — the agent's own
// handshake budget then expires for the wrong reason.
func TestRegisteredAgents_WorkingPath(t *testing.T) {
	snapshotBuiltinCommands(t)

	for _, s := range agent.Builtins.List() {
		s := s
		t.Run(s.Info().Name, func(t *testing.T) {
			if sharedHostAgents[s.Info().Name] {
				t.Skipf("shared-host bridge: no subprocess of its own to start")
			}
			if err := s.Detect(); err != nil {
				t.Skipf("agent not installed on this machine: %v", err)
			}

			ctx, cancel := context.WithTimeout(context.Background(), workingPathTimeout)
			defer cancel()

			ag, err := s.Start(ctx, agent.StartConfig{Workspace: t.TempDir()})
			if err != nil {
				t.Skipf("Start() = %v: the CLI could not complete its handshake here", err)
			}
			t.Cleanup(func() { _ = ag.Close() })

			if !agent.ImplementsDriver(ag.Driver()) {
				t.Errorf("driver %T does not satisfy agent.driver", ag.Driver())
			}
			if ag.PID() <= 0 {
				t.Errorf("PID() = %d, want a live child pid", ag.PID())
			}
		})
	}
}
