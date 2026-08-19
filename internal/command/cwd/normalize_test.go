package cwd

import (
	"strings"
	"testing"
)

// TestNormalizePathInput covers the full mapping table in
// normalize.go. Each rule is exercised as a separate case so
// a regression in one row shows up as a single failure.
func TestNormalizePathInput(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		// Identity cases — must not be touched.
		{"empty", "", ""},
		{"plain ascii path", "/usr/local/bin", "/usr/local/bin"},
		{"windows absolute", `C:\Users\name\project`, `C:\Users\name\project`},
		{"tilde path", "~/projects", "~/projects"},
		{"relative path", "foo/bar", "foo/bar"},
		{"single dot", ".", "."},
		{"digits and letters", "abc123XYZ", "abc123XYZ"},
		// CJK ideographs pass through — we don't transliterate.
		{"cjk ideograph", "我的项目", "我的项目"},
		{"cjk ideograph with path", "/home/我的项目", "/home/我的项目"},

		// Full-width ASCII block (U+FF01..U+FF5E) → half-width.
		{"full-width slash", "／foo/bar", "/foo/bar"},
		{"full-width colon", `C：\Users\name`, `C:\Users\name`},
		{"full-width semicolon", "a；b", "a;b"},
		{"full-width parens", "（foo）", "(foo)"},
		{"full-width comma", "a，b", "a,b"},
		{"full-width question", "what？", "what?"},
		{"full-width exclamation", "wow！", "wow!"},
		{"full-width quotes", "“foo”", `"foo"`},
		{"full-width apostrophe", "‘foo’", `'foo'`},
		{"full-width brackets", "【foo】", "[foo]"},
		{"full-width angle brackets", "《foo》", "<foo>"},
		{"full-width less-than", "a＜b", "a<b"},
		{"full-width greater-than", "a＞b", "a>b"},

		// Full-width space (U+3000) → ASCII space.
		{"full-width space", "　/foo", " /foo"},
		{"multiple fw spaces", "　　foo", "  foo"},

		// CJK punctuation without ASCII counterpart.
		{"chinese period", "foo。", "foo."},
		{"chinese enumeration comma", "a、b、c", "a,b,c"},
		{"em-dash", "foo—bar", "foo-bar"},
		{"ellipsis", "wait…", "wait..."},

		// Mixed: the realistic IME-mangled path.
		{"mixed path", `／Users／name：项目（１）`, `/Users/name:项目(1)`},
		{"windows path full-width", `“C：\Program Files（x86）”`, `"C:\Program Files(x86)"`},

		// Edge cases that must NOT be remapped.
		{"ascii slash untouched", "/foo/bar/baz", "/foo/bar/baz"},
		{"plain colon untouched", "a:b:c", "a:b:c"},
		{"backtick untouched", "`foo`", "`foo`"},
		{"emoji untouched", "🎉", "🎉"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizePathInput(tc.in)
			if got != tc.want {
				t.Errorf("normalizePathInput(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestNormalizePathInput_Idempotent verifies that running
// normalise twice produces the same result as running it
// once. This matters because the result is fed back into
// the parser; if normalisation were destructive we'd see
// drift on every invocation.
func TestNormalizePathInput_Idempotent(t *testing.T) {
	inputs := []string{
		"／foo：bar。",
		"“我的项目”",
		`（C：\foo）`,
		"　／usr／local／bin　",
	}
	for _, in := range inputs {
		once := normalizePathInput(in)
		twice := normalizePathInput(once)
		if once != twice {
			t.Errorf("normalize not idempotent: %q → %q → %q", in, once, twice)
		}
	}
}

// TestNormalizePathInput_FullBlockSweep walks the entire
// full-width ASCII range (U+FF01..U+FF5E) and asserts each
// code point maps to its expected half-width counterpart.
// This is the regression guard for the "r - 0xFEE0" trick.
func TestNormalizePathInput_FullBlockSweep(t *testing.T) {
	var b strings.Builder
	for r := rune(0xFF01); r <= 0xFF5E; r++ {
		b.WriteRune(r)
	}
	got := normalizePathInput(b.String())

	var want strings.Builder
	for r := rune(0x21); r <= 0x7E; r++ {
		want.WriteRune(r)
	}
	if got != want.String() {
		t.Errorf("full-block normalisation wrong:\n got  %q\n want %q", got, want.String())
	}
}

// TestNormalizePathInput_IMRichText strips IM-emitted link
// markup. Without this, pasting a URL into the chat input
// causes /cwd to receive "<a href="...">visible text</a>"
// (because feishu/lark/slack/teams all wrap links). The path
// resolver rejects the raw markup; stripping extracts the
// visible text so the user's intent is preserved.
func TestNormalizePathInput_IMRichText(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "feishu link card (quoted href)",
			in:   `<a href="https://github.com/cnlangzi/nightme">cnlangzi/nightme</a>`,
			want: "cnlangzi/nightme",
		},
		{
			name: "feishu link card (single-quoted href)",
			in:   `<a href='https://github.com/cnlangzi/nightme'>cnlangzi/nightme</a>`,
			want: "cnlangzi/nightme",
		},
		{
			name: "link card with full path inside label",
			in:   `<a href="https://example.com">/Users/geax/code/myproj</a>`,
			want: "/Users/geax/code/myproj",
		},
		{
			name: "link card with no label falls back to href",
			in:   `<a href="/Users/geax/code/myproj"></a>`,
			want: "/Users/geax/code/myproj",
		},
		{
			name: "no markup, untouched",
			in:   "/Users/geax/code/myproj",
			want: "/Users/geax/code/myproj",
		},
		{
			name: "empty link tag is removed",
			in:   `<a href=""></a>`,
			want: "",
		},
		{
			name: "link tag with extra attributes (feishu posts)",
			in:   `<a href="https://github.com/cnlangzi/nightme" target="_blank">cnlangzi/nightme</a>`,
			want: "cnlangzi/nightme",
		},
		{
			name: "full-width slash inside link",
			// After IM mangling: full-width ／ becomes / via
			// the existing normalize table; the link strip
			// runs first (extracts visible text), then the
			// full-width map runs on the extracted text.
			in:   `<a href="https://github.com/cnlangzi/nightme">／Users／geax</a>`,
			want: "/Users/geax",
		},
		{
			name: "uppercase tag (Slack / Teams occasionally emit <A ...>)",
			in:   `<A HREF="https://example.com">/Users/geax/code</A>`,
			want: "/Users/geax/code",
		},
		{
			name: "mixed case tag (the regex matches case-insensitively)",
			in:   `<A href="/Users/geax/code">x</a>`,
			want: "x",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := normalizePathInput(tc.in)
			if got != tc.want {
				t.Errorf("normalizePathInput(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
