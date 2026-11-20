package llm

import "testing"

// TestAPIKey_ResolvesTheChannel — the two channels and their
// precedence.
//
// The inline key wins over the environment variable: it is the more
// specific of the two, and an operator who wrote it expects it to be
// used. A key that is only whitespace counts as unset, and a
// configuration that names no channel at all asks for no
// authentication.
func TestAPIKey_ResolvesTheChannel(t *testing.T) {
	t.Setenv("NIGHTME_TEST_KEY", "sk-env")
	t.Setenv("NIGHTME_TEST_EMPTY", "")

	cases := []struct {
		name string
		cfg  ChatConfig
		want string
		err  string
	}{
		{
			name: "the inline key wins",
			cfg:  ChatConfig{APIKey: "sk-inline", APIKeyEnv: "NIGHTME_TEST_KEY"},
			want: "sk-inline",
		},
		{
			name: "the inline key is trimmed",
			cfg:  ChatConfig{APIKey: "  sk-x  "},
			want: "sk-x",
		},
		{
			name: "a whitespace-only key counts as unset",
			cfg:  ChatConfig{APIKey: "   ", APIKeyEnv: "NIGHTME_TEST_KEY"},
			want: "sk-env",
		},
		{
			name: "the environment channel alone",
			cfg:  ChatConfig{APIKeyEnv: "NIGHTME_TEST_KEY"},
			want: "sk-env",
		},
		{
			name: "the inline key covers an unset variable",
			cfg:  ChatConfig{APIKey: "sk-inline", APIKeyEnv: "NIGHTME_TEST_EMPTY"},
			want: "sk-inline",
		},
		{
			name: "no channel means no credential",
			cfg:  ChatConfig{},
			want: "",
		},
		{
			name: "an unset variable is an error",
			cfg:  ChatConfig{APIKeyEnv: "NIGHTME_TEST_EMPTY"},
			err:  "llm: the environment variable NIGHTME_TEST_EMPTY is empty or unset",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			key, err := APIKey(tc.cfg)
			if tc.err != "" {
				if err == nil {
					t.Fatalf("the call succeeded with %q, want the error %q", key, tc.err)
				}
				if err.Error() != tc.err {
					t.Fatalf("error = %q, want %q", err, tc.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("APIKey: %v", err)
			}
			if key != tc.want {
				t.Fatalf("key = %q, want %q", key, tc.want)
			}
		})
	}
}

// TestDualCredential — only a configuration that names both channels
// is ambiguous.
func TestDualCredential(t *testing.T) {
	cases := []struct {
		name string
		cfg  ChatConfig
		want bool
	}{
		{name: "both channels", cfg: ChatConfig{APIKey: "sk-inline", APIKeyEnv: "NIGHTME_TEST_KEY"}, want: true},
		{name: "the inline key alone", cfg: ChatConfig{APIKey: "sk-inline"}},
		{name: "the environment channel alone", cfg: ChatConfig{APIKeyEnv: "NIGHTME_TEST_KEY"}},
		{name: "a whitespace-only key", cfg: ChatConfig{APIKey: "   ", APIKeyEnv: "NIGHTME_TEST_KEY"}},
		{name: "no channel", cfg: ChatConfig{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DualCredential(tc.cfg); got != tc.want {
				t.Fatalf("DualCredential = %t, want %t", got, tc.want)
			}
		})
	}
}

// TestMergeHeaders — the configuration's headers go over the ones the
// protocol needs, and an empty value never erases what the protocol
// set: a blank credential is never what an empty header means.
func TestMergeHeaders(t *testing.T) {
	own := map[string]string{
		"Authorization":     "Bearer sk-secret",
		"anthropic-version": "2026-10-21",
	}
	extra := map[string]string{
		"HTTP-Referer":  "https://nightme.dev",
		"Authorization": "",
	}

	got := MergeHeaders(own, extra)
	if got["HTTP-Referer"] != "https://nightme.dev" {
		t.Fatalf("the referer = %q, want the configured one", got["HTTP-Referer"])
	}
	if got["anthropic-version"] != "2026-10-21" {
		t.Fatalf("the version = %q, want the protocol's own", got["anthropic-version"])
	}
	if got["Authorization"] != "Bearer sk-secret" {
		t.Fatalf("the authorization = %q, want the credential — an empty header does not erase it", got["Authorization"])
	}
}

// TestMergeHeaders_KeepsAnEmptyHeaderOfItsOwn — a header the
// protocol did not set goes out empty rather than being dropped.
func TestMergeHeaders_KeepsAnEmptyHeaderOfItsOwn(t *testing.T) {
	got := MergeHeaders(map[string]string{}, map[string]string{"X-Empty": ""})
	value, ok := got["X-Empty"]
	if !ok {
		t.Fatal("the empty header was dropped")
	}
	if value != "" {
		t.Fatalf("the empty header = %q, want it empty", value)
	}
}
