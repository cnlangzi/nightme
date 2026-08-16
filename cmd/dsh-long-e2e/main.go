// dsh_long_run.go — runs a real dsh for [duration] minutes, watching
// events. Verifies the R1.5 fix keeps the bridge alive across respawn.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"sync/atomic"
	"time"

	"github.com/cnlangzi/nightme/internal/agent"
	"github.com/cnlangzi/nightme/internal/agentsession"
	"github.com/cnlangzi/nightme/internal/bridge/dsh"
	"github.com/cnlangzi/nightme/internal/registry"
)

func main() {
	durationFlag := flag.Duration("duration", 15*time.Minute, "watch duration")
	flag.Parse()

	fmt.Printf("=== dsh long run (R1.5 verify) ===\n")
	fmt.Printf("duration: %s\n", *durationFlag)

	workspace, _ := os.MkdirTemp("", "dsh-long-*")
	defer os.RemoveAll(workspace)

	starter := dsh.NewStarter("dsh")
	a, err := starter.Start(context.Background(), agent.StartConfig{Workspace: workspace})
	if err != nil {
		log.Fatalf("Start: %v", err)
	}
	defer a.Close()
	fmt.Printf("dsh PID=%d\n", a.PID())

	// Wait for first session
	deadline := time.NewTimer(15 * time.Second)
	var sessionID string
loop:
	for {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				log.Fatalf("events channel closed before sessionID")
			}
			if ev.Kind == agent.EventAgentReady && ev.SessionID != "" {
				sessionID = ev.SessionID
				break loop
			}
		case <-deadline.C:
			log.Fatalf("no EventAgentReady in 15s")
		}
	}
	fmt.Printf("sessionID: %s\n", sessionID)

	// Build AS and Submit
	as := agentsession.NewAgentSession("as_long", "cs_long", "dsh", workspace, nil)
	as.SetSessionID(sessionID)
	as.SetPersist(func(_ *registry.AgentSessionEntry) error { return nil })
	as.SetRunning(a.PID())

	if err := a.SendBlocks(context.Background(), []agent.ContentBlock{
		{Type: agent.ContentText, Text: "reply with one word: PONG"},
	}); err != nil {
		log.Fatalf("SendBlocks: %v", err)
	}
	fmt.Println("prompt submitted")

	// Watch loop
	var eventCount atomic.Int64
	var doneSeen atomic.Bool
	deadlineTimer := time.After(*durationFlag)
	watchTicker := time.NewTicker(30 * time.Second)
	defer watchTicker.Stop()

eventLoop:
	for {
		select {
		case ev, ok := <-a.Events():
			if !ok {
				fmt.Println("events channel closed (bridge died)")
				break eventLoop
			}
			n := eventCount.Add(1)
			if ev.Kind == agent.EventAgentDone {
				doneSeen.Store(true)
				fmt.Printf("[%s] event %d: DONE\n", time.Now().Format("15:04:05"), n)
			} else if ev.Kind == agent.EventAgentError {
				fmt.Printf("[%s] event %d: ERROR: %v\n", time.Now().Format("15:04:05"), n, ev.Err)
			}
		case <-watchTicker.C:
			fmt.Printf("[%s] alive: events=%d done=%v\n",
				time.Now().Format("15:04:05"), eventCount.Load(), doneSeen.Load())
		case <-deadlineTimer:
			fmt.Printf("deadline reached (duration=%s)\n", *durationFlag)
			break eventLoop
		}
	}

	fmt.Printf("\n=== final ===\n")
	fmt.Printf("events: %d\n", eventCount.Load())
	fmt.Printf("done: %v\n", doneSeen.Load())
	_ = sessionID
	_ = as
}
