// Package format provides shared rendering helpers for slash
// command reply text (IM-friendly plain-text payloads).
//
// Both /close's per-entry table and /new's per-entry table render
// a header line + a sorted list of result rows. They share the
// byte cap (Feishu's 4 KB single-message limit), the
// "...and N more" tail, the typed-priority sort, and the row
// shape. This package centralizes those so neither close nor
// newcmd needs to import the other for rendering.
//
// Specific commands still own the wording of their headers and
// row-render functions — format only knows about the byte cap,
// sort, and tail helpers, plus the RenderedRow output struct
// that the row-render callback returns.
//
// Lives under internal/command/ as a sibling of close / newcmd /
// stop — the helpers are shared across command packages, not
// specific to any one command.
package format

import (
	"fmt"
	"sort"
)

// ReplyByteCap is the Feishu single-message payload limit.
// Both /close and /new format strings cap here to keep the
// channel side from rejecting the message outright.
const ReplyByteCap = 4096

// TailFmt is the "...and N more" suffix appended when the
// output would otherwise exceed the byte cap.
const TailFmt = "  ... and %d more"

// RowBucket is the typed priority bucket for a formatter row.
// Lower values sort first. success < failure < skipped so the
// user sees ✓ first, then ✗, then • (previous alphabetical sort
// on the rendered strings placed `•` (U+2022) before `✓`
// (U+2713) before `✗` (U+2717), inverting the spec).
type RowBucket int

const (
	BucketSuccess RowBucket = iota
	BucketFailure
	BucketSkipped
)

// RenderedRow is one formatted line + the structured fields
// the sorter needs. Produced by the row-render callback
// passed to FormatTable.
type RenderedRow struct {
	Text   string
	Bucket RowBucket
	Agent  string
	Cwd    string
}

// Render is the per-row callback callers provide. It maps one
// of their result types to a formatted row + sort bucket.
//
// T is intentionally unconstrained (any) — callers' result
// types don't need to implement any interface; the callback
// just constructs a RenderedRow from whatever data the type
// carries. This keeps the call-site boilerplate to "one
// function" per command.
type Render[T any] func(T) RenderedRow

// HeaderBuilder builds the top-of-reply line from the (success,
// skipped, failed) counts. Specific commands own the wording.
type HeaderBuilder func(success, skipped, failed int) string

// FormatTable renders rows as an IM-friendly table:
//
//   - Calls render on each row to produce text + bucket
//   - Sorts by (bucket asc, agent, cwd) — typed-priority order
//   - Builds the header via headerBuilder
//   - Truncates to ReplyByteCap with TailFmt suffix
//
// Returns the empty string when rows is empty. Callers
// provide their own empty-state reply (e.g. "No active
// agents to close." for /close; /new handles empty differently).
func FormatTable[T any](rows []T, render Render[T], headerBuilder HeaderBuilder) string {
	if len(rows) == 0 {
		return ""
	}

	rendered := make([]RenderedRow, 0, len(rows))
	var success, skipped, failed int
	for _, r := range rows {
		rr := render(r)
		rendered = append(rendered, rr)
		switch rr.Bucket {
		case BucketSuccess:
			success++
		case BucketSkipped:
			skipped++
		case BucketFailure:
			failed++
		}
	}

	sort.SliceStable(rendered, func(i, j int) bool {
		if rendered[i].Bucket != rendered[j].Bucket {
			return rendered[i].Bucket < rendered[j].Bucket
		}
		if rendered[i].Agent != rendered[j].Agent {
			return rendered[i].Agent < rendered[j].Agent
		}
		return rendered[i].Cwd < rendered[j].Cwd
	})

	header := headerBuilder(success, skipped, failed)
	return truncateByBytes(header, rendered)
}

// truncateByBytes joins rows with "\n" and caps the total byte
// length at ReplyByteCap. Lines that would push the output
// over the cap are truncated and replaced with the TailFmt
// summary. The header is always included.
func truncateByBytes(header string, rows []RenderedRow) string {
	out := header
	for i, r := range rows {
		candidate := out + "\n" + r.Text
		if len(candidate)+len(tailFmtFor(i, len(rows))) > ReplyByteCap {
			hidden := len(rows) - i
			out = out + "\n" + fmt.Sprintf(TailFmt, hidden)
			return out
		}
		out = candidate
	}
	return out
}

// tailFmtFor returns the byte-length the tail would consume for
// the given (i, total) under the standard TailFmt template.
func tailFmtFor(i, total int) string {
	return fmt.Sprintf(TailFmt, total-i)
}

// HumanAction returns a short human-readable verb for an Action
// string (used in error messages). Exported so callers can
// build consistent error-row text.
func HumanAction(action string) string {
	switch action {
	case "closed", "close-failed":
		return "close"
	case "stale-cleared":
		return "stale-clear"
	default:
		return action
	}
}

// JoinCounts joins the count summary fragment used in mixed-
// result headers. Kept here so /close and /new use the same
// comma-space separator.
func JoinCounts(parts []string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += ", "
		}
		out += p
	}
	return out
}