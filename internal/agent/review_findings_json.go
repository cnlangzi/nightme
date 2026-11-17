// review_findings_json.go — the JSON salvage: the findings a group
// reports as a JSON payload instead of the block form.
//
// The payload is read in two steps. The candidate is cut out of the
// text — a fenced ```json block, or a balanced {...} / [...] region —
// and then repaired step by step until it decodes. The repair is
// progressive: every step is followed by a decode attempt, and the
// first form that decodes wins.
//
// The salvage never invents a finding: a decoded object without a
// path names no file, so it says nothing about the change.

package agent

import (
	"encoding/json"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// jsonFenceRe matches a fenced json block.
var jsonFenceRe = regexp.MustCompile("(?is)```json[ \t]*\r?\n?(.*?)```")

// The repairs applied to a candidate payload, in order.
var jsonRepairSteps = []func(string) string{
	stripJSONFence,
	dropJSONTrailingCommas,
	replaceJSONSmartQuotes,
	quoteJSONKeys,
	doubleQuoteJSONStrings,
}

var (
	// jsonFenceStripRe matches the code fence around a payload.
	jsonFenceStripRe = regexp.MustCompile("(?s)^\\s*```[A-Za-z]*\\s*|```\\s*$")

	// jsonTrailingCommaRe matches a comma that sits before a closing
	// bracket.
	jsonTrailingCommaRe = regexp.MustCompile(`,(\s*[}\]])`)

	// jsonUnquotedKeyRe matches an object key that was not quoted.
	jsonUnquotedKeyRe = regexp.MustCompile(`([{,]\s*)([A-Za-z_][A-Za-z0-9_]*)\s*:`)

	// jsonSingleQuoteRe matches a single-quoted string. A quote pair
	// without a quote or a line break inside it is a string.
	jsonSingleQuoteRe = regexp.MustCompile(`'([^'\n]*)'`)
)

// jsonFindingArrayKeys are the keys an object may carry its findings
// under.
var jsonFindingArrayKeys = []string{"findings", "issues", "results", "items"}

// parseReviewFindingsJSON reads the findings a group reported as a
// JSON payload.
func parseReviewFindingsJSON(text string) []ReviewFinding {
	for _, candidate := range jsonCandidates(text) {
		value, ok := decodeJSONCandidate(candidate)
		if !ok {
			continue
		}
		if findings := jsonFindings(value); len(findings) > 0 {
			return findings
		}
	}
	return nil
}

// jsonCandidates returns the payloads worth trying, in order: the
// fenced json blocks of the text, then its balanced regions.
func jsonCandidates(text string) []string {
	var out []string
	for _, m := range jsonFenceRe.FindAllStringSubmatch(text, -1) {
		if candidate := strings.TrimSpace(m[1]); candidate != "" {
			out = append(out, candidate)
		}
	}
	for _, region := range jsonRegions(text) {
		if !jsonPayloadPosition(text, region[0], region[1]) {
			continue
		}
		out = append(out, text[region[0]:region[1]])
	}
	return out
}

// jsonPayloadPosition reports whether the region at [start, end) sits
// where a payload sits: it opens a line, or it follows a colon that
// introduces it, and nothing follows it on its line.
//
// A region quoted inside a sentence is prose — a config file the
// agent talks about is not a finding.
func jsonPayloadPosition(text string, start, end int) bool {
	lineStart := strings.LastIndexByte(text[:start], '\n') + 1
	prefix := strings.TrimSpace(text[lineStart:start])
	if prefix != "" && !strings.HasSuffix(prefix, ":") && !strings.HasSuffix(prefix, "=") {
		return false
	}
	lineEnd := strings.IndexByte(text[end:], '\n')
	if lineEnd < 0 {
		lineEnd = len(text)
	} else {
		lineEnd += end
	}
	return strings.TrimSpace(text[end:lineEnd]) == ""
}

// jsonRegions returns the balanced {...} and [...] regions of the
// text, largest first.
//
// Every matched pair is a region: a stray brace in the prose before
// the payload must not swallow the payload that follows it. The
// regions come largest first — the payload that holds the findings is
// the biggest one.
func jsonRegions(text string) [][2]int {
	var regions [][2]int
	var stack []int
	inString := false
	escaped := false
	for i, r := range text {
		switch {
		case inString:
			switch {
			case escaped:
				escaped = false
			case r == '\\':
				escaped = true
			case r == '"':
				inString = false
			}
		case r == '"':
			inString = true
		case r == '{' || r == '[':
			stack = append(stack, i)
		case r == '}' || r == ']':
			if len(stack) == 0 {
				continue
			}
			open := stack[len(stack)-1]
			stack = stack[:len(stack)-1]
			regions = append(regions, [2]int{open, i + 1})
		}
	}
	slices.SortFunc(regions, func(a, b [2]int) int {
		return (b[1] - b[0]) - (a[1] - a[0])
	})
	return regions
}

// decodeJSONCandidate decodes a candidate payload, repairing it step
// by step until it decodes.
func decodeJSONCandidate(candidate string) (any, bool) {
	current := candidate
	for step := 0; ; step++ {
		if value, ok := decodeJSONValue(current); ok {
			return value, true
		}
		if step >= len(jsonRepairSteps) {
			return nil, false
		}
		current = jsonRepairSteps[step](current)
	}
}

// decodeJSONValue decodes a JSON payload.
func decodeJSONValue(s string) (any, bool) {
	var value any
	if err := json.Unmarshal([]byte(s), &value); err != nil {
		return nil, false
	}
	return value, true
}

// stripJSONFence removes the code fence around a payload.
func stripJSONFence(s string) string {
	return jsonFenceStripRe.ReplaceAllString(s, "")
}

// dropJSONTrailingCommas removes the commas that sit before a closing
// bracket.
func dropJSONTrailingCommas(s string) string {
	return jsonTrailingCommaRe.ReplaceAllString(s, "${1}")
}

// replaceJSONSmartQuotes turns the typographic quotes into their ASCII
// equivalents.
func replaceJSONSmartQuotes(s string) string {
	return strings.NewReplacer("’", "'", "‘", "'", "“", `"`, "”", `"`).Replace(s)
}

// quoteJSONKeys quotes the object keys that were not quoted.
func quoteJSONKeys(s string) string {
	return jsonUnquotedKeyRe.ReplaceAllString(s, `${1}"${2}":`)
}

// doubleQuoteJSONStrings turns the single-quoted strings into
// double-quoted ones.
func doubleQuoteJSONStrings(s string) string {
	return jsonSingleQuoteRe.ReplaceAllString(s, `"${1}"`)
}

// jsonFindings maps a decoded payload onto findings. The payload is a
// single finding, an array of findings, or an object carrying a
// findings array.
func jsonFindings(value any) []ReviewFinding {
	switch payload := value.(type) {
	case []any:
		return jsonFindingList(payload)
	case map[string]any:
		for _, key := range jsonFindingArrayKeys {
			if items, ok := payload[key].([]any); ok {
				return jsonFindingList(items)
			}
		}
		if finding, ok := jsonFinding(payload); ok {
			return []ReviewFinding{finding}
		}
	}
	return nil
}

// jsonFindingList maps a decoded array onto findings.
func jsonFindingList(items []any) []ReviewFinding {
	var out []ReviewFinding
	for _, item := range items {
		if finding, ok := jsonFinding(item); ok {
			out = append(out, finding)
		}
	}
	return out
}

// jsonFinding maps one decoded object onto a finding.
//
// A finding without a path is not a finding: it names no file, so it
// says nothing about the change.
func jsonFinding(item any) (ReviewFinding, bool) {
	object, ok := item.(map[string]any)
	if !ok {
		return ReviewFinding{}, false
	}
	finding := ReviewFinding{Severity: SeverityUnspecified}
	finding.Path = jsonString(object, reviewPathKeys)
	if finding.Path == "" {
		return ReviewFinding{}, false
	}
	finding.Line = jsonLine(object, reviewLineKeys)
	if finding.Line == "" {
		finding.Line = jsonLine(object, reviewEndLineKeys)
	}
	if severity := jsonString(object, reviewSeverityKeys); severity != "" {
		finding.Severity = NormalizeReviewSeverity(severity)
	}
	finding.Category = jsonString(object, reviewCategoryKeys)
	finding.Content = jsonString(object, reviewContentKeys)
	return finding, true
}

// jsonString returns the value of the first of the keys that carries a
// non-empty string.
func jsonString(object map[string]any, keys []string) string {
	for _, key := range keys {
		if value, ok := object[key].(string); ok {
			if trimmed := strings.TrimSpace(value); trimmed != "" {
				return trimmed
			}
		}
	}
	return ""
}

// jsonLine returns the line of the first of the keys that carries one.
// The line may arrive as a JSON number or as a string.
func jsonLine(object map[string]any, keys []string) string {
	for _, key := range keys {
		switch value := object[key].(type) {
		case string:
			if line := jsonLineText(value); line != "" {
				return line
			}
		case float64:
			return strconv.FormatFloat(value, 'f', -1, 64)
		}
	}
	return ""
}

// jsonLineText normalizes a line reference: a range keeps its first
// line, like the block form does. A reference that is not a number
// names no line.
func jsonLineText(s string) string {
	s = strings.TrimSpace(s)
	m := lineRangeRe.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	return m[1]
}
