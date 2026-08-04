// Package main — `nightme doctor` subcommand.
//
// Reads the live WebSocket lifecycle state from the running daemon
// via the daemoncontrol "health" RPC, then prints a human-readable
// status report. Designed to be the first stop when a user reports
// "feishu消息nightme没收到" — it shows whether the WS is connected,
// when the last inbound event arrived, recent reconnect attempts,
// and the most recent error if any.
//
// Usage:
//
//	nightme doctor             # human-readable text
//	nightme doctor --json      # raw JSON for piping to jq etc.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/channel/feishu"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
)

func newDoctorCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the running daemon (WS state, reconnect health, inbound/outbound liveness)",
		Long: `Print the current Feishu WebSocket connection state plus recent
lifecycle events. Connects to the running daemon via its IPC socket
(paths.Socket under cfg.Paths.DataDir) and asks for the "health"
RPC. If the daemon is not running, prints a one-line error.

Useful first-stop diagnostic for "feishu消息没收到" reports:
  - WebSocket is connected?
  - When did the last inbound event arrive?
  - When did the last outbound send succeed?
  - How many reconnects in this session?
  - What's the most recent error?

Examples:
  nightme doctor
  nightme doctor --json`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runDoctor(cmd.OutOrStdout(), jsonOut)
		},
	}
	cmd.Flags().BoolVar(&jsonOut, "json", false, "print raw JSON snapshot instead of formatted text")
	return cmd
}

func runDoctor(out io.Writer, jsonOut bool) error {
	_, paths, err := loadLifecyclePaths()
	if err != nil {
		return err
	}
	payload, err := daemoncontrol.GetHealth(paths.Socket, 5*time.Second)
	if err != nil {
		return fmt.Errorf("daemon health RPC: %w", err)
	}
	if jsonOut {
		return writeDoctorJSON(out, payload)
	}
	return writeDoctorText(out, payload)
}

// writeDoctorJSON pretty-prints the raw payload. The Health field
// is a json.RawMessage; we don't round-trip decode so the user sees
// the field shape the daemon actually emitted.
func writeDoctorJSON(out io.Writer, payload daemoncontrol.HealthPayload) error {
	envelope := map[string]any{
		"channel": payload.Channel,
		"health":  json.RawMessage(payload.Health),
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(envelope)
}

// writeDoctorText prints a human-readable multi-line status with
// aligned columns. Lifted from cc-connect's health-display pattern
// (one section per concern, blank line between sections).
func writeDoctorText(out io.Writer, payload daemoncontrol.HealthPayload) error {
	var snap feishu.WSHealthSnapshot
	if err := json.Unmarshal(payload.Health, &snap); err != nil {
		return fmt.Errorf("decode health snapshot: %w", err)
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)

	// Section 1: connection state
	fmt.Fprintln(tw, "STATUS")
	fmt.Fprintf(tw, "  channel:\t%s\n", payload.Channel)
	if snap.Connected {
		fmt.Fprintf(tw, "  connected:\t%s (since %s)\n", "yes", formatAge(snap.LastConnectedAt, time.Now()))
	} else {
		fmt.Fprintf(tw, "  connected:\t%s (since %s)\n", "NO", formatAge(snap.LastDisconnectedAt, time.Now()))
	}
	fmt.Fprintf(tw, "  reconnects (this session):\t%d\n", snap.ReconnectCount)

	// Section 2: liveness
	fmt.Fprintln(tw, "")
	fmt.Fprintln(tw, "LIVENESS")
	now := time.Now()
	if !snap.LastInboundAt.IsZero() {
		fmt.Fprintf(tw, "  last inbound:\t%s ago\t(chat=%s)\n", formatAge(snap.LastInboundAt, now), snap.LastInboundChatID)
	} else {
		fmt.Fprintln(tw, "  last inbound:\tnever")
	}
	if !snap.LastOutboundAt.IsZero() {
		fmt.Fprintf(tw, "  last outbound:\t%s ago\n", formatAge(snap.LastOutboundAt, now))
	} else {
		fmt.Fprintln(tw, "  last outbound:\tnever")
	}

	// Section 3: last error
	if snap.LastError != "" {
		fmt.Fprintln(tw, "")
		fmt.Fprintln(tw, "LAST ERROR")
		fmt.Fprintf(tw, "  %s\tat %s\n", snap.LastError, snap.LastErrorAt.Format(time.RFC3339))
	}

	// Section 3.5: F-41 active-reconnect prober
	// Always printed (even when inactive) so the user knows the
	// feature exists. When active, prints the force-restart count
	// and last attempt timestamp.
	fmt.Fprintln(tw, "")
	fmt.Fprintln(tw, "PROBER (F-41 active reconnect)")
	if snap.Prober.Active {
		fmt.Fprintf(tw, "  active:\t%s\n", "yes")
	} else {
		fmt.Fprintf(tw, "  active:\t%s\n", "no")
	}
	fmt.Fprintf(tw, "  interval:\t%s\n", snap.Prober.Interval)
	fmt.Fprintf(tw, "  force_attempts:\t%d\n", snap.Prober.ForceCount)
	if !snap.Prober.LastForceAt.IsZero() {
		fmt.Fprintf(tw, "  last_force_at:\t%s ago\n", formatAge(snap.Prober.LastForceAt, now))
	} else {
		fmt.Fprintln(tw, "  last_force_at:\tnever")
	}
	if !snap.Prober.StartedAt.IsZero() {
		fmt.Fprintf(tw, "  started_at:\t%s ago\n", formatAge(snap.Prober.StartedAt, now))
	}
	if snap.Prober.LastError != "" {
		fmt.Fprintf(tw, "  last_error:\t%s\n", snap.Prober.LastError)
	}

	// Section 4: event ring (most recent first)
	if len(snap.EventRing) > 0 {
		fmt.Fprintln(tw, "")
		fmt.Fprintln(tw, "RECENT EVENTS (newest first)")
		// reverse for display
		for i := len(snap.EventRing) - 1; i >= 0; i-- {
			ev := snap.EventRing[i]
			msg := ev.Message
			if msg == "" {
				msg = "-"
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\n", ev.At.Format("15:04:05"), ev.Kind, truncate(msg, 80))
		}
	}

	// Section 5: inbound ring
	if len(snap.InboundRing) > 0 {
		fmt.Fprintln(tw, "")
		fmt.Fprintln(tw, "RECENT INBOUND")
		for i := len(snap.InboundRing) - 1; i >= 0; i-- {
			s := snap.InboundRing[i]
			fmt.Fprintf(tw, "  %s\t%s\tchat=%s\n", s.At.Format("15:04:05"), s.Kind, s.ChatID)
		}
	}

	// Section 6: outbound ring
	if len(snap.OutboundRing) > 0 {
		fmt.Fprintln(tw, "")
		fmt.Fprintln(tw, "RECENT OUTBOUND")
		for i := len(snap.OutboundRing) - 1; i >= 0; i-- {
			s := snap.OutboundRing[i]
			fmt.Fprintf(tw, "  %s\t%s\tchat=%s\n", s.At.Format("15:04:05"), s.Kind, s.ChatID)
		}
	}

	return tw.Flush()
}

// formatAge returns "5m12s" / "2h03m" / "30s ago" — relative to now.
// Negative durations (clock skew) clamp to "0s".
func formatAge(t, now time.Time) string {
	if t.IsZero() {
		return "-"
	}
	d := now.Sub(t)
	if d < 0 {
		d = 0
	}
	return d.Round(time.Second).String()
}

// truncate trims a string to max runes, appending an ellipsis if cut.
func truncate(s string, max int) string {
	if max <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max-1]) + "…"
}
