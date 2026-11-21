package main

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cnlangzi/nightme/internal/channel"
	"github.com/cnlangzi/nightme/internal/config"
	"github.com/cnlangzi/nightme/internal/daemoncontrol"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

// errDaemonNotRunning is what the health RPC answers when there is no
// daemon listening.
var errDaemonNotRunning = errors.New("nightme daemon is not running")

// healthyConfigReport is a config file that passed the check.
func healthyConfigReport() config.Report {
	return config.Report{Path: "/tmp/nightme/config.yaml", Exists: true}
}

// brokenConfigReport is a config file with one unknown key.
func brokenConfigReport() config.Report {
	return config.Report{
		Path:   "/tmp/nightme/config.yaml",
		Exists: true,
		Diagnostics: []config.Diagnostic{
			{Line: 12, Column: 5, Message: `unknown key "feishu.app_secrett"`},
		},
	}
}

// TestDoctor_IsRegisteredOnEveryPlatform asserts the doctor command is
// in the cobra tree and last in the REPL banner. It used to be
// registered on Unix only, which left Windows users without a way to
// look at their config file.
func TestDoctor_IsRegisteredOnEveryPlatform(t *testing.T) {
	root, reg := newRootCmd()
	found := false
	for _, cmd := range root.Commands() {
		if cmd.Name() == "doctor" {
			found = true
		}
	}
	if !found {
		t.Fatal("the doctor command is not registered")
	}
	entries := strings.Split(strings.TrimRight(reg.banner(), "\n"), "\n")
	last := entries[len(entries)-1]
	if !strings.HasPrefix(last, "doctor") {
		t.Errorf("the last banner entry is %q, want doctor", last)
	}
}

// TestDoctor_HelpDescribesTheConfigCheck asserts the help says what
// the command checks — the user reads it when they do not know what
// doctor is for.
func TestDoctor_HelpDescribesTheConfigCheck(t *testing.T) {
	help := newDoctorCmd().Long
	for _, want := range []string{"CONFIG", "config file", "--json"} {
		if !strings.Contains(help, want) {
			t.Errorf("the help text does not mention %q:\n%s", want, help)
		}
	}
}

// TestRunDoctor_ConfigSectionComesFirst asserts the config check is
// the first thing the report says: it is the answer to "why does
// nightme not work", and it holds whether the daemon is up or not.
func TestRunDoctor_ConfigSectionComesFirst(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()
	fake := &fakeDoctorRPCs{startedAt: time.Now()}

	var out strings.Builder
	if err := runDoctorWith(&out, false, brokenConfigReport(), cfgWithDataDir(dir), daemoncontrol.Paths{}, fake.rpcs()); err == nil {
		t.Fatal("doctor returned nil, want the config error")
	}
	if !strings.HasPrefix(out.String(), "CONFIG ") {
		t.Errorf("the report does not start with the CONFIG section:\n%s", out.String())
	}
}

// TestRunDoctor_ConfigSectionWithoutADaemon asserts the config check
// is printed even when the daemon cannot be reached — that is exactly
// when the user needs it.
func TestRunDoctor_ConfigSectionWithoutADaemon(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()
	fake := &fakeDoctorRPCs{healthErr: errDaemonNotRunning}

	var out strings.Builder
	if err := runDoctorWith(&out, false, brokenConfigReport(), cfgWithDataDir(dir), daemoncontrol.Paths{}, fake.rpcs()); err == nil {
		t.Fatal("doctor returned nil, want an error")
	}
	text := out.String()
	if !strings.HasPrefix(text, "CONFIG ") {
		t.Errorf("the report does not start with the CONFIG section:\n%s", text)
	}
	if !strings.Contains(text, "DAEMON") {
		t.Errorf("the report does not say the daemon is unreachable:\n%s", text)
	}
}

// TestWriteConfigSection_Shapes pins the three states the CONFIG
// section can be in.
func TestWriteConfigSection_Shapes(t *testing.T) {
	cases := []struct {
		name   string
		report config.Report
		want   string
	}{
		{
			name:   "healthy",
			report: healthyConfigReport(),
			want:   "ok",
		},
		{
			name:   "missing",
			report: config.Report{Path: "/tmp/nightme/config.yaml"},
			want:   "not found",
		},
		{
			name:   "one problem",
			report: brokenConfigReport(),
			want:   "1 problem",
		},
		{
			name: "two problems",
			report: config.Report{Path: "/tmp/nightme/config.yaml", Exists: true, Diagnostics: []config.Diagnostic{
				{Line: 3, Message: "did not find expected key"},
				{Line: 12, Column: 5, Message: `unknown key "feishu.app_secrett"`},
			}},
			want: "2 problems",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var sb strings.Builder
			if err := writeConfigSection(&sb, tc.report); err != nil {
				t.Fatalf("writeConfigSection: %v", err)
			}
			out := sb.String()
			if !strings.Contains(out, "status:") {
				t.Errorf("no status line:\n%s", out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("status %q missing:\n%s", tc.want, out)
			}
			if !strings.HasPrefix(out, "CONFIG "+tc.report.Path) {
				t.Errorf("the section does not name the file:\n%s", out)
			}
		})
	}
}

