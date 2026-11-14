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
		cfg  Config
		want string
		err  string
	}{
		{
			name: "the inline key wins",
			cfg:  Config{APIKey: "sk-inline", APIKeyEnv: "NIGHTME_TEST_KEY"},
			want: "sk-inline",
		},
		{
			name: "the inline key is trimmed",
			cfg:  Config{APIKey: "  sk-x  "},
			want: "sk-x",
		},
		{
			name: "a whitespace-only key counts as unset",
			cfg:  Config{APIKey: "   ", APIKeyEnv: "NIGHTME_TEST_KEY"},
			want: "sk-env",
		},
		{
			name: "the environment channel alone",
			cfg:  Config{APIKeyEnv: "NIGHTME_TEST_KEY"},
			want: "sk-env",
		},
		{
			name: "the inline key covers an unset variable",
			cfg:  Config{APIKey: "sk-inline", APIKeyEnv: "NIGHTME_TEST_EMPTY"},
			want: "sk-inline",
		},
		{
			name: "no channel means no credential",
			cfg:  Config{},
			want: "",
		},
		{
			name: "an unset variable is an error",
			cfg:  Config{APIKeyEnv: "NIGHTME_TEST_EMPTY"},
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
		cfg  Config
		want bool
	}{
		{name: "both channels", cfg: Config{APIKey: "sk-inline", APIKeyEnv: "NIGHTME_TEST_KEY"}, want: true},
		{name: "the inline key alone", cfg: Config{APIKey: "sk-inline"}},
		{name: "the environment channel alone", cfg: Config{APIKeyEnv: "NIGHTME_TEST_KEY"}},
		{name: "a whitespace-only key", cfg: Config{APIKey: "   ", APIKeyEnv: "NIGHTME_TEST_KEY"}},
		{name: "no channel", cfg: Config{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DualCredential(tc.cfg); got != tc.want {
				t.Fatalf("DualCredential = %t, want %t", got, tc.want)
			}
		})
	}
}
