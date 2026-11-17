// review_findings.go — the parser that turns one review group's raw
// output into findings.
//
// The output schema belongs to the prompts the agent layer owns
// (the ocr delegate rules' per-finding block, the builtin rubric's
// one-line form, the simplify lens' vocabulary), so the parser lives
// here and not in the command layer.
//
// The parser is deliberately tolerant. It reads the shape the prompt
// asked for and the shapes an agent answers in when it drifts: the
// block form with or without its bullet markers, the field names the
// agent used instead of the prompt's, and a JSON payload. A group
// whose output matches no known shape yields a report with Parsed=false
// and no findings — the caller reports the group as unparsed, and the
// group's raw text still reaches the channel through the coalesced
// stream. Nothing is ever silently discarded: what the parser does not
// understand, the user still reads.

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
	// Line is the finding's line: the start of the range the agent
	// cited ("42" for a "42-50" range).
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

// reviewSeverities maps the severity words the review prompts use —
// and the words an agent reaches for when it drifts from them — onto
// the normalized buckets. The simplify lens' vocabulary folds onto the
// same buckets as the ocr rules' and the builtin rubric's.
var reviewSeverities = map[string]string{
	SeverityCritical: SeverityCritical,
	SeverityHigh:     SeverityHigh,
	SeverityMedium:   SeverityMedium,
	SeverityLow:      SeverityLow,
	"blocking":       SeverityCritical,
	"blocker":        SeverityCritical,
	"fatal":          SeverityCritical,
	"warning":        SeverityHigh,
	"major":          SeverityHigh,
	"severe":         SeverityHigh,
	"moderate":       SeverityMedium,
	"style":          SeverityLow,
	"suggestion":     SeverityLow,
	"minor":          SeverityLow,
	"nit":            SeverityLow,
	"nitpick":        SeverityLow,
	"trivial":        SeverityLow,
	"info":           SeverityLow,
	"informational":  SeverityLow,
}

// NormalizeReviewSeverity maps a severity word onto one of the five
// buckets. The simplify lens' vocabulary folds onto the same buckets
// (blocking → critical, warning → high, style / suggestion → low), and
// so do the words an agent reaches for when it drifts from the
// prompt's (blocker → critical, major → high, minor → low, …); a word
// this parser does not know becomes SeverityUnspecified.
func NormalizeReviewSeverity(s string) string {
	if bucket, ok := reviewSeverities[strings.ToLower(strings.TrimSpace(s))]; ok {
		return bucket
	}
	return SeverityUnspecified
}

// The field names a review agent may use for each finding field.
//
// The names are ordered by preference: the first name of a row wins —
// an output that carries two names for one field takes the first.
var (
	reviewPathKeys     = []string{"path", "file", "filename", "file_path", "location"}
	reviewLineKeys     = []string{"line", "start_line", "line_number", "start"}
	reviewEndLineKeys  = []string{"end_line", "to"}
	reviewSeverityKeys = []string{"severity", "level", "priority"}
	reviewCategoryKeys = []string{"category", "type", "kind", "tag", "rule"}
	reviewContentKeys  = []string{
		"content", "message", "description", "detail", "details",
		"body", "text", "issue", "summary", "title", "comment", "problem",
	}
)

// reviewFieldKind names the finding field a key carries.
type reviewFieldKind int

const (
	reviewFieldLine reviewFieldKind = iota
	reviewFieldEndLine
	reviewFieldSeverity
	reviewFieldCategory
	reviewFieldContent
)

// reviewAlias is a key that names a finding field: the field it
// carries and its rank inside that field's alias list.
type reviewAlias struct {
	kind reviewFieldKind
	rank int
}

// reviewFieldAliases maps every accepted field name onto the field it
// carries.
var reviewFieldAliases = buildReviewFieldAliases()

// reviewPathKeySet is the set of names that open a finding block.
var reviewPathKeySet = buildReviewPathKeySet()

func buildReviewFieldAliases() map[string]reviewAlias {
	m := map[string]reviewAlias{}
	add := func(kind reviewFieldKind, keys []string) {
		for i, k := range keys {
			m[k] = reviewAlias{kind: kind, rank: i}
		}
	}
	add(reviewFieldLine, reviewLineKeys)
	add(reviewFieldEndLine, reviewEndLineKeys)
	add(reviewFieldSeverity, reviewSeverityKeys)
	add(reviewFieldCategory, reviewCategoryKeys)
	add(reviewFieldContent, reviewContentKeys)
	return m
}

func buildReviewPathKeySet() map[string]bool {
	m := make(map[string]bool, len(reviewPathKeys))
	for _, k := range reviewPathKeys {
		m[k] = true
	}
	return m
}

