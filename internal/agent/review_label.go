// review_label.go — how a review group is named for the reader.
//
// A group is named by what it reviews: nightme's own built-in rubric,
// the simplify lens, or the ocr rule it came from — the rule's source
// and pattern. The name is followed by a summary of the files the
// group covers, so the reader knows what kind of code the findings are
// about before reading them.

package agent

import (
	"path/filepath"
	"sort"
	"strings"
)

// ReviewGroupName is the name of a review group: what it reviews.
//
// The nightme-owned groups get a fixed name — their sentinel pattern is
// an internal marker and must not reach the chat. An ocr group is
// named by its rule's source and pattern: the rule's own text is not
// used, because its first line describes only the rule's first
// category while the group checks every category of that rule.
//
// The source and the pattern together are the group's identity: two
// rules over the same glob are two groups.
func ReviewGroupName(source, pattern string) string {
	switch pattern {
	case PatternBuiltin:
		return "nightme code reviewer"
	case PatternSimplify:
		return simplifyGroupName()
	}
	if source != "" {
		return source + " / " + pattern
	}
	return pattern
}

// reviewGroupName is the name of the group the plan describes.
func reviewGroupName(g reviewGroup) string {
	return ReviewGroupName(g.Source, g.Pattern)
}

// CodeSpan renders a group's name for the chat: a code span when the
// name carries a markdown metacharacter — `*` (bold, italic, glob) or
// `_` (italic). The plan's label — the name plus the file summary —
// goes through the same rule.
//
// A glob's `**` pairs with another name's `**` into a bold span and
// swallows the text between them, so a name that carries one must not
// sit in the markdown stream bare.
//
// A name that carries a backtick is left alone: a code span would
// break on it.
func CodeSpan(name string) string {
	if strings.ContainsAny(name, "*_") && !strings.Contains(name, "`") {
		return "`" + name + "`"
	}
	return name
}

// simplifyGroupName is the simplify group's name: the heading of its
// prompt, without the internal marker the heading carries.
func simplifyGroupName() string {
	head, _, _ := strings.Cut(simplifyPrompt, "\n")
	head = strings.TrimSpace(strings.TrimPrefix(head, "# "))
	if i := strings.Index(head, " ("); i > 0 {
		head = head[:i]
	}
	return head
}

// reviewGroupLabel is the name the user reads: the group's name and
// the summary of the files it covers.
func reviewGroupLabel(g reviewGroup) string {
	name := reviewGroupName(g)
	summary := reviewFileSummary(g.Files)
	if summary == "" {
		return name
	}
	return name + " · " + summary
}

// reviewSummaryExtensions is how many extensions the file summary
// names before it falls back to 等.
const reviewSummaryExtensions = 3

// reviewFileSummary summarizes the files a group covers by their
// extensions: the most frequent first, at most three of them, the rest
// as 等.
//
// A file without an extension contributes its own name — there is no
// extension to count it under.
func reviewFileSummary(files []string) string {
	counts := make(map[string]int, len(files))
	var names []string
	for _, f := range files {
		name := fileExtensionName(f)
		if counts[name] == 0 {
			names = append(names, name)
		}
		counts[name]++
	}
	sort.SliceStable(names, func(i, j int) bool {
		return counts[names[i]] > counts[names[j]]
	})
	if len(names) <= reviewSummaryExtensions {
		return strings.Join(names, ", ")
	}
	return strings.Join(names[:reviewSummaryExtensions], ", ") + " 等"
}

// fileExtensionName is the name the summary counts a file under: the
// name its extension is known by, or the file's own name when it has
// no extension.
func fileExtensionName(path string) string {
	ext := filepath.Ext(path)
	if ext == "" {
		return filepath.Base(path)
	}
	ext = strings.TrimPrefix(ext, ".")
	if name, ok := reviewExtensionNames[ext]; ok {
		return name
	}
	return ext
}

// reviewExtensionNames maps a file extension to the name a reader
// knows it by. An extension that is not in the map keeps its own name.
var reviewExtensionNames = map[string]string{
	"c":     "C",
	"cc":    "C++",
	"cpp":   "C++",
	"cs":    "C#",
	"css":   "CSS",
	"dart":  "Dart",
	"ex":    "Elixir",
	"exs":   "Elixir",
	"go":    "Go",
	"h":     "C",
	"hpp":   "C++",
	"hs":    "Haskell",
	"html":  "HTML",
	"java":  "Java",
	"js":    "JavaScript",
	"json":  "JSON",
	"jsx":   "JavaScript",
	"kt":    "Kotlin",
	"lua":   "Lua",
	"md":    "Markdown",
	"php":   "PHP",
	"proto": "Protobuf",
	"py":    "Python",
	"rb":    "Ruby",
	"rs":    "Rust",
	"scala": "Scala",
	"scss":  "SCSS",
	"sh":    "Shell",
	"sql":   "SQL",
	"swift": "Swift",
	"toml":  "TOML",
	"ts":    "TypeScript",
	"tsx":   "TypeScript",
	"vue":   "Vue",
	"xml":   "XML",
	"yaml":  "YAML",
	"yml":   "YAML",
	"zig":   "Zig",
}
