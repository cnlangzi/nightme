package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeMeConfig writes content to a file in a fresh temp dir and
// returns its path.
func writeMeConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// TestSetMeLLM_CreatesTheSection covers a config file that has no
// me section at all: the writer creates it.
func TestSetMeLLM_CreatesTheSection(t *testing.T) {
	path := writeMeConfig(t, "primary: claude\n")

	err := SetMeLLM(path, MeLLMValues{
		Provider:  "openai",
		BaseURL:   "https://example.test/v1",
		Model:     "gpt-4.1-mini",
		MaxTokens: 40960,
		Agent:     "claude",
		APIKey:    "sk-secret",
	})
	if err != nil {
		t.Fatalf("SetMeLLM: %v", err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", cfg.Me.LLM.Provider)
	}
	if cfg.Me.LLM.BaseURL != "https://example.test/v1" {
		t.Errorf("BaseURL = %q", cfg.Me.LLM.BaseURL)
	}
	if cfg.Me.LLM.Model != "gpt-4.1-mini" {
		t.Errorf("Model = %q", cfg.Me.LLM.Model)
	}
	if cfg.Me.LLM.MaxTokens != 40960 {
		t.Errorf("MaxTokens = %d, want 40960", cfg.Me.LLM.MaxTokens)
	}
	if cfg.Me.LLM.Agent != "claude" {
		t.Errorf("Agent = %q", cfg.Me.LLM.Agent)
	}
	if cfg.Me.LLM.APIKey != "sk-secret" {
		t.Errorf("APIKey = %q", cfg.Me.LLM.APIKey)
	}
	if cfg.Me.LLM.APIKeyEnv != "" {
		t.Errorf("APIKeyEnv = %q, want empty", cfg.Me.LLM.APIKeyEnv)
	}
	if cfg.Primary != "claude" {
		t.Errorf("Primary = %q, want claude (the rest of the file survives)", cfg.Primary)
	}
}

// TestSetMeLLM_PreservesTheRestOfTheDocument covers the promise of
// the surgical write: comments, key order and keys nightme does not
// model survive.
func TestSetMeLLM_PreservesTheRestOfTheDocument(t *testing.T) {
	path := writeMeConfig(t, `# nightme config — hand written
name: laptop

# a key nightme does not model
mystery: keep me

me:
  llm:
    model: gpt-4.1-mini
`)

	err := SetMeLLM(path, MeLLMValues{
		Provider:  "openai",
		BaseURL:   "https://example.test/v1",
		Model:     "gpt-4.1",
		MaxTokens: 8192,
	})
	if err != nil {
		t.Fatalf("SetMeLLM: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(data)
	for _, want := range []string{
		"# nightme config — hand written",
		"# a key nightme does not model",
		"mystery: keep me",
		"name: laptop",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("file lost %q:\n%s", want, got)
		}
	}
	if !strings.Contains(got, "model: gpt-4.1\n") {
		t.Errorf("model not written:\n%s", got)
	}
}

// TestSetMeLLM_EmptyValueRemovesTheKey covers the removal rule: an
// empty value takes its key with it.
func TestSetMeLLM_EmptyValueRemovesTheKey(t *testing.T) {
	path := writeMeConfig(t, `me:
  llm:
    provider: openai
    base_url: https://example.test/v1
    model: gpt-4.1-mini
    agent: claude
    api_key: sk-old
    api_key_env: OLD_KEY
`)

	err := SetMeLLM(path, MeLLMValues{
		Provider: "openai",
		BaseURL:  "https://example.test/v1",
		Model:    "gpt-4.1-mini",
	})
	if err != nil {
		t.Fatalf("SetMeLLM: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(data)
	for _, gone := range []string{"agent:", "api_key:", "api_key_env:"} {
		if strings.Contains(got, gone) {
			t.Errorf("file still holds %q:\n%s", gone, got)
		}
	}
	if !strings.Contains(got, "model: gpt-4.1-mini") {
		t.Errorf("the kept values are gone:\n%s", got)
	}
}

// TestSetMeLLM_OneCredentialChannel covers the two credential
// channels being alternatives: whichever one carries a value is
// written and the other is removed.
func TestSetMeLLM_OneCredentialChannel(t *testing.T) {
	cases := []struct {
		name    string
		values  MeLLMValues
		want    string
		notWant string
	}{
		{
			name:    "the key itself",
			values:  MeLLMValues{APIKey: "sk-new"},
			want:    "api_key: sk-new",
			notWant: "api_key_env",
		},
		{
			name:    "the environment variable",
			values:  MeLLMValues{APIKeyEnv: "NEW_KEY"},
			want:    "api_key_env: NEW_KEY",
			notWant: "api_key:",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeMeConfig(t, `me:
  llm:
    api_key: sk-old
    api_key_env: OLD_KEY
`)
			values := tc.values
			values.Provider = "openai"
			values.Model = "gpt-4.1-mini"
			if err := SetMeLLM(path, values); err != nil {
				t.Fatalf("SetMeLLM: %v", err)
			}
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read: %v", err)
			}
			got := string(data)
			if !strings.Contains(got, tc.want) {
				t.Errorf("file missing %q:\n%s", tc.want, got)
			}
			if strings.Contains(got, tc.notWant) {
				t.Errorf("file still holds %q:\n%s", tc.notWant, got)
			}
		})
	}
}

// TestSetMeLLM_RemovesBothCredentialChannels covers the credential
// being dropped: neither channel survives.
func TestSetMeLLM_RemovesBothCredentialChannels(t *testing.T) {
	path := writeMeConfig(t, `me:
  llm:
    api_key: sk-old
    api_key_env: OLD_KEY
`)

	if err := SetMeLLM(path, MeLLMValues{Provider: "openai"}); err != nil {
		t.Fatalf("SetMeLLM: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	got := string(data)
	if strings.Contains(got, "api_key") {
		t.Errorf("file still holds a credential:\n%s", got)
	}
}

// TestSetMeLLM_ZeroMaxTokensRemovesTheKey covers the cap rule: zero
// takes the key with it.
func TestSetMeLLM_ZeroMaxTokensRemovesTheKey(t *testing.T) {
	path := writeMeConfig(t, `me:
  llm:
    max_tokens: 8192
`)

	if err := SetMeLLM(path, MeLLMValues{Provider: "openai"}); err != nil {
		t.Fatalf("SetMeLLM: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "max_tokens") {
		t.Errorf("max_tokens survived a zero:\n%s", string(data))
	}
}

// TestSetMeLLM_MaxTokensIsAnInteger covers the cap reading back as a
// number: a quoted "40960" does not decode into an int field.
func TestSetMeLLM_MaxTokensIsAnInteger(t *testing.T) {
	path := writeMeConfig(t, "primary: claude\n")

	if err := SetMeLLM(path, MeLLMValues{Provider: "openai", MaxTokens: 40960}); err != nil {
		t.Fatalf("SetMeLLM: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "max_tokens: 40960") {
		t.Errorf("max_tokens is not written as a number:\n%s", string(data))
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.MaxTokens != 40960 {
		t.Errorf("MaxTokens = %d, want 40960", cfg.Me.LLM.MaxTokens)
	}
}

// TestSetMeLLM_MalformedFileIsAnError covers the broken file: it is
// reported and left untouched.
func TestSetMeLLM_MalformedFileIsAnError(t *testing.T) {
	broken := "primary: [unclosed\n"
	path := writeMeConfig(t, broken)

	if err := SetMeLLM(path, MeLLMValues{Provider: "openai"}); err == nil {
		t.Fatal("expected an error for a malformed file")
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if string(data) != broken {
		t.Errorf("the broken file was rewritten:\n%s", string(data))
	}
}

// TestSetMeLLM_MissingFileIsCreated covers the fresh install: the
// file appears with the mode the config file keeps.
func TestSetMeLLM_MissingFileIsCreated(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "config.yaml")

	if err := SetMeLLM(path, MeLLMValues{Provider: "openai", Model: "gpt-4.1-mini"}); err != nil {
		t.Fatalf("SetMeLLM: %v", err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("file mode = %o, want 600", perm)
	}
	dirInfo, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatalf("stat dir: %v", err)
	}
	if perm := dirInfo.Mode().Perm(); perm != 0o700 {
		t.Errorf("dir mode = %o, want 700", perm)
	}
}

// TestSetMeLLM_QuotedNumberBecomesANumber covers a config file that
// quotes the cap: the rewritten value has to be a number again — a
// quoted "40960" does not decode into an int, and the file would not
// load.
func TestSetMeLLM_QuotedNumberBecomesANumber(t *testing.T) {
	path := writeMeConfig(t, "me:\n  llm:\n    max_tokens: \"8192\"\n")

	if err := SetMeLLM(path, MeLLMValues{Provider: "openai", MaxTokens: 40960}); err != nil {
		t.Fatalf("SetMeLLM: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(string(data), "max_tokens: 40960") {
		t.Errorf("the cap is not written as a number:\n%s", string(data))
	}
	if _, err := Load(path); err != nil {
		t.Errorf("the rewritten file does not load: %v", err)
	}
}
