package gtw

// nightmeSite is the canonical NightMe product URL. The issue
// footer (/gtw new) and the PR body heading (/gtw pr) both link
// the brand to it, so the URL lives in one place.
const nightmeSite = "https://nightme.dev"

// summaryHeading is the opening heading of every PR body. The
// NightMe name links to nightmeSite.
const summaryHeading = "## Summary by [NightMe](" + nightmeSite + ")"

// summaryHeadingPlain is summaryHeading without the link — the
// shape models fall back to when they paraphrase the heading.
// See normalizeSummaryHeading.
const summaryHeadingPlain = "## Summary by NightMe"
