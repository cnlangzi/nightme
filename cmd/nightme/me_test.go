package main

import (
	"bufio"
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mattn/go-isatty"
	"github.com/spf13/cobra"

	"github.com/cnlangzi/nightme/internal/config"
	nmerrors "github.com/cnlangzi/nightme/internal/errors"
)

// meTestConfig writes a config file in a fresh temp dir and returns
// its path.
func meTestConfig(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return path
}

// runMeWizard drives the wizard with the given answers and returns
// its output and its error.
func runMeWizard(t *testing.T, path, answers string) (string, error) {
	t.Helper()
	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	var buf bytes.Buffer
	err = meWizard(cfg, path, strings.NewReader(answers), &buf)
	return buf.String(), err
}

// TestMeWizard_WritesTheChatModel covers the happy path: the answers
// land in the me.llm block of the config file.
func TestMeWizard_WritesTheChatModel(t *testing.T) {
	path := meTestConfig(t, "primary: claude\n")

	out, err := runMeWizard(t, path, strings.Join([]string{
		"https://api.example.com/v1",
		"gpt-4.1-mini",
		"40960",
		"claude",
		"2", // set api_key
		"sk-test-1234",
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	llmCfg := cfg.Me.LLM
	if llmCfg.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", llmCfg.Provider)
	}
	if llmCfg.BaseURL != "https://api.example.com/v1" {
		t.Errorf("BaseURL = %q", llmCfg.BaseURL)
	}
	if llmCfg.Model != "gpt-4.1-mini" {
		t.Errorf("Model = %q", llmCfg.Model)
	}
	if llmCfg.MaxTokens != 40960 {
		t.Errorf("MaxTokens = %d, want 40960", llmCfg.MaxTokens)
	}
	if llmCfg.Agent != "claude" {
		t.Errorf("Agent = %q", llmCfg.Agent)
	}
	if llmCfg.APIKey != "sk-test-1234" {
		t.Errorf("APIKey = %q", llmCfg.APIKey)
	}
	if llmCfg.APIKeyEnv != "" {
		t.Errorf("APIKeyEnv = %q, want empty", llmCfg.APIKeyEnv)
	}
	if cfg.Primary != "claude" {
		t.Errorf("Primary = %q, want claude (the rest of the file survives)", cfg.Primary)
	}
}

// TestMeWizard_EmptyAnswerKeepsTheCurrentValue covers the keep
// semantics: an empty answer leaves the field as it was.
func TestMeWizard_EmptyAnswerKeepsTheCurrentValue(t *testing.T) {
	path := meTestConfig(t, `me:
  llm:
    provider: openai
    base_url: https://old.example.com/v1
    model: old-model
    max_tokens: 8192
    agent: codex
`)

	out, err := runMeWizard(t, path, "\n\n\n\n1\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	llmCfg := cfg.Me.LLM
	if llmCfg.BaseURL != "https://old.example.com/v1" {
		t.Errorf("BaseURL = %q", llmCfg.BaseURL)
	}
	if llmCfg.Model != "old-model" {
		t.Errorf("Model = %q", llmCfg.Model)
	}
	if llmCfg.MaxTokens != 8192 {
		t.Errorf("MaxTokens = %d, want 8192", llmCfg.MaxTokens)
	}
	if llmCfg.Agent != "codex" {
		t.Errorf("Agent = %q", llmCfg.Agent)
	}
}

// TestMeWizard_PromptShowsTheCurrentValue covers the prompt: it
// carries the value an empty answer keeps.
func TestMeWizard_PromptShowsTheCurrentValue(t *testing.T) {
	path := meTestConfig(t, `me:
  llm:
    provider: openai
    base_url: https://old.example.com/v1
    model: old-model
    max_tokens: 8192
`)

	out, err := runMeWizard(t, path, "\n\n\n\n1\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}
	for _, want := range []string{
		"base_url [https://old.example.com/v1]",
		"model [old-model]",
		"max_tokens [8192]",
		"agent []",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("prompt missing %q:\n%s", want, out)
		}
	}
}

// TestMeWizard_UnsetMaxTokensShowsTheDefault covers the cap an
// operator never configured: the prompt offers the shipped default.
func TestMeWizard_UnsetMaxTokensShowsTheDefault(t *testing.T) {
	path := meTestConfig(t, "primary: claude\n")

	out, err := runMeWizard(t, path, strings.Join([]string{
		"https://api.example.com/v1",
		"gpt-4.1-mini",
		"",
		"",
		"1",
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}
	if !strings.Contains(out, "max_tokens [40960]") {
		t.Errorf("prompt missing the default cap:\n%s", out)
	}
}

// TestMeWizard_QAborts covers the abort: nothing is written and the
// wizard reports success.
func TestMeWizard_QAborts(t *testing.T) {
	path := meTestConfig(t, "primary: claude\n")

	out, err := runMeWizard(t, path, "q\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}
	if _, err := os.Stat(path); err == nil {
		data, _ := os.ReadFile(path)
		if strings.Contains(string(data), "me:") {
			t.Errorf("the wizard wrote a configuration:\n%s", string(data))
		}
	}
}

// TestMeWizard_ProviderSwitchNeedsConfirmation covers a file that
// names another provider: the wizard says what it configures and
// asks before switching.
func TestMeWizard_ProviderSwitchNeedsConfirmation(t *testing.T) {
	path := meTestConfig(t, `me:
  llm:
    provider: anthropic
    model: claude-sonnet-4
`)

	out, err := runMeWizard(t, path, "n\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}
	if !strings.Contains(out, "openai") {
		t.Errorf("the wizard did not say which provider it configures:\n%s", out)
	}
	if !strings.Contains(out, "Nothing changed") {
		t.Errorf("the wizard did not report that nothing changed:\n%s", out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.Provider != "anthropic" {
		t.Errorf("Provider = %q, want anthropic (declining writes nothing)", cfg.Me.LLM.Provider)
	}
}

// TestMeWizard_ProviderSwitchAccepted covers the confirmed switch.
func TestMeWizard_ProviderSwitchAccepted(t *testing.T) {
	path := meTestConfig(t, `me:
  llm:
    provider: anthropic
    model: claude-sonnet-4
`)

	out, err := runMeWizard(t, path, strings.Join([]string{
		"y",
		"https://api.example.com/v1",
		"gpt-4.1-mini",
		"",
		"",
		"1",
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", cfg.Me.LLM.Provider)
	}
	if cfg.Me.LLM.Model != "gpt-4.1-mini" {
		t.Errorf("Model = %q", cfg.Me.LLM.Model)
	}
}

// TestMeWizard_UnknownAgentIsRefused covers the agent field: a name
// that is not a built-in is refused and the built-ins are listed.
func TestMeWizard_UnknownAgentIsRefused(t *testing.T) {
	path := meTestConfig(t, "primary: claude\n")

	out, err := runMeWizard(t, path, strings.Join([]string{
		"https://api.example.com/v1",
		"gpt-4.1-mini",
		"",
		"nosuchagent",
		"claude",
		"1",
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}
	if !strings.Contains(out, "nosuchagent") {
		t.Errorf("the wizard did not report the unknown agent:\n%s", out)
	}
	if !strings.Contains(out, "claude") {
		t.Errorf("the wizard did not list the built-in agents:\n%s", out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.Agent != "claude" {
		t.Errorf("Agent = %q, want claude", cfg.Me.LLM.Agent)
	}
}

// TestMeWizard_InvalidMaxTokensIsRefused covers the cap: a value that
// is not a number is refused and the question is asked again.
func TestMeWizard_InvalidMaxTokensIsRefused(t *testing.T) {
	path := meTestConfig(t, "primary: claude\n")

	out, err := runMeWizard(t, path, strings.Join([]string{
		"https://api.example.com/v1",
		"gpt-4.1-mini",
		"not-a-number",
		"-1",
		"2048",
		"",
		"1",
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %d, want 2048", cfg.Me.LLM.MaxTokens)
	}
}

// TestMeWizard_KeepsOneCredentialChannel covers the two credential
// channels being alternatives: the one that is in force survives, the
// other is gone.
func TestMeWizard_KeepsOneCredentialChannel(t *testing.T) {
	path := meTestConfig(t, `me:
  llm:
    provider: openai
    base_url: https://api.example.com/v1
    model: gpt-4.1-mini
    api_key: sk-old
    api_key_env: OLD_KEY
`)

	out, err := runMeWizard(t, path, "\n\n\n\n1\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.APIKey != "sk-old" {
		t.Errorf("APIKey = %q, want sk-old", cfg.Me.LLM.APIKey)
	}
	if cfg.Me.LLM.APIKeyEnv != "" {
		t.Errorf("APIKeyEnv = %q, want empty (the inline key wins)", cfg.Me.LLM.APIKeyEnv)
	}
}

// TestMeWizard_RemovesTheCredential covers dropping the credential:
// neither channel survives.
func TestMeWizard_RemovesTheCredential(t *testing.T) {
	path := meTestConfig(t, `me:
  llm:
    provider: openai
    base_url: https://api.example.com/v1
    model: gpt-4.1-mini
    api_key: sk-old
`)

	out, err := runMeWizard(t, path, "\n\n\n\n4\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.APIKey != "" {
		t.Errorf("APIKey = %q, want empty", cfg.Me.LLM.APIKey)
	}
	if cfg.Me.LLM.APIKeyEnv != "" {
		t.Errorf("APIKeyEnv = %q, want empty", cfg.Me.LLM.APIKeyEnv)
	}
}

// TestMeWizard_NeverPrintsTheKey covers the secret: the summary and
// the report carry at most the last four characters.
func TestMeWizard_NeverPrintsTheKey(t *testing.T) {
	path := meTestConfig(t, "primary: claude\n")
	const key = "sk-super-secret-abcd"

	out, err := runMeWizard(t, path, strings.Join([]string{
		"https://api.example.com/v1",
		"gpt-4.1-mini",
		"",
		"",
		"2",
		key,
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}
	if strings.Contains(out, key) {
		t.Errorf("the key was printed in full:\n%s", out)
	}
	if !strings.Contains(out, "…abcd") {
		t.Errorf("the masked key is missing:\n%s", out)
	}
}

// TestMeWizard_MissingEnvVarWarns covers a credential the daemon
// resolves in its own environment: a variable that is missing in the
// operator's shell is a warning, not a failure.
func TestMeWizard_MissingEnvVarWarns(t *testing.T) {
	t.Setenv("NIGHTME_TEST_MISSING_KEY", "")
	os.Unsetenv("NIGHTME_TEST_MISSING_KEY")
	path := meTestConfig(t, "primary: claude\n")

	out, err := runMeWizard(t, path, strings.Join([]string{
		"https://api.example.com/v1",
		"gpt-4.1-mini",
		"",
		"",
		"3", // set api_key_env
		"NIGHTME_TEST_MISSING_KEY",
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}
	if !strings.Contains(out, "NIGHTME_TEST_MISSING_KEY") {
		t.Errorf("the wizard did not warn about the variable:\n%s", out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.APIKeyEnv != "NIGHTME_TEST_MISSING_KEY" {
		t.Errorf("APIKeyEnv = %q", cfg.Me.LLM.APIKeyEnv)
	}
}

// TestMeWizard_ReportsTheRestartHint covers the restart: the daemon
// builds its models at start-up.
func TestMeWizard_ReportsTheRestartHint(t *testing.T) {
	path := meTestConfig(t, "primary: claude\n")

	out, err := runMeWizard(t, path, strings.Join([]string{
		"https://api.example.com/v1",
		"gpt-4.1-mini",
		"",
		"",
		"1",
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}
	if !strings.Contains(out, "nightme restart") {
		t.Errorf("the wizard did not name the restart command:\n%s", out)
	}
	if !strings.Contains(out, "builds") {
		t.Errorf("the wizard did not report the build:\n%s", out)
	}
}

// TestMeWizard_BuildFailureIsAConfigError covers a configuration the
// builder refuses: the wizard says why and fails with the config
// exit code.
func TestMeWizard_BuildFailureIsAConfigError(t *testing.T) {
	path := meTestConfig(t, "primary: claude\n")

	out, err := runMeWizard(t, path, strings.Join([]string{
		"https://api.example.com/v1",
		"", // no model
		"",
		"",
		"1",
	}, "\n")+"\n")
	if err == nil {
		t.Fatalf("expected an error for a configuration that does not build:\n%s", out)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeConfigError {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeConfigError)
	}
	if !strings.Contains(out, "has no model") {
		t.Errorf("the wizard did not report the reason:\n%s", out)
	}
}

// TestMeWizard_MissingBaseURLIsReported covers a configuration with
// no endpoint: the builder's reason is printed.
func TestMeWizard_MissingBaseURLIsReported(t *testing.T) {
	path := meTestConfig(t, "primary: claude\n")

	out, err := runMeWizard(t, path, strings.Join([]string{
		"", // no base_url
		"gpt-4.1-mini",
		"",
		"",
		"1",
	}, "\n")+"\n")
	if err == nil {
		t.Fatalf("expected an error for a configuration that does not build:\n%s", out)
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeConfigError {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeConfigError)
	}
	if !strings.Contains(out, "base_url is required") {
		t.Errorf("the wizard did not report the reason:\n%s", out)
	}
}

// TestMeCmd_MalformedConfigIsAConfigError covers a broken file: the
// command reports it, writes nothing and fails with the config exit
// code.
func TestMeCmd_MalformedConfigIsAConfigError(t *testing.T) {
	broken := "primary: [unclosed\n"
	path := meTestConfig(t, broken)
	t.Setenv("NIGHTME_CONFIG", path)

	cmd := newMeCmd()
	cmd.SetIn(strings.NewReader("\n"))
	var buf bytes.Buffer
	cmd.SetOut(&buf)

	err := cmd.RunE(cmd, nil)
	if err == nil {
		t.Fatal("expected an error for a malformed file")
	}
	if code := nmerrors.ExitCode(err); code != nmerrors.CodeConfigError {
		t.Errorf("exit code = %d, want %d", code, nmerrors.CodeConfigError)
	}
	data, _ := os.ReadFile(path)
	if string(data) != broken {
		t.Errorf("the broken file was rewritten:\n%s", string(data))
	}
}

// TestMeCmd_IsInTheTreeAndTheBanner covers the registration: the
// command is in the cobra tree, the REPL banner lists it once and the
// tray menu leaves it out — it reads stdin.
func TestMeCmd_IsInTheTreeAndTheBanner(t *testing.T) {
	root, reg := newRootCmd()

	var found *cobra.Command
	for _, c := range root.Commands() {
		if c.Name() == "me" {
			found = c
			break
		}
	}
	if found == nil {
		t.Fatal("the me command is not in the cobra tree")
	}

	banner := reg.banner()
	count := 0
	for _, line := range strings.Split(banner, "\n") {
		if strings.HasPrefix(line, "me ") {
			count++
		}
	}
	if count != 1 {
		t.Errorf("the banner lists the me command %d times, want once:\n%s", count, banner)
	}
	for _, item := range reg.TrayItems() {
		if item.Command == found {
			t.Error("the me command must stay out of the tray menu")
		}
	}
}

// TestMeCmd_InsideTheREPL covers the REPL: the wizard runs, and the
// line after it is read and executed normally.
func TestMeCmd_InsideTheREPL(t *testing.T) {
	path := meTestConfig(t, "primary: claude\n")
	t.Setenv("NIGHTME_CONFIG", path)

	root, reg := newRootCmd()
	var buf bytes.Buffer
	captureREPLIO(root, &buf)

	in := strings.NewReader(strings.Join([]string{
		"me",
		"https://api.example.com/v1",
		"gpt-4.1-mini",
		"",
		"",
		"1", // keep the credential
		"version",
		"exit",
	}, "\n") + "\n")
	if err := runREPLWith(root, reg, nil, in, &buf); err != nil {
		t.Fatalf("runREPLWith: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "wrote me.llm") {
		t.Errorf("the wizard did not run inside the REPL:\n%s", out)
	}
	// The banner carries a version line of its own, so the command
	// ran only if the line appears twice.
	if got := strings.Count(out, "nightme version"); got != 2 {
		t.Errorf("the line after the wizard was not executed (version line x%d):\n%s", got, out)
	}
}

// TestCanReadPassword_NeedsATerminal covers the rule the key prompt
// follows: only a terminal reads the key without echo, and only while
// the buffered reader holds nothing.
func TestCanReadPassword_NeedsATerminal(t *testing.T) {
	file, err := os.CreateTemp(t.TempDir(), "input")
	if err != nil {
		t.Fatalf("temp file: %v", err)
	}
	defer file.Close()

	if canReadPassword(file, bufio.NewReader(file)) {
		t.Error("a plain file must not be read without echo")
	}
	if canReadPassword(os.Stdin, bufio.NewReader(os.Stdin)) != isatty.IsTerminal(os.Stdin.Fd()) {
		t.Error("the terminal check does not follow the input")
	}
}

// TestMeWizard_ProviderSwitchAcceptsYes covers the confirmation: a
// full "yes" switches, the way an operator would answer it.
func TestMeWizard_ProviderSwitchAcceptsYes(t *testing.T) {
	path := meTestConfig(t, `me:
  llm:
    provider: anthropic
    model: claude-sonnet-4
`)

	out, err := runMeWizard(t, path, strings.Join([]string{
		"yes",
		"https://api.example.com/v1",
		"gpt-4.1-mini",
		"",
		"",
		"1",
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.Provider != "openai" {
		t.Errorf("Provider = %q, want openai", cfg.Me.LLM.Provider)
	}
}

// TestMeWizard_ZeroMaxTokensRemovesTheCap covers the cap of zero:
// the key leaves the file and the shipped default takes its place.
func TestMeWizard_ZeroMaxTokensRemovesTheCap(t *testing.T) {
	path := meTestConfig(t, `me:
  llm:
    provider: openai
    base_url: https://api.example.com/v1
    model: gpt-4.1-mini
    max_tokens: 8192
`)

	out, err := runMeWizard(t, path, strings.Join([]string{
		"",
		"",
		"0",
		"",
		"1",
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}
	if !strings.Contains(out, "removes the cap") {
		t.Errorf("the wizard did not say what zero means:\n%s", out)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if strings.Contains(string(data), "max_tokens") {
		t.Errorf("the cap survived a zero:\n%s", string(data))
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.MaxTokens != 40960 {
		t.Errorf("MaxTokens = %d, want the default 40960", cfg.Me.LLM.MaxTokens)
	}
}

// TestMeWizard_CreatesAMissingConfigFile covers a config file that
// is not there yet: the wizard writes it.
func TestMeWizard_CreatesAMissingConfigFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".nightme", "config.yaml")

	out, err := runMeWizard(t, path, strings.Join([]string{
		"https://api.example.com/v1",
		"gpt-4.1-mini",
		"",
		"",
		"1",
	}, "\n")+"\n")
	if err != nil {
		t.Fatalf("meWizard: %v\n%s", err, out)
	}

	cfg, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Me.LLM.Model != "gpt-4.1-mini" {
		t.Errorf("Model = %q, want gpt-4.1-mini", cfg.Me.LLM.Model)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("stat: %v", err)
	}
}

// TestMeCmd_RefusesArguments covers a typo: the command takes no
// arguments, so `nightme me whatever` says so instead of opening the
// wizard.
func TestMeCmd_RefusesArguments(t *testing.T) {
	cmd := newMeCmd()
	cmd.SetArgs([]string{"whatever"})
	var buf bytes.Buffer
	cmd.SetOut(&buf)
	cmd.SetErr(&buf)

	if err := cmd.Execute(); err == nil {
		t.Fatal("expected an error for an unexpected argument")
	}
}
