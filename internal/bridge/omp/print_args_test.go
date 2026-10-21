package omp

import (
	"reflect"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

// TestBuildPrintArgs_OmitsTransportTweaks pins R12: the
// print-mode argv does NOT inject model / provider / API key /
// MCP config — only transport/permission flags. argv layout
// per the issue spec:
//
//	omp -p <prompt> --mode json --approval-mode yolo
func TestBuildPrintArgs_OmitsTransportTweaks(t *testing.T) {
	args, prompt := buildPrintArgs([]agent.ContentBlock{
		{Type: agent.ContentText, Text: "headless"},
	})
	wantArgs := []string{
		"-p", "headless",
		"--mode", "json",
		"--approval-mode", "yolo",
	}
	if prompt != "headless" {
		t.Fatalf("prompt = %q, want %q", prompt, "headless")
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("buildPrintArgs() = %v, want %v", args, wantArgs)
	}
	for _, banned := range []string{"--model", "--provider", "--api-key", "--mcp-config"} {
		for _, a := range args {
			if a == banned {
				t.Errorf("argv contains %q, which is forbidden on the bridge surface", banned)
			}
		}
	}
}

// TestBuildArgs_AppendsResumeLast asserts R13: cfg.SessionID
// is appended LAST as ["--resume", "<id>"].
func TestBuildArgs_AppendsResumeLast(t *testing.T) {
	args := buildArgs(nil, agent.StartConfig{SessionID: "abc"})
	if got := args[len(args)-2:]; !reflect.DeepEqual(got, []string{"--resume", "abc"}) {
		t.Fatalf("last two argv elements = %v, want [\"--resume\", \"abc\"]", got)
	}
}

// TestBuildArgs_FiltersCfgResume verifies R13: when
// cfg.SessionID is set, any user-supplied session selection
// flag is removed from cfg.Args so the runtime SessionID
// wins.
func TestBuildArgs_FiltersCfgResume(t *testing.T) {
	args := buildArgs(nil, agent.StartConfig{
		SessionID: "abc",
		Args:      []string{"--resume", "user-supplied", "--no-session"},
	})
	for _, a := range args {
		if a == "user-supplied" {
			t.Errorf("user-supplied --resume value leaked into argv")
		}
	}
	// The runtime value must still be the last two.
	if got := args[len(args)-2:]; !reflect.DeepEqual(got, []string{"--resume", "abc"}) {
		t.Fatalf("last two argv elements = %v, want [\"--resume\", \"abc\"]", got)
	}
}

// TestBuildArgs_NoResumeWhenEmpty verifies that with no
// SessionID, no `--resume` ends up in argv.
func TestBuildArgs_NoResumeWhenEmpty(t *testing.T) {
	args := buildArgs(nil, agent.StartConfig{})
	for _, a := range args {
		if a == "--resume" {
			t.Errorf("--resume injected into argv when SessionID was empty")
		}
	}
}
