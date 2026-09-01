package pi

import (
	"reflect"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestBuildPrintArgs_UsesHeadlessDefaults(t *testing.T) {
	args, prompt := buildPrintArgs([]agent.ContentBlock{
		{Type: agent.ContentText, Text: "headless"},
	})
	wantArgs := []string{
		"--mode", "json",
		"--approve",
		"--no-themes",
		"--offline",
		"-p", "headless",
	}
	if prompt != "headless" {
		t.Fatalf("prompt = %q, want %q", prompt, "headless")
	}
	if !reflect.DeepEqual(args, wantArgs) {
		t.Fatalf("buildPrintArgs() = %v, want %v", args, wantArgs)
	}
}
