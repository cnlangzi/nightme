package llm

import "testing"

// The inline key wins over the environment variable: it is the more
// specific of the two, and an operator who wrote it expects it to be
// used.
func TestAPIKey_TheInlineKeyWins(t *testing.T) {
	t.Setenv("NIGHTME_TEST_KEY", "sk-env")
	key, err := APIKey(Config{APIKey: "sk-inline", APIKeyEnv: "NIGHTME_TEST_KEY"})
	if err != nil {
		t.Fatalf("APIKey: %v", err)
	}
	if key != "sk-inline" {
		t.Fatalf("key = %q, want sk-inline", key)
	}
}

// An inline key that is only whitespace counts as unset — the
// environment channel takes over.
func TestAPIKey_AWhitespaceOnlyKeyCountsAsUnset(t *testing.T) {
	t.Setenv("NIGHTME_TEST_KEY", "sk-env")
	key, err := APIKey(Config{APIKey: "   ", APIKeyEnv: "NIGHTME_TEST_KEY"})
	if err != nil {
		t.Fatalf("APIKey: %v", err)
	}
	if key != "sk-env" {
		t.Fatalf("key = %q, want sk-env", key)
	}
}

// The inline key is trimmed before it is used.
func TestAPIKey_TrimsTheInlineKey(t *testing.T) {
	key, err := APIKey(Config{APIKey: "  sk-x  "})
	if err != nil {
		t.Fatalf("APIKey: %v", err)
	}
	if key != "sk-x" {
		t.Fatalf("key = %q, want sk-x", key)
	}
}

// The environment channel works on its own.
func TestAPIKey_ReadsTheEnvironmentVariable(t *testing.T) {
	t.Setenv("NIGHTME_TEST_KEY", "sk-env")
	key, err := APIKey(Config{APIKeyEnv: "NIGHTME_TEST_KEY"})
	if err != nil {
		t.Fatalf("APIKey: %v", err)
	}
	if key != "sk-env" {
		t.Fatalf("key = %q, want sk-env", key)
	}
}

// An unset variable is an error that names the variable and never the
// key.
func TestAPIKey_ReportsAnUnsetEnvironmentVariable(t *testing.T) {
	t.Setenv("NIGHTME_TEST_KEY", "")
	_, err := APIKey(Config{APIKeyEnv: "NIGHTME_TEST_KEY"})
	if err == nil {
		t.Fatal("an unset environment variable was accepted")
	}
	want := "llm: the environment variable NIGHTME_TEST_KEY is empty or unset"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

// The inline key makes an unset variable irrelevant.
func TestAPIKey_TheInlineKeyCoversAnUnsetVariable(t *testing.T) {
	t.Setenv("NIGHTME_TEST_KEY", "")
	key, err := APIKey(Config{APIKey: "sk-inline", APIKeyEnv: "NIGHTME_TEST_KEY"})
	if err != nil {
		t.Fatalf("APIKey: %v", err)
	}
	if key != "sk-inline" {
		t.Fatalf("key = %q, want sk-inline", key)
	}
}

// A configuration that names no credential at all asks for no
// authentication.
func TestAPIKey_NoChannelMeansNoCredential(t *testing.T) {
	key, err := APIKey(Config{})
	if err != nil {
		t.Fatalf("APIKey: %v", err)
	}
	if key != "" {
		t.Fatalf("key = %q, want no credential", key)
	}
}
