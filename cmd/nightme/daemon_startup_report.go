// Package main — the daemon startup report.
//
// `nightme start` forks a child whose stdout goes to /dev/null (Unix)
// or NUL (Windows), so nothing the operator needs to know can be
// learned from the child's own prints. Unix hands a single boolean
// and a top-level error back over the inherited readiness pipe;
// Windows has no such channel at all, which is why every Windows
// startup failure reads the same: "daemon did not become ready
// within 15s". A per-channel outcome is not one boolean anyway.
//
// The child therefore publishes a structured report to
// <DataDir>/daemon-startup.json and the parent reads it back on both
// the success and the failure path:
//
//   - success — print one line per channel, so the operator sees
//     which platforms actually came up instead of assuming all did;
//   - failure — fold the recorded reason into the error, so Windows
//     stops collapsing every cause into one sentence.
//
// PID guards against a file left behind by a daemon that was killed
// partway through startup: the parent only trusts a report written
// by the process it just spawned.
//
// The file is rewritten atomically (temp + rename) after every
// transition because the parent polls it concurrently.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"github.com/cnlangzi/nightme/internal/daemoncontrol"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
	"github.com/cnlangzi/nightme/internal/runtime"
)

// daemonStartupReportName is the report file inside the data
// directory, alongside daemon-stderr.log.
const daemonStartupReportName = "daemon-startup.json"

// startupReportWait bounds how long `nightme start` keeps polling for
// the daemon's channel results after readiness was already
// confirmed. Readiness does not depend on any channel, so running out
// only means the report prints incomplete; the start still succeeds.
//
// It does have to outlast the daemon's whole channel phase, which is
// one runtime.ChannelStartBudget because channels start concurrently.
// TestStartupReportWaitOutlastsChannelStartBudget pins that.
const startupReportWait = 15 * time.Second

// startupReportPollInterval is how often the parent re-reads the
// report while waiting for the channel phase to finish.
const startupReportPollInterval = 50 * time.Millisecond

type startupChannelReport struct {
	Channel string `json:"channel"`
	Status  string `json:"status"`
	Error   string `json:"error,omitempty"`
}

type startupReport struct {
	PID int `json:"pid"`

	// Ready mirrors the readiness handshake: local resources are
	// loaded and the control socket is serving.
	Ready bool `json:"ready"`

	// Error is the reason the child failed before readiness. Empty
	// on the success path.
	Error string `json:"error,omitempty"`

	// ChannelsDone separates "the child reported no channel" from
	// "the child has not got round to channels yet" — the report is
	// written once at startup and once at readiness, long before the
	// channel loop finishes.
	ChannelsDone bool                   `json:"channels_done"`
	Channels     []startupChannelReport `json:"channels,omitempty"`
}

// startupReportPath is the report's location in the daemon's data
// directory. Both the parent and the child derive it from the same
// daemoncontrol.ResolvePaths result, so the two always agree.
func startupReportPath(paths daemoncontrol.Paths) string {
	return filepath.Join(paths.Dir, daemonStartupReportName)
}

// startupReportWriter is the child's side of the report. Every
// mutator rewrites the whole file atomically so a concurrent reader
// never observes a half-written JSON document.
type startupReportWriter struct {
	path string

	rep    startupReport
	warned bool
}

func newStartupReportWriter(path string) *startupReportWriter {
	w := &startupReportWriter{path: path}
	w.rep.PID = os.Getpid()
	w.flush()
	return w
}

func (w *startupReportWriter) SetReady() {
	w.rep.Ready = true
	w.flush()
}

func (w *startupReportWriter) SetError(msg string) {
	w.rep.Error = msg
	w.flush()
}

func (w *startupReportWriter) SetChannels(startup runtime.ChannelStartup) {
	w.rep.ChannelsDone = true
	w.rep.Channels = w.rep.Channels[:0]
	for _, r := range startup.Results {
		w.rep.Channels = append(w.rep.Channels, startupChannelReport{
			Channel: r.Name,
			Status:  r.Status,
			Error:   r.Error,
		})
	}
	w.flush()
}

