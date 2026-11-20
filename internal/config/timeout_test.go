package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestTimeout_ReadsTheDurationString — the file carries the deadline
// of a call as a duration string, and it reads back as the length it
// names.
func TestTimeout_ReadsTheDurationString(t *testing.T) {
	path := writeMeConfig(t, "me:\n  llm:\n    timeout: 30s\n  decider:\n    timeout: 2m\n")

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.Timeout != 30*time.Second {
		t.Errorf("the chat timeout = %v, want 30s", cfg.Me.LLM.Timeout)
	}
	if cfg.Me.Decider.Timeout != 2*time.Minute {
		t.Errorf("the decider timeout = %v, want 2m", cfg.Me.Decider.Timeout)
	}
}

// TestTimeout_RejectsWhatIsNotADuration — a value that is not a
// duration is a parse error of the file: the daemon does not come up
// on it.
func TestTimeout_RejectsWhatIsNotADuration(t *testing.T) {
	path := writeMeConfig(t, "me:\n  llm:\n    timeout: soon\n")

	if _, err := Load(path); err == nil {
		t.Fatal("a timeout that is not a duration was accepted")
	}
}

// TestTimeout_SurvivesTheWholeFileSave — the menu writes the whole
// file again, and the deadline has to survive it as a duration
// string.
func TestTimeout_SurvivesTheWholeFileSave(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	cfg := &Config{Me: MeConfig{LLM: MeLLMConfig{
		Protocol: "chat_completions",
		Timeout:  90 * time.Second,
	}}}
	if err := Save(cfg, path); err != nil {
		t.Fatalf("Save: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "timeout: 1m30s") {
		t.Errorf("the timeout is not written as a duration string:\n%s", string(data))
	}

	back, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if back.Me.LLM.Timeout != 90*time.Second {
		t.Errorf("the timeout = %v, want 1m30s", back.Me.LLM.Timeout)
	}
}
