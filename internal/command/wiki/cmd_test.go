package wiki

import "testing"

func TestParseSubcommandOptionsAreOptional(t *testing.T) {
	tests := []struct {
		name string
		argv []string
		want InitOptions
	}{
		{
			name: "default runs every step",
			argv: []string{"init"},
			want: InitOptions{Modules: true, Llmstxt: true, Arch: true},
		},
		{
			name: "modules only",
			argv: []string{"init", "--modules"},
			want: InitOptions{Modules: true, Llmstxt: false, Arch: false},
		},
		{
			name: "llms text only",
			argv: []string{"init", "--llmstxt"},
			want: InitOptions{Modules: false, Llmstxt: true, Arch: false},
		},
		{
			name: "architecture only",
			argv: []string{"init", "--arch"},
			want: InitOptions{Modules: false, Llmstxt: false, Arch: true},
		},
		{
			name: "explicit combination",
			argv: []string{"init", "--modules", "--llmstxt"},
			want: InitOptions{Modules: true, Llmstxt: true, Arch: false},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sub, got, err := parseSubcommand(tt.argv)
			if err != nil {
				t.Fatalf("parseSubcommand() error = %v", err)
			}
			if sub != "init" {
				t.Fatalf("parseSubcommand() subcommand = %q, want %q", sub, "init")
			}
			if got != tt.want {
				t.Fatalf("parseSubcommand() options = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestParseSubcommandRejectsMissingOrExtraArguments(t *testing.T) {
	tests := []struct {
		name string
		argv []string
	}{
		{name: "missing subcommand", argv: nil},
		{name: "extra positional argument", argv: []string{"init", "unexpected"}},
		{name: "unknown option", argv: []string{"init", "--unknown"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, _, err := parseSubcommand(tt.argv); err == nil {
				t.Fatal("parseSubcommand() error = nil, want an error")
			}
		})
	}
}
