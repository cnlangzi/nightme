// review_findings.go — the parser that turns one review group's raw
// output into findings.
//
// The output schema belongs to the prompts the agent layer owns
// (the ocr delegate rules' per-finding block, the builtin rubric's
// one-line form, the simplify lens' vocabulary), so the parser lives
// here and not in the command layer.
//
// The parser is deliberately tolerant. A group whose output matches no
// known shape yields a report with Parsed=false and no findings — the
// caller reports the group as unparsed, and the group's raw text still
// reaches the channel through the coalesced stream. Nothing is ever
// silently discarded: what the parser does not understand, the user
// still reads.

package agent

import (
	"regexp"
	"strings"
)

// The normalized severity buckets a finding is counted under.
//
// The review prompts use two vocabularies: the ocr delegate rules and
// the builtin rubric write critical / high / medium / low, while the
// simplify lens writes blocking / warning / style / suggestion. Both
// land on the same five buckets.
const (
	// SeverityCritical is a finding that would break in production or
	// lose data.
	SeverityCritical = "critical"
	// SeverityHigh is a real bug or footgun with user-visible impact.
	SeverityHigh = "high"
	// SeverityMedium is a quality / maintainability issue.
	SeverityMedium = "medium"
	// SeverityLow is a nit — subjective, pedantic or stylistic.
	SeverityLow = "low"
	// SeverityUnspecified is the bucket of a finding whose severity
	// the agent did not state, or stated in a way this parser does
	// not recognize.
	SeverityUnspecified = "unspecified"
)

// SeverityOrder returns the normalized severity buckets from the most
// to the least severe. The last bucket is the "could not tell" one.
func SeverityOrder() []string {
	return []string{
		SeverityCritical,
		SeverityHigh,
		SeverityMedium,
		SeverityLow,
		SeverityUnspecified,
	}
}

// ReviewFinding is one finding of one group's review output.
type ReviewFinding struct {
	// Path is the file the finding is about.
	Path string
	// Line is the finding's line, as the agent wrote it ("42" for a
	// single line, "42" for a "42-50" range).
	Line string
	// Severity is the normalized severity bucket — one of the
	// Severity* constants.
	Severity string
	// Category is the category the agent assigned, when it named one.
	Category string
	// Content is the finding's text.
	Content string
}

// ReviewReport is the parsed form of one group's review output.
type ReviewReport struct {
	// Findings are the findings the parser recognized.
	Findings []ReviewFinding
	// Parsed reports whether the output matched a known shape. A
	// group whose output matched none yields Parsed=false and no
	// findings — the caller reports it as unparsed.
	Parsed bool
}

// NormalizeReviewSeverity maps a severity word onto one of the five
// buckets. The simplify lens' vocabulary folds onto the same buckets
// (blocking → critical, warning → high, style / suggestion → low);
// a word this parser does not know becomes SeverityUnspecified.
func NormalizeReviewSeverity(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case SeverityCritical:
		return SeverityCritical
	case SeverityHigh:
		return SeverityHigh
	case SeverityMedium:
		return SeverityMedium
	case SeverityLow:
		return SeverityLow
	case "blocking":
		return SeverityCritical
	case "warning":
		return SeverityHigh
	case "style":
		return SeverityLow
	case "suggestion":
		return SeverityLow
	default:
		return SeverityUnspecified
	}
}

// The shapes the review prompts ask for. The block form is the ocr
// delegate rules' schema; the one-line form is the builtin rubric's.
var (
	// blockStartRe matches the first line of a per-finding block.
	blockStartRe = regexp.MustCompile(`^\s*[-*]\s+path\s*:\s*(.*)$`)

	// blockFieldRe matches the fields that follow a block's path line.
	blockFieldRe = regexp.MustCompile(`^\s*[-*]\s+(start_line|end_line|category|severity|content)\s*:\s*(.*)$`)

	// oneLineRe matches the one-line finding form with a bold
	// severity: "- **critical**: <file>:<line> — <issue>".
	oneLineRe = regexp.MustCompile(`^\s*[-*]\s+\*\*([A-Za-z][A-Za-z_-]*)\*\*\s*:\s*(.+)$`)

	// oneLinePlainRe matches the one-line finding form without the
	// bold marker: "- critical: <file>:<line> — <issue>".
	oneLinePlainRe = regexp.MustCompile(`^\s*[-*]\s+([A-Za-z][A-Za-z_-]*)\s*:\s*(.+)$`)

	// lineRangeRe matches a line reference, single or ranged.
	lineRangeRe = regexp.MustCompile(`^(\d+)(-\d+)?$`)
)

