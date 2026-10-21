package omp

import (
	"reflect"
	"testing"

	"github.com/cnlangzi/nightme/internal/agent"
)

func TestStarterInfo_ExposesHeadlessDefaults(t *testing.T) {
	s := NewStarter("omp", "omp", nil)
	wantArgs := append([]string(nil), DefaultArgs...)

	if got := s.Info().Args; !reflect.DeepEqual(got, wantArgs) {
		t.Fatalf("Info().Args = %v, want %v", got, wantArgs)
	}
	// Info().Env is nil when ompHeadlessEnv is empty
	// (copyStrings turns the empty literal back into nil).
	gotEnv := s.Info().Env
	if len(gotEnv) != 0 {
		t.Fatalf("Info().Env = %v, want empty/nil", gotEnv)
	}
	if mode := s.Info().Mode; mode != agent.ModeJSONIO {
		t.Fatalf("Info().Mode = %s, want json-io", mode)
	}
}

func TestStarterDetect_FakeBinary(t *testing.T) {
	// TestStarterDetect verifies that a config-supplied path
	// override is reflected by Detect. We construct a
	// shell-script "binary" in a tempdir and override the
	// command path via Init.
	tmpDir := t.TempDir()
	fakePath := tmpDir + "/omp-fake"
	if err := writeFile(fakePath, "#!/bin/sh\nexit 0\n"); err != nil {
		t.Fatalf("write fake binary: %v", err)
	}
	s := NewStarter("omp", "omp", nil)
	s.Init(fakePath)
	if err := s.Detect(); err != nil {
		t.Fatalf("Detect() = %v, want nil for an existing file", err)
	}
}

func TestStarterInit_EmptyResetsToDefault(t *testing.T) {
	s := NewStarter("omp", "omp", nil)
	s.Init("/tmp/whatever")
	s.Init("")
	if got := s.Command(); got != "omp" {
		t.Fatalf("Command() after Init(\"\") = %q, want %q", got, "omp")
	}
}

// writeFile is a tiny os.WriteFile shim that doesn't drag the
// os import into the test header.
func writeFile(path, content string) error {
	return writeFileOS(path, content, 0o755)
}
