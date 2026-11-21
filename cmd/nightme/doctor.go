// Package main — `nightme doctor` subcommand.
//
// Checks the config file, reads the health of every running channel
// from the daemon via the daemoncontrol "health" RPC, then prints a
// human-readable status report — a CONFIG section and one section per
// channel. Designed to be the first stop when a user reports "消息nightme没收到"
// — it shows whether the config file is sane, whether the channel is
// connected, whether the link was ever verified, when the last
// inbound event arrived, and the most recent error if any.
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
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

// configRestartNote tells the user that the file they just fixed is
// not the file the running daemon reads.
const configRestartNote = "the running daemon keeps the config it loaded at start — restart it after fixing the file"

func newDoctorCmd() *cobra.Command {
	var jsonOut bool
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Diagnose the config file and the running daemon (per-channel link state, inbound/outbound liveness)",
		Long: `Print the health of every running channel and check the config file.

The report starts with a CONFIG section: the config file is parsed and
checked against the schema. A YAML syntax error, a value of the wrong
type and a key the schema does not know are all reported with the line
they are on. The check is read-only — doctor never writes the config
file. A config file with problems makes the command exit with code 2.

Then the daemon is asked for the health of every running channel
through its IPC socket (paths.Socket under cfg.Paths.DataDir). If the
daemon is not running, a DAEMON section says so.

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

--json prints the whole report as one object: the "config" and
"daemon" sections, and the "channels" array.

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

// runDoctor is the command's entry point: it checks the config file,
// asks the daemon for the health of every running channel and reports
// both.
//
// The config file is read, never written — a diagnostic command has no
// business changing the thing it diagnoses. That is why the config is
// loaded with config.Load and not with config.LoadDefault: the latter
// probes the built-in agents and writes the one it finds back into the
// file.
func runDoctor(out io.Writer, jsonOut bool) error {
	path := config.DefaultPath()
	report := config.Inspect(path)
	cfg, err := config.Load(path)
	if err != nil {
		// A config file that does not parse leaves nothing to
		// configure the daemon with — fall back to the defaults so
		// doctor can still look for a socket.
		cfg, err = config.Load("")
		if err != nil {
			return err
		}
	}
	paths, err := daemoncontrol.ResolvePaths(cfg.Paths.DataDir)
	if err != nil {
		return err
	}
	return runDoctorWith(out, jsonOut, report, cfg, paths, defaultDoctorRPCs())
}

// doctorRPCs are the daemoncontrol calls doctor makes. Tests swap in
// fakes so the RPC sequence can be asserted without a live daemon.
type doctorRPCs struct {
	health func(path string, timeout time.Duration) (daemoncontrol.HealthPayload, error)
	status func(path string, timeout time.Duration) (daemoncontrol.Status, error)
}

func defaultDoctorRPCs() doctorRPCs {
	return doctorRPCs{
		health: daemoncontrol.GetHealth,
		status: daemoncontrol.GetStatus,
	}
}

// runDoctorWith is the testable core of runDoctor: it takes the
// config check, the config, the resolved paths and the RPC seams
// instead of reaching for them itself.
func runDoctorWith(out io.Writer, jsonOut bool, report config.Report, cfg *config.Config, paths daemoncontrol.Paths, rpcs doctorRPCs) error {
	payload, err := rpcs.health(paths.Socket, 5*time.Second)
	if err != nil {
		// The daemon is unreachable — which is precisely when the
		// crash capture is worth reading. Print it BEFORE returning
		// the RPC error, otherwise the one command a user runs when
		// "the daemon is gone" tells them nothing about why.
		if jsonOut {
			if jerr := writeDoctorJSON(out, report, false, err.Error(), daemoncontrol.HealthPayload{}); jerr != nil {
				return jerr
			}
		} else {
			if werr := writeConfigSection(out, report); werr != nil {
				return werr
			}
			if werr := writeDaemonSection(out, err); werr != nil {
				return werr
			}
		}
		if summary, ok := crashCaptureSummary(cfg, time.Time{}); ok {
			fmt.Fprint(out, summary)
		}
		return doctorError(report, fmt.Errorf("daemon health RPC: %w", err))
	}
	if jsonOut {
		if err := writeDoctorJSON(out, report, true, "", payload); err != nil {
			return err
		}
		return doctorError(report, nil)
	}
	if err := writeDoctorText(out, report, payload); err != nil {
		return err
	}
	// A live daemon can still be the survivor of an earlier crash
	// loop, so surface the capture here too — a footer when it
	// predates the running daemon, an alarm when the running daemon
	// wrote it.
	startedAt := time.Time{}
	if path, info, ok := captureFile(cfg); ok {
		// The daemon's start time is what says which run wrote the
		// capture, and it is only worth a round trip when there is
		// a capture to attribute.
		if rpcs.status != nil {
			if status, err := rpcs.status(paths.Socket, 5*time.Second); err == nil {
				startedAt = status.StartedAt
			}
		}
		fmt.Fprint(out, renderCrashCapture(path, info, startedAt))
	}
	return doctorError(report, nil)
}

// doctorError is what the command ends with: the config problems
// first — they are the thing the user has to fix — and the daemon's
// absence second.
func doctorError(report config.Report, daemonErr error) error {
	if len(report.Diagnostics) > 0 {
		return nmerrors.New(nmerrors.CodeConfigError,
			fmt.Sprintf("doctor: the config file has %s", configStatus(report)))
	}
	return daemonErr
}

// captureFile returns the crash capture's path and file info when it
// exists and is not empty — the steady state has neither. One place
// decides what counts as a capture, so the section and the RPC that
// attributes it cannot drift apart.
func captureFile(cfg *config.Config) (string, os.FileInfo, bool) {
	path, err := daemonStderrPath(cfg)
	if err != nil {
		return "", nil, false
	}
	info, err := os.Stat(path)
	if err != nil || info.Size() == 0 {
		return "", nil, false
	}
	return path, info, true
}

// crashCaptureSummary renders the CRASH CAPTURE section, or ok=false
// when there is nothing to report (no file, or an empty one — the
// steady state).
//
// startedAt is the running daemon's start time; the zero value means
// "unknown" — the daemon is unreachable, or its status could not be
// read. The capture file is appended to and survives every restart,
// so only its mtime says which daemon run wrote it.
func crashCaptureSummary(cfg *config.Config, startedAt time.Time) (string, bool) {
	path, info, ok := captureFile(cfg)
	if !ok {
		return "", false
	}
	return renderCrashCapture(path, info, startedAt), true
}

// renderCrashCapture renders the section for one capture file.
func renderCrashCapture(path string, info os.FileInfo, startedAt time.Time) string {
	var sb strings.Builder
	tw := tabwriter.NewWriter(&sb, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "")
	fmt.Fprintln(tw, crashCaptureTitle(info.ModTime(), startedAt))
	fmt.Fprintf(tw, "  file:\t%s\n", path)
	fmt.Fprintf(tw, "  size:\t%d bytes\t(written %s ago)\n", info.Size(), formatAge(info.ModTime(), time.Now()))
	if head := crashHeadline(path); head != "" {
		fmt.Fprintf(tw, "  headline:\t%s\n", truncate(head, 100))
	}
	tw.Flush()
	return sb.String()
}

// crashCaptureTitle picks the section title from the capture's mtime
// against the running daemon's start time.
//
// The capture file is appended to and never cleared on a normal start,
// so "the file is not empty" says nothing about the daemon that is
// running now — an eleven-hour-old crash reads like a fresh one. The
// mtime does: an older capture belongs to an earlier run, a newer one
// was written by the daemon that is running now.
func crashCaptureTitle(capturedAt, startedAt time.Time) string {
	switch {
	case startedAt.IsZero():
		return "LAST CRASH CAPTURE (the last thing a daemon wrote to stderr)"
	case capturedAt.Before(startedAt):
		return fmt.Sprintf("LAST CRASH CAPTURE (from an earlier daemon run; the running one started at %s)",
			startedAt.Local().Format(time.RFC3339))
	default:
		return "CRASH CAPTURE (the running daemon wrote this — it should not write to stderr)"
	}
}

// crashScanBytes bounds how much of the capture file the headline
// scan reads. The file is rotated at 1 MiB, so this covers every
// realistic capture; a bigger one is scanned from its tail, where the
// most recent crash is.
const crashScanBytes = 4 << 20

// crashHeadline returns the last line that looks like a Go failure
// banner ("panic: ...", "fatal error: ..."), falling back to the
// first non-empty line.
//
// The capture file is appended to across restarts, so it can hold
// several crashes. The section's title describes the file's last
// write (its mtime), so the headline has to come from the same end
// of the file — a headline from the first crash would describe a
// different daemon run than the title does.
func crashHeadline(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()

	if info, err := f.Stat(); err == nil && info.Size() > crashScanBytes {
		if _, err := f.Seek(-crashScanBytes, io.SeekEnd); err != nil {
			return ""
		}
	}

	var first, last string
	sc := bufio.NewScanner(io.LimitReader(f, crashScanBytes))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "panic:") || strings.HasPrefix(line, "fatal error:") {
			last = line
			continue
		}
		if first == "" {
			first = line
		}
	}
	if last != "" {
		return last
	}
	// A scanner error here is almost certainly bufio.ErrTooLong: a
	// single panic line longer than the scan/token limit. The file
	// is non-empty (the caller already checked Size() > 0) but we
	// couldn't extract a headline — point the user at the file
	// rather than printing an empty headline that suggests the
	// capture is benign.
	if err := sc.Err(); err != nil && first == "" {
		return fmt.Sprintf("(headline unreadable: %v; see file)", err)
	}
	return first
}

// doctorJSON is the --json shape of the report.
//
// The channels stay at the top level: a consumer that reads
// `jq .channels[]` keeps working, and the two new sections are what
// the doctor knows when the daemon does not answer.
type doctorJSON struct {
	Config   doctorConfigJSON              `json:"config"`
	Daemon   doctorDaemonJSON              `json:"daemon"`
	Channels []daemoncontrol.ChannelHealth `json:"channels"`
}

// doctorConfigJSON is the state of the config file: where it is,
// whether it is healthy and every problem it has.
type doctorConfigJSON struct {
	Path        string              `json:"path"`
	Status      string              `json:"status"`
	Diagnostics []config.Diagnostic `json:"diagnostics"`
}

// doctorDaemonJSON says whether the daemon answered the health RPC.
type doctorDaemonJSON struct {
	Reachable bool   `json:"reachable"`
	Error     string `json:"error"`
}

// writeDoctorJSON pretty-prints the report. The Health field of
// every channel entry is a json.RawMessage; we don't round-trip decode
// so the user sees the field shape the daemon actually emitted.
func writeDoctorJSON(out io.Writer, report config.Report, reachable bool, daemonErr string, payload daemoncontrol.HealthPayload) error {
	channels := payload.Channels
	if channels == nil {
		channels = []daemoncontrol.ChannelHealth{}
	}
	diagnostics := report.Diagnostics
	if diagnostics == nil {
		diagnostics = []config.Diagnostic{}
	}
	doc := doctorJSON{
		Config: doctorConfigJSON{
			Path:        report.Path,
			Status:      configJSONStatus(report),
			Diagnostics: diagnostics,
		},
		Daemon:   doctorDaemonJSON{Reachable: reachable, Error: daemonErr},
		Channels: channels,
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	return enc.Encode(doc)
}

// writeDoctorText prints the report: the config check first, then one
// section per running channel. Every section is rendered from the
// channel's normalized health summary — the raw payload behind it is
// adapter-specific and is only available through --json.
func writeDoctorText(out io.Writer, report config.Report, payload daemoncontrol.HealthPayload) error {
	if err := writeConfigSection(out, report); err != nil {
		return err
	}
	fmt.Fprintln(out)

	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	if len(payload.Channels) == 0 {
		fmt.Fprintln(tw, "NO CHANNELS RUNNING")
		return tw.Flush()
	}

	now := time.Now()
	for i, entry := range payload.Channels {
		if i > 0 {
			fmt.Fprintln(tw, "")
		}
		fmt.Fprintf(tw, "CHANNEL %s\n", entry.Channel)
		summary, err := decodeHealthSummary(entry)
		if err != nil {
			// One channel with an unreadable summary must not hide
			// the others — report it and keep going.
			fmt.Fprintf(tw, "  health error:\t%s\n", err)
			continue
		}
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

// writeConfigSection prints the CONFIG section: the state of the
// config file and every problem it has.
func writeConfigSection(out io.Writer, report config.Report) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintf(tw, "CONFIG %s\n", report.Path)
	fmt.Fprintf(tw, "  status:\t%s\n", configStatus(report))
	for _, d := range report.Diagnostics {
		if label := diagnosticLabel(d); label != "" {
			fmt.Fprintf(tw, "  %s\t%s\n", label, d.Message)
			continue
		}
		// A problem without a place in the file has no line to
		// point at — the message says it all.
		fmt.Fprintf(tw, "  %s\n", d.Message)
	}
	if len(report.Diagnostics) > 0 {
		fmt.Fprintf(tw, "  note:\t%s\n", configRestartNote)
	}
	return tw.Flush()
}

// writeDaemonSection prints the DAEMON section: the daemon is not
// there, and why the doctor could not reach it.
func writeDaemonSection(out io.Writer, err error) error {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "")
	fmt.Fprintln(tw, "DAEMON")
	fmt.Fprintf(tw, "  status:\tunreachable\n")
	fmt.Fprintf(tw, "  error:\t%s\n", err)
	return tw.Flush()
}

// configStatus renders the state of the config file: a missing file
// is not a problem, and a file with problems says how many.
func configStatus(report config.Report) string {
	switch {
	case !report.Exists:
		return "not found"
	case len(report.Diagnostics) == 0:
		return "ok"
	case len(report.Diagnostics) == 1:
		return "1 problem"
	default:
		return fmt.Sprintf("%d problems", len(report.Diagnostics))
	}
}

// configJSONStatus renders the state of the config file for the JSON
// output: a fixed word, not the count the text output shows.
func configJSONStatus(report config.Report) string {
	switch {
	case !report.Exists:
		return "not found"
	case len(report.Diagnostics) == 0:
		return "ok"
	default:
		return "error"
	}
}

// diagnosticLabel renders where a problem is: "line 12:" or
// "line 12:5:" — the column is only there when the problem has one.
// A problem without a place in the file has no label.
func diagnosticLabel(d config.Diagnostic) string {
	switch {
	case d.Line <= 0:
		return ""
	case d.Column <= 0:
		return fmt.Sprintf("line %d:", d.Line)
	default:
		return fmt.Sprintf("line %d:%d:", d.Line, d.Column)
	}
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