// The shapes a review group answers in.
var (
	// blockKeyRe matches a "key: value" line — the opener of a
	// per-finding block, or one of its fields. The bullet marker is
	// optional: an agent that drops it still answered in the block
	// form.
	blockKeyRe = regexp.MustCompile(`^\s*(?:[-*]\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*:\s*(.*)$`)

	// blockBulletRe matches the bullet marker at the start of a line.
	blockBulletRe = regexp.MustCompile(`^\s*[-*]\s+`)

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
// It recognizes the per-finding block form, the one-line form, the
// simplify lens' severity vocabulary and a JSON payload. Output that
// matches none of them yields a report with Parsed=false and no
// findings; output that says it found nothing ("No findings.") yields
// Parsed=true with zero findings.
func ParseReviewFindings(text string) ReviewReport {
	findings := parseReviewFindings(text)
	if len(findings) == 0 {
		findings = parseReviewFindingsJSON(text)
	}
	report := ReviewReport{Findings: findings}
	report.Parsed = len(findings) > 0 || statesNoFindings(text)
	return report
}

// parseReviewFindings reads the findings the review prompts ask for:
// the per-finding block form and the one-line form.
func parseReviewFindings(text string) []ReviewFinding {
	lines := strings.Split(text, "\n")
	var findings []ReviewFinding
	for i := 0; i < len(lines); i++ {
		if f, next, ok := parseBlockFinding(lines, i); ok {
			findings = append(findings, f)
			i = next
			continue
		}
		if f, ok := parseOneLineFinding(lines[i]); ok {
			findings = append(findings, f)
		}
	}
	return findings
}

// blockLineKind classifies a line of a per-finding block.
type blockLineKind int

const (
	// blockLineOther is a line that names no key at all.
	blockLineOther blockLineKind = iota
	// blockLineField is a line that carries a finding field.
	blockLineField
	// blockLinePath is a path line — it opens the next block.
	blockLinePath
)

// blockLine is one line of a block, classified.
type blockLine struct {
	kind  blockLineKind
	alias reviewAlias
	value string
}

// classifyBlockLine classifies one line: a field line, the path line
// that opens the next block, or a line that names no key.
func classifyBlockLine(line string) blockLine {
	m := blockKeyRe.FindStringSubmatch(line)
	if m == nil {
		return blockLine{kind: blockLineOther}
	}
	key := strings.ToLower(m[1])
	if alias, ok := reviewFieldAliases[key]; ok {
		return blockLine{kind: blockLineField, alias: alias, value: strings.TrimSpace(m[2])}
	}
	if reviewPathKeySet[key] {
		return blockLine{kind: blockLinePath}
	}
	return blockLine{kind: blockLineOther}
}

// blockOpener reads the opener of a per-finding block: a path line.
//
// A quoted value belongs to a JSON payload — the JSON salvage reads
// those, and the block scanner must not claim them. A quoted value on
// a bulleted line is a path the agent quoted: the quotes come off and
// the block is read. A JSON payload's keys are never bulleted, so the
// two never overlap.
func blockOpener(line string) (string, bool) {
	m := blockKeyRe.FindStringSubmatch(line)
	if m == nil {
		return "", false
	}
	if !reviewPathKeySet[strings.ToLower(m[1])] {
		return "", false
	}
	value := strings.TrimSpace(m[2])
	if strings.HasPrefix(value, `"`) || strings.HasPrefix(value, "'") {
		if !blockBulletRe.MatchString(line) {
			return "", false
		}
		unquoted, ok := unquoteValue(value)
		if !ok {
			return "", false
		}
		return unquoted, true
	}
	return value, true
}

// unquoteValue strips one matching pair of surrounding quotes.
func unquoteValue(s string) (string, bool) {
	if len(s) < 2 {
		return "", false
	}
	quote := s[0]
	if (quote != '"' && quote != '\'') || s[len(s)-1] != quote {
		return "", false
	}
	return strings.TrimSpace(s[1 : len(s)-1]), true
}

// parseBlockFinding parses a per-finding block starting at lines[i].
// It returns the finding and the index of the block's last line.
//
// The block's path line opens it; the lines that follow fill its
// fields. A block may omit any field it cannot determine — the
// severity of such a finding is unspecified.
func parseBlockFinding(lines []string, i int) (ReviewFinding, int, bool) {
	path, ok := blockOpener(lines[i])
	if !ok {
		return ReviewFinding{}, i, false
	}
	// A bullet-less opener is prose unless a field line follows it:
	// "path: …" on its own is a sentence, not a block.
	if !blockBulletRe.MatchString(lines[i]) && !nextBlockField(lines, i+1) {
		return ReviewFinding{}, i, false
	}
	fields := reviewFields{path: path}
	j := i + 1
	// The opener and its fields are one unit: a blank line between
	// them does not end the block. The guard above looks for the next
	// non-empty line, so the block has to reach it too.
	for j < len(lines) && strings.TrimSpace(lines[j]) == "" {
		j++
	}
	for j < len(lines) {
		line := classifyBlockLine(lines[j])
		if line.kind != blockLineField {
			break
		}
		j++
		value := line.value
		// A field's value continues onto the following lines while
		// they are indented and name no field.
		for j < len(lines) {
			next := classifyBlockLine(lines[j])
			if next.kind != blockLineOther || !isIndentedLine(lines[j]) {
				break
			}
			value = strings.TrimSpace(value + " " + strings.TrimSpace(lines[j]))
			j++
		}
		fields.set(line.alias, value)
	}
	return fields.finding(), j - 1, true
}

// nextBlockField reports whether the next non-empty line is a field
// line.
func nextBlockField(lines []string, i int) bool {
	for ; i < len(lines); i++ {
		if strings.TrimSpace(lines[i]) == "" {
			continue
		}
		return classifyBlockLine(lines[i]).kind == blockLineField
	}
	return false
}

// isIndentedLine reports whether the line starts with whitespace — the
// mark of a value that continues onto the next line.
func isIndentedLine(line string) bool {
	return strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
}

// reviewValue is one field's value and the rank of the alias that
// carried it.
type reviewValue struct {
	value string
	rank  int
	set   bool
}

// take keeps the value of the better-ranked alias: the first name of
// the field's alias row wins.
func (v *reviewValue) take(rank int, value string) {
	if !v.set || rank < v.rank {
		v.value = value
		v.rank = rank
		v.set = true
	}
}

// reviewFields accumulates the fields of one block.
type reviewFields struct {
	path     string
	line     reviewValue
	endLine  reviewValue
	severity reviewValue
	category reviewValue
	content  reviewValue
}

// set records one field's value.
func (f *reviewFields) set(alias reviewAlias, value string) {
	switch alias.kind {
	case reviewFieldLine:
		f.line.take(alias.rank, value)
	case reviewFieldEndLine:
		f.endLine.take(alias.rank, value)
	case reviewFieldSeverity:
		f.severity.take(alias.rank, value)
	case reviewFieldCategory:
		f.category.take(alias.rank, value)
	case reviewFieldContent:
		f.content.take(alias.rank, value)
	}
}

// finding assembles the finding the block describes.
func (f *reviewFields) finding() ReviewFinding {
	out := ReviewFinding{
		Path:     f.path,
		Severity: SeverityUnspecified,
		Category: f.category.value,
		Content:  f.content.value,
	}
	out.Line = f.line.value
	if out.Line == "" {
		out.Line = f.endLine.value
	}
	if f.severity.set {
		out.Severity = NormalizeReviewSeverity(f.severity.value)
	}
	return out
}

// parseOneLineFinding parses the one-line finding form.
//
// The bold form is taken as written — the bold marker is the agent
// naming a severity. The plain form is only taken when its first word
// is a severity this parser knows, so a stray "- Note: …" bullet does
// not become a finding.
//
// Both forms must carry a location: the prompts ask for
// "<file>:<line> — <issue>", and a bullet without one is prose — the
// severity rubric the builtin prompt spells out is such a bullet.
func parseOneLineFinding(line string) (ReviewFinding, bool) {
	if m := oneLineRe.FindStringSubmatch(line); m != nil {
		return oneLineFinding(m[1], m[2])
	}
	m := oneLinePlainRe.FindStringSubmatch(line)
	if m == nil {
		return ReviewFinding{}, false
	}
	if isReviewFieldName(m[1]) || !isKnownReviewSeverity(m[1]) {
		return ReviewFinding{}, false
	}
	return oneLineFinding(m[1], m[2])
}

// oneLineFinding builds a finding from a severity word and the rest of
// the line: "<file>:<line> — <issue>". A line without a location is
// not a finding.
func oneLineFinding(severity, rest string) (ReviewFinding, bool) {
	loc, content := splitLocation(rest)
	path, line := splitPathLine(loc)
	if line == "" {
		return ReviewFinding{}, false
	}
	return ReviewFinding{
		Path:     path,
		Line:     line,
		Severity: NormalizeReviewSeverity(severity),
		Content:  content,
	}, true
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
	word = strings.ToLower(strings.TrimSpace(word))
	if _, ok := reviewFieldAliases[word]; ok {
		return true
	}
	return reviewPathKeySet[word]
}

// isKnownReviewSeverity reports whether word is a severity this parser
// recognizes.
func isKnownReviewSeverity(word string) bool {
	_, ok := reviewSeverities[strings.ToLower(strings.TrimSpace(word))]
	return ok
}

// reviewNoFindingsRe matches a line that says the group found
// nothing.
//
// The statement has to BE the line. A group that mentions "no
// findings" in passing and reports real findings elsewhere must not
// be read as a group that found nothing — that would drop its
// findings from the counts and hide the group from the re-emit.
var reviewNoFindingsRe = regexp.MustCompile(`(?i)^\s*(?:i\s+(?:found|see)\s+|there\s+(?:are|were|is)\s+)?` +
	`(?:no\s+findings` +
	`|no\s+blockers(?:\s*;\s*nothing\s+material\s+to\s+flag)?` +
	`|nothing\s+material\s+to\s+flag` +
	`|nothing\s+to\s+(?:flag|report)` +
	`|no\s+problems(?:\s+found)?` +
	`|found\s+no\s+problems` +
	`|no\s+issues(?:\s+found)?` +
	`|found\s+no\s+issues` +
	`)\s*[.!]?\s*$`)

// statesNoFindings reports whether the output says the group found
// nothing — the shape the prompts ask for when there is no finding.
func statesNoFindings(text string) bool {
	for _, line := range strings.Split(text, "\n") {
		if reviewNoFindingsRe.MatchString(line) {
			return true
		}
	}
	return false
}
