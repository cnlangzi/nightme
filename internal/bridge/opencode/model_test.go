// Package opencode — effective model resolution tests.
package opencode

import "testing"

// TestParseEffectiveModel feeds the bridge the JSON `opencode debug
// config` prints and asserts the model it resolves.
func TestParseEffectiveModel(t *testing.T) {
	cases := []struct {
		name string
		json string
		want string
	}{
		{
			name: "object form",
			json: `[
				{
					"type": "document",
					"path": "/home/devin/.config/opencode/opencode.jsonc",
					"info": {
						"model": { "providerID": "minimax-cn", "model": "MiniMax-M3" }
					}
				},
				{ "type": "directory", "path": "/home/devin/.config/opencode" }
			]`,
			want: "minimax-cn/MiniMax-M3",
		},
		{
			name: "string form",
			json: `[
				{
					"type": "document",
					"path": "/home/devin/.config/opencode/opencode.json",
					"info": { "model": "minimax-cn/MiniMax-M3" }
				}
			]`,
			want: "minimax-cn/MiniMax-M3",
		},
		{
			name: "variant",
			json: `[
				{
					"type": "document",
					"path": "/home/devin/.config/opencode/opencode.json",
					"info": {
						"model": { "providerID": "minimax-cn", "model": "MiniMax-M3", "variant": "fast" }
					}
				}
			]`,
			want: "minimax-cn/MiniMax-M3/fast",
		},
		{
			name: "variant in the string form",
			json: `[
				{
					"type": "document",
					"path": "/home/devin/.config/opencode/opencode.json",
					"info": { "model": "minimax-cn/MiniMax-M3#fast" }
				}
			]`,
			want: "minimax-cn/MiniMax-M3/fast",
		},
		{
			name: "last source wins",
			json: `[
				{
					"type": "document",
					"path": "/home/devin/.config/opencode/opencode.jsonc",
					"info": { "model": { "providerID": "global", "model": "one" } }
				},
				{ "type": "directory", "path": "/home/devin/.config/opencode" },
				{
					"type": "document",
					"path": "/work/project/opencode.json",
					"info": { "model": { "providerID": "project", "model": "two" } }
				}
			]`,
			want: "project/two",
		},
		{
			name: "a document without a model keeps the previous one",
			json: `[
				{
					"type": "document",
					"path": "/home/devin/.config/opencode/opencode.jsonc",
					"info": { "model": { "providerID": "global", "model": "one" } }
				},
				{
					"type": "document",
					"path": "/work/project/opencode.json",
					"info": { "theme": "dark" }
				}
			]`,
			want: "global/one",
		},
		{
			name: "no model configured",
			json: `[
				{
					"type": "document",
					"path": "/home/devin/.config/opencode/opencode.jsonc",
					"info": { "$schema": "https://opencode.ai/config.json" }
				},
				{ "type": "directory", "path": "/home/devin/.config/opencode" }
			]`,
			want: "",
		},
		{
			name: "unparseable output",
			json: "not json at all",
			want: "",
		},
		{
			name: "empty output",
			json: "",
			want: "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parseEffectiveModel([]byte(tc.json)); got != tc.want {
				t.Errorf("parseEffectiveModel() = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestParseEffectiveModel_IgnoresStrayOutput verifies the parser
// survives a line printed around the JSON list.
func TestParseEffectiveModel_IgnoresStrayOutput(t *testing.T) {
	out := "some log line\n[{\"type\":\"document\",\"info\":{\"model\":{\"providerID\":\"p\",\"model\":\"m\"}}}]\n"
	if got := parseEffectiveModel([]byte(out)); got != "p/m" {
		t.Errorf("parseEffectiveModel() = %q, want p/m", got)
	}
}