// flush writes the current report. A failure is warned about once
// and then ignored: the report is a diagnostic aid, and a data
// directory that cannot hold it must not stop the daemon.
func (w *startupReportWriter) flush() {
	data, err := json.Marshal(w.rep)
	if err != nil {
		return
	}
	dir := filepath.Dir(w.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		w.warnOnce(err)
		return
	}
	tmp, err := os.CreateTemp(dir, ".daemon-startup-*.json.tmp")
	if err != nil {
		w.warnOnce(err)
		return
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		w.warnOnce(err)
		return
	}
	if err := tmp.Close(); err != nil {
		w.warnOnce(err)
		return
	}
	if err := os.Rename(tmpName, w.path); err != nil {
		w.warnOnce(err)
	}
}

func (w *startupReportWriter) warnOnce(err error) {
	if w.warned {
		return
	}
	w.warned = true
	slog.Default().Warn("daemon startup report disabled",
		"path", w.path, "err", err)
}

// readStartupReport returns the report written by pid, or ok=false
// when there is none, it is unreadable, or it belongs to another
// process (a daemon killed mid-startup leaves its report behind).
func readStartupReport(path string, pid int) (startupReport, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return startupReport{}, false
	}
	var rep startupReport
	if err := json.Unmarshal(data, &rep); err != nil {
		return startupReport{}, false
	}
	if rep.PID != pid {
		return startupReport{}, false
	}
	return rep, true
}

// waitStartupChannels polls until the child has published its
// channel outcomes, or timeout elapses. Returns the last readable
// report either way — a partial report still names the channels that
// did come up.
func waitStartupChannels(paths daemoncontrol.Paths, pid int, timeout time.Duration) startupReport {
	path := startupReportPath(paths)
	deadline := time.Now().Add(timeout)
	for {
		rep, ok := readStartupReport(path, pid)
		if ok {
			if rep.ChannelsDone || !time.Now().Before(deadline) {
				return rep
			}
		} else if !time.Now().Before(deadline) {
			return startupReport{}
		}
		time.Sleep(startupReportPollInterval)
	}
}

// printStartupChannels writes one line per channel the daemon tried
// to start, with the reason for every one that did not make it.
func printStartupChannels(out io.Writer, rep startupReport) {
	for _, ch := range rep.Channels {
		if ch.Error == "" {
			fmt.Fprintf(out, "  channel %s: %s\n", ch.Channel, ch.Status)
			continue
		}
		fmt.Fprintf(out, "  channel %s: %s (%s)\n", ch.Channel, ch.Status, ch.Error)
	}
}

// startupFailureReason returns the reason the child recorded for
// failing before readiness, or "" when it recorded none. Used on
// every failure path — including the timeout, where the child is
// still alive and has therefore had every chance to publish why it
// is taking so long.
func startupFailureReason(paths daemoncontrol.Paths, pid int) string {
	rep, ok := readStartupReport(startupReportPath(paths), pid)
	if !ok {
		return ""
	}
	return rep.Error
}

// detailSuffix renders a child-reported startup reason for splicing
// into the parent's own error text, or "" when there is none.
func detailSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ": " + reason
}

// readinessTimeoutError is the message both platform parents report
// when the child has not signalled readiness within startupWait.
//
// reason is whatever the child published in its startup report before
// being killed. Before the report existed, every timeout read the same
// on both platforms and told the operator nothing beyond "look at the
// log" — which is why it is spliced in here rather than left for the
// caller to remember.
func readinessTimeoutError(reason, stderrPath string) error {
	if stderrPath == "" {
		return nmerrors.New(nmerrors.CodeBridgeError, fmt.Sprintf(
			"daemon did not become ready within 15s%s (stderr capture was disabled at start; see warning above)",
			detailSuffix(reason)))
	}
	return nmerrors.New(nmerrors.CodeBridgeError, fmt.Sprintf(
		"daemon did not become ready within 15s%s; child stderr: %s",
		detailSuffix(reason), stderrPath))
}