// TestWriteConfigSection_NamesThePlaceOfEveryProblem asserts each
// problem carries the line it is on and the column when it has one.
func TestWriteConfigSection_NamesThePlaceOfEveryProblem(t *testing.T) {
	report := config.Report{Path: "/tmp/nightme/config.yaml", Exists: true, Diagnostics: []config.Diagnostic{
		{Line: 3, Message: "did not find expected key"},
		{Line: 12, Column: 5, Message: `unknown key "feishu.app_secrett"`},
	}}
	var sb strings.Builder
	if err := writeConfigSection(&sb, report); err != nil {
		t.Fatalf("writeConfigSection: %v", err)
	}
	out := sb.String()
	for _, want := range []string{
		"line 3:",
		"did not find expected key",
		"line 12:5:",
		`unknown key "feishu.app_secrett"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q:\n%s", want, out)
		}
	}
}

// TestWriteConfigSection_NoteOnlyWhenThereAreProblems asserts the
// restart hint appears when there is something to fix — and stays
// away when there is not.
func TestWriteConfigSection_NoteOnlyWhenThereAreProblems(t *testing.T) {
	var healthy strings.Builder
	if err := writeConfigSection(&healthy, healthyConfigReport()); err != nil {
		t.Fatalf("writeConfigSection: %v", err)
	}
	if strings.Contains(healthy.String(), "note:") {
		t.Errorf("a healthy config file must not carry a note:\n%s", healthy.String())
	}

	var broken strings.Builder
	if err := writeConfigSection(&broken, brokenConfigReport()); err != nil {
		t.Fatalf("writeConfigSection: %v", err)
	}
	if !strings.Contains(broken.String(), "note:") {
		t.Errorf("a broken config file must carry the restart note:\n%s", broken.String())
	}
	if !strings.Contains(broken.String(), "the running daemon keeps the config it loaded at start") {
		t.Errorf("the restart note does not say what to do:\n%s", broken.String())
	}
}

// TestRunDoctor_ConfigProblemExitsWithCode2 asserts a config file
// with problems is a failure a script can see.
func TestRunDoctor_ConfigProblemExitsWithCode2(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()
	fake := &fakeDoctorRPCs{startedAt: time.Now()}

	var out strings.Builder
	err := runDoctorWith(&out, false, brokenConfigReport(), cfgWithDataDir(dir), daemoncontrol.Paths{}, fake.rpcs())
	if err == nil {
		t.Fatal("doctor returned nil, want the config error")
	}
	if got := nmerrors.ExitCode(err); got != nmerrors.CodeConfigError {
		t.Errorf("exit code = %d, want %d", got, nmerrors.CodeConfigError)
	}
}

// TestRunDoctor_UnreachableDaemonExitsWithCode1 asserts a healthy
// config file and an absent daemon keep the exit code they have
// today.
func TestRunDoctor_UnreachableDaemonExitsWithCode1(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()
	fake := &fakeDoctorRPCs{healthErr: errDaemonNotRunning}

	var out strings.Builder
	err := runDoctorWith(&out, false, healthyConfigReport(), cfgWithDataDir(dir), daemoncontrol.Paths{}, fake.rpcs())
	if err == nil {
		t.Fatal("doctor returned nil, want the health RPC error")
	}
	if got := nmerrors.ExitCode(err); got != nmerrors.CodeGenericError {
		t.Errorf("exit code = %d, want %d", got, nmerrors.CodeGenericError)
	}
	if !strings.Contains(err.Error(), "daemon health RPC") {
		t.Errorf("error = %q, want it to name the RPC", err)
	}
}

// TestRunDoctor_ConfigProblemWinsOverTheDaemonError asserts the
// config is the thing the user has to fix first: the exit code says
// so, and the report still tells them the daemon is gone.
func TestRunDoctor_ConfigProblemWinsOverTheDaemonError(t *testing.T) {
	t.Setenv(daemonStderrEnv, "")
	dir := t.TempDir()
	fake := &fakeDoctorRPCs{healthErr: errDaemonNotRunning}

	var out strings.Builder
	err := runDoctorWith(&out, false, brokenConfigReport(), cfgWithDataDir(dir), daemoncontrol.Paths{}, fake.rpcs())
	if err == nil {
		t.Fatal("doctor returned nil, want the config error")
	}
	if got := nmerrors.ExitCode(err); got != nmerrors.CodeConfigError {
		t.Errorf("exit code = %d, want %d", got, nmerrors.CodeConfigError)
	}
	if !strings.Contains(out.String(), "DAEMON") {
		t.Errorf("the report does not mention the daemon:\n%s", out.String())
	}
}

// TestRunDoctor_DoesNotWriteTheConfigFile asserts the doctor leaves
// the config file exactly as it found it: a diagnostic command has no
// business changing the thing it diagnoses.
func TestRunDoctor_DoesNotWriteTheConfigFile(t *testing.T) {
	// A short data dir: the socket path has a length limit and the
	// temp dir of the test runner is not short enough on every OS.
	dataDir, err := os.MkdirTemp("", "nm-doctor")
	if err != nil {
		t.Fatalf("temp dir: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dataDir) })
	t.Setenv("NIGHTME_PATHS_DATA_DIR", dataDir)

	cases := []struct {
		name    string
		content string
	}{
		{name: "with a primary", content: "primary: claude\n"},
		{name: "without a primary", content: "name: macbook\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.content), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}
			t.Setenv("NIGHTME_CONFIG", path)

			var out strings.Builder
			// The daemon is not running in the test environment; the
			// error it returns is not what this test is about.
			_ = runDoctor(&out, false)

			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read config: %v", err)
			}
			if string(got) != tc.content {
				t.Errorf("config file changed: %q", got)
			}
		})
	}
}

// TestWriteDoctorJSON_Shape asserts the JSON report carries the three
// sections a consumer reads, and that the channels keep their place
// at the top level.
func TestWriteDoctorJSON_Shape(t *testing.T) {
	payload := daemoncontrol.HealthPayload{Channels: []daemoncontrol.ChannelHealth{{
		Channel: "feishu",
		Summary: mustSummaryJSON(t, channel.HealthSummary{
			Channel:      "feishu",
			Connected:    true,
			LinkVerified: true,
		}),
	}}}
	var sb strings.Builder
	if err := writeDoctorJSON(&sb, brokenConfigReport(), true, "", payload); err != nil {
		t.Fatalf("writeDoctorJSON: %v", err)
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(sb.String()), &decoded); err != nil {
		t.Fatalf("decode output: %v", err)
	}
	for _, key := range []string{"config", "daemon", "channels"} {
		if _, ok := decoded[key]; !ok {
			t.Errorf("the report has no %q section:\n%s", key, sb.String())
		}
	}
	if len(decoded) != 3 {
		t.Errorf("the report has %d sections, want 3:\n%s", len(decoded), sb.String())
	}

	var cfg struct {
		Path        string              `json:"path"`
		Status      string              `json:"status"`
		Diagnostics []config.Diagnostic `json:"diagnostics"`
	}
	if err := json.Unmarshal(decoded["config"], &cfg); err != nil {
		t.Fatalf("decode config section: %v", err)
	}
	if cfg.Path != "/tmp/nightme/config.yaml" {
		t.Errorf("config path = %q", cfg.Path)
	}
	if cfg.Status != "error" {
		t.Errorf("config status = %q, want error", cfg.Status)
	}
	if len(cfg.Diagnostics) != 1 || cfg.Diagnostics[0].Line != 12 || cfg.Diagnostics[0].Column != 5 {
		t.Errorf("config diagnostics = %+v", cfg.Diagnostics)
	}

	var daemon struct {
		Reachable bool   `json:"reachable"`
		Error     string `json:"error"`
	}
	if err := json.Unmarshal(decoded["daemon"], &daemon); err != nil {
		t.Fatalf("decode daemon section: %v", err)
	}
	if !daemon.Reachable || daemon.Error != "" {
		t.Errorf("daemon section = %+v", daemon)
	}
}

// TestWriteDoctorJSON_EmptyDiagnosticsAreAList asserts an empty list
// is a list — a consumer that iterates it must not have to handle a
// null.
func TestWriteDoctorJSON_EmptyDiagnosticsAreAList(t *testing.T) {
	var sb strings.Builder
	if err := writeDoctorJSON(&sb, healthyConfigReport(), true, "", daemoncontrol.HealthPayload{}); err != nil {
		t.Fatalf("writeDoctorJSON: %v", err)
	}
	if !strings.Contains(sb.String(), `"diagnostics": []`) {
		t.Errorf("diagnostics is not an empty list:\n%s", sb.String())
	}
	if !strings.Contains(sb.String(), `"channels": []`) {
		t.Errorf("channels is not an empty list:\n%s", sb.String())
	}
}
