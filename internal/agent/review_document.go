package agent

// review_document.go — the merged review document: one section per
// review group, the group's own output under its header.
//
// The document is the main agent's input: it carries what every
// group said, group by group, so a follow-up ("fix the blockers")
// can name the group a finding came from.
//
// The format lives here, in one place. The fan-out builds the
// document of a run with it, and the command layer rebuilds the same
// document from the findings it has cached — a delivery that spans
// several runs must look exactly like the one a single run produces.

import (
	"fmt"
	"strings"
)

// ReviewGroupSection is one group's part of a merged review
// document.
type ReviewGroupSection struct {
	// Pattern is the group's pattern — the glob of an ocr rule
	// group, or the builtin / simplify sentinel.
	Pattern string
	// Files are the files the group covered.
	Files []string
	// Texts are the group's own outputs. A group that ran more than
	// once carries one text per output that belongs in the
	// document.
	Texts []string
	// Err is the error that ended the group, nil on success. A failed
	// group contributes the error, not an output.
	Err error
}

// MergeReviewDocument renders the merged review document of the
// given sections.
//
// The document opens with the number of groups it holds, and every
// group gets one section: a successful one carries the group's own
// output under its header, a failed one carries the error that ended
// it.
func MergeReviewDocument(sections []ReviewGroupSection) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Reviewed %d groups.\n\n", len(sections))
	for _, s := range sections {
		if s.Err != nil {
			fmt.Fprintf(&b, "### Group: pattern %s — failed: %v\n\n", s.Pattern, s.Err)
			continue
		}
		fmt.Fprintf(&b, "### Group: pattern %s — files: %s\n\n", s.Pattern, strings.Join(s.Files, ", "))
		for _, t := range s.Texts {
			b.WriteString(t)
			b.WriteString("\n\n")
		}
	}
	return strings.TrimSpace(b.String())
}