// ParseReviewFindings parses one group's raw review output into
// findings.
//
// It recognizes the per-finding block form, the one-line form, and the
// simplify lens' severity vocabulary. Output that matches none of them
// yields a report with Parsed=false and no findings; output that says
// it found nothing ("No blockers; nothing material to flag.") yields
// Parsed=true with zero findings.
func ParseReviewFindings(text string) ReviewReport {
	var report ReviewReport
	lines := strings.Split(text, "\n")
	for i := 0; i < len(lines); i++ {
		if f, next, ok := parseBlockFinding(lines, i); ok {
			report.Findings = append(report.Findings, f)
			i = next
			continue
		}
		if f, ok := parseOneLineFinding(lines[i]); ok {
			report.Findings = append(report.Findings, f)
		}
	}
	report.Parsed = len(report.Findings) > 0 || statesNoFindings(text)
	return report
}

// parseBlockFinding parses a per-finding block starting at lines[i].
// It returns the finding and the index of the block's last line.
//
// The block's path line opens it; the lines that follow fill its
// fields. A block may omit any field it cannot determine — the
// severity of such a finding is unspecified.
func parseBlockFinding(lines []string, i int) (ReviewFinding, int, bool) {
	m := blockStartRe.FindStringSubmatch(lines[i])
	if m == nil {
		return ReviewFinding{}, i, false
	}
	f := ReviewFinding{Path: strings.TrimSpace(m[1]), Severity: SeverityUnspecified}
	j := i + 1
	for j < len(lines) {
		km := blockFieldRe.FindStringSubmatch(lines[j])
		if km == nil {
			break
		}
		switch km[1] {
		case "start_line":
			f.Line = strings.TrimSpace(km[2])
		case "end_line":
			if f.Line == "" {
				f.Line = strings.TrimSpace(km[2])
			}
		case "category":
			f.Category = strings.TrimSpace(km[2])
		case "severity":
			f.Severity = NormalizeReviewSeverity(km[2])
		case "content":
			f.Content = strings.TrimSpace(km[2])
		}
		j++
	}
	return f, j - 1, true
}

// parseOneLineFinding parses the one-line finding form.
//
// The bold form is taken as written — the bold marker is the agent
// naming a severity. The plain form is only taken when its first word
// is a severity this parser knows, so a stray "- Note: …" bullet does
// not become a finding.
func parseOneLineFinding(line string) (ReviewFinding, bool) {
	if m := oneLineRe.FindStringSubmatch(line); m != nil {
		return oneLineFinding(m[1], m[2]), true
	}
	m := oneLinePlainRe.FindStringSubmatch(line)
	if m == nil {
		return ReviewFinding{}, false
	}
	if isReviewFieldName(m[1]) || !isKnownReviewSeverity(m[1]) {
		return ReviewFinding{}, false
	}
	return oneLineFinding(m[1], m[2]), true
}

// oneLineFinding builds a finding from a severity word and the rest of
// the line: "<file>:<line> — <issue>".
func oneLineFinding(severity, rest string) ReviewFinding {
	loc, content := splitLocation(rest)
	path, line := splitPathLine(loc)
	return ReviewFinding{
		Path:     path,
		Line:     line,
		Severity: NormalizeReviewSeverity(severity),
		Content:  content,
	}
}

// splitLocation splits a one-line finding's remainder into its
// location and its text. The location ends at the em dash that
// separates it from the issue.
func splitLocation(rest string) (loc, content string) {
	loc = strings.TrimSpace(rest)
	i := strings.Index(loc, "—")
	if i < 0 {
		return loc, ""
	}
	return strings.TrimSpace(loc[:i]), strings.TrimSpace(loc[i+len("—"):])
}

// splitPathLine splits "<file>:<line>" into its two parts. A location
// without a numeric line stays a path.
func splitPathLine(loc string) (path, line string) {
	i := strings.LastIndex(loc, ":")
	if i <= 0 {
		return loc, ""
	}
	m := lineRangeRe.FindStringSubmatch(loc[i+1:])
	if m == nil {
		return loc, ""
	}
	return loc[:i], m[1]
}

// isReviewFieldName reports whether word names a field of the
// per-finding block schema — such a word is never a severity.
func isReviewFieldName(word string) bool {
	switch strings.ToLower(word) {
	case "path", "start_line", "end_line", "category", "severity", "content":
		return true
	}
	return false
}

// isKnownReviewSeverity reports whether word is a severity this parser
// recognizes.
func isKnownReviewSeverity(word string) bool {
	switch strings.ToLower(strings.TrimSpace(word)) {
	case SeverityCritical, SeverityHigh, SeverityMedium, SeverityLow,
		"blocking", "warning", "style", "suggestion":
		return true
	}
	return false
}

// statesNoFindings reports whether the output says the group found
// nothing — the shape the prompts ask for when there is no finding.
func statesNoFindings(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "no blockers") ||
		strings.Contains(lower, "nothing material to flag")
}
