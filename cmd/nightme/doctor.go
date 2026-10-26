// Package main — `nightme doctor` subcommand.
//
// Reads the health of every running channel from the daemon via the
// daemoncontrol "health" RPC, then prints a human-readable status
// report — one section per channel. Designed to be the first stop
// when a user reports "消息nightme没收到" — it shows whether the
// channel is connected, whether the link was ever verified, when the
// last inbound event arrived, and the most recent error if any.
//
// Usage:
//
//	nightme doctor             # human-readable text
//	nightme doctor --json      # raw JSON for piping to jq etc.
//
// Cross-platform: doctor only consumes the cross-platform
// daemoncontrol.GetHealth RPC (named pipe on Windows, AF_UNIX
// socket on Unix) and reads the daemon-stderr crash-capture file
// at <DataDir>/daemon-stderr.log. No Unix-only assumptions.
package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
)

func newDoctorCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the running daemon (per-channel link state, inbound/outbound liveness)",
		Long: `Print the health of every running channel. Connects to the running
daemon via its IPC socket (paths.Socket under cfg.Paths.DataDir) and
asks for the "health" RPC. If the daemon is not running, prints a
one-line error.

Each channel gets its own section:
  - status: connected, started (link not verified) or disconnected
  - link verified: whether a real remote round trip proved the
    credentials and the link. An adapter that was only started
    reports "no" and is never shown as connected.
  - when did the last inbound event arrive?
  - when did the last outbound send succeed?
  - what's the most recent error?

The channel-specific payload (WS lifecycle events, gateway session,
poll offset, …) is adapter-specific; --json prints it verbatim next
to the normalized summary.

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
	cfg, paths, err := loadLifecyclePaths()
	if err != nil {
		return err
	}
	payload, err := daemoncontrol.GetHealth(paths.Socket, 5*time.Second)
	if err != nil {
		// The daemon is unreachable — which is precisely when the
		// crash capture is worth reading. Print it BEFORE returning
		// the RPC error, otherwise the one command a user runs when
		// "the daemon is gone" tells them nothing about why.
		if summary, ok := crashCaptureSummary(cfg); ok {
			fmt.Fprint(out, summary)
		}
		return fmt.Errorf("daemon health RPC: %w", err)
	}
	if jsonOut {
		return writeDoctorJSON(out, payload)
	}
	if err := writeDoctorText(out, payload); err != nil {
		return err
	}
	// A live daemon can still be the survivor of an earlier crash
	// loop, so surface the capture here too — as a footer, not an
	// alarm.
	if summary, ok := crashCaptureSummary(cfg); ok {
		fmt.Fprint(out, summary)
	}
	return nil
}

// crashCaptureSummary renders the CRASH CAPTURE section, or ok=false
// when there is nothing to report (no file, or an empty one — the
// steady state). It shows the panic headline rather than the whole
// stack: enough to recognize the failure, with the path to read the
// rest.
func crashCaptureSummary(cfg *config.Config) (string, bool) {
	path, err := daemonStderrPath(cfg)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return "", false
	}

	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "")
	fmt.Fprintln(tw, "CRASH CAPTURE (daemon stderr — non-empty means it printed something it should not have)")
	fmt.Fprintf(tw, "  file:\t%s\n", path)
	fmt.Fprintf(tw, "  size:\t%d bytes\t(written %s ago)\n", info.Size(), formatAge(info.ModTime(), time.Now()))
	if head := crashHeadline(path); head != "" {
		fmt.Fprintf(tw, "  headline:\t%s\n", truncate(head, 100))
	}
	tw.Flush()
	return sb.String(), true
}

// crashHeadline returns the first line that looks like a Go failure
// banner ("panic: ...", "fatal error: ..."), falling back to the
// first non-empty line. Reading a bounded prefix keeps `doctor` cheap
// even if the file grew large before rotation.
func crashHeadline(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	var first string
	sc := bufio.NewScanner(io.LimitReader(f, 64<<10))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "fatal error:") {
			return line
		}
		if first == "" {
			first = line
		}
	}
	// A scanner error here is almost certainly bufio.ErrTooLong: a
	// single panic line longer than the 64KB scan/token limit. The
	// file is non-empty (the caller already checked Size() > 0) but
	// we couldn't extract a headline — point the user at the file
	// rather than printing an empty headline that suggests the
	// capture is benign.
	if err := sc.Err(); err != nil && first == "" {
		return fmt.Sprintf("(headline unreadable: %v; see file)", err)
	}
	return first
}

// writeDoctorJSON pretty-prints the payload. The Health field of
// every entry is a json.RawMessage; we don't round-trip decode so the
// user sees the field shape the daemon actually emitted.
func writeDoctorJSON(out io.Writer, payload daemoncontrol.HealthPayload) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

// writeDoctorText prints one section per running channel. Every
// section is rendered from the channel's normalized health summary —
// the raw payload behind it is adapter-specific and is only available
// through --json.
func writeDoctorText(out io.Writer, payload daemoncontrol.HealthPayload) error {
	if len(payload.Channels) == 0 {
		fmt.Fprintln(out, "NO CHANNELS RUNNING")
		return nil
	}

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	now := time.Now()
	for i, entry := range payload.Channels {
		if i > 0 {
			fmt.Fprintln(tw, "")
		}
		summary, err := decodeHealthSummary(entry)
		if err != nil {
			return err
		}
		fmt.Fprintf(tw, "CHANNEL %s\n", entry.Channel)
		fmt.Fprintf(tw, "  status:\t%s\n", channelStatus(summary))
		fmt.Fprintf(tw, "  link verified:\t%s\n", yesNo(summary.LinkVerified))
		fmt.Fprintf(tw, "  last inbound:\t%s\n", formatTimestamp(summary.LastInboundAt, now))
		fmt.Fprintf(tw, "  last outbound:\t%s\n", formatTimestamp(summary.LastOutboundAt, now))
		if summary.LastError != "" {
			fmt.Fprintf(tw, "  last error:\t%s\n", summary.LastError)
		}
		if entry.Error != "" {
			fmt.Fprintf(tw, "  health error:\t%s\n", entry.Error)
		}
	}
	return tw.Flush()
}

// decodeHealthSummary decodes one channel's normalized summary. A
// channel whose summary is missing still gets a section — the
// zero summary reads as "not connected, never verified".
func decodeHealthSummary(entry daemoncontrol.ChannelHealth) (channel.HealthSummary, error) {
	var summary channel.HealthSummary
	if len(entry.Summary) == 0 {
		return summary, nil
	}
	if err := json.Unmarshal(entry.Summary, &summary); err != nil {
		return summary, fmt.Errorf("decode health summary for %s: %w", entry.Channel, err)
	}
	return summary, nil
}

// channelStatus renders the connection state. An adapter that was
// started but never verified its link must not read as connected.
func channelStatus(summary channel.HealthSummary) string {
	switch {
	case summary.Connected && summary.LinkVerified:
		return "connected"
	case summary.Connected:
		return "started (link not verified)"
	default:
		return "disconnected"
	}
}

// yesNo renders a boolean as yes / no.
func yesNo(v bool) string {
	if v {
		return "yes"
	}
	return "no"
}

// formatTimestamp renders a timestamp as an age, or "never" when the
// adapter never recorded one.
func formatTimestamp(t, now time.Time) string {
	if t.IsZero() {
		return "never"
	}
	return formatAge(t, now) + " ago"
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
