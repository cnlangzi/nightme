package gtw

// nightmeSite is the canonical NightMe product URL. The issue
// footer (/gtw new) and the PR body heading (/gtw pr) both link
// the brand to it. Code builds its links from this constant;
// comments and docs spell the URL out literally.
const nightmeSite = "https://nightme.dev"

// summaryHeading is the opening heading of every PR body. The
// NightMe name links to nightmeSite.
const summaryHeading = "## Summary by [NightMe](" + nightmeSite + ")"

// summaryHeadingPlain is summaryHeading without the link — the
// shape models fall back to when they paraphrase the heading.
// See normalizeSummaryHeading.
const summaryHeadingPlain = "## Summary by NightMe"

// summaryHeadingLinkedPrefix is the linked heading without its
// URL: any heading starting with it carries a NightMe link, and
// normalizeSummaryHeading replaces it with summaryHeading.
const summaryHeadingLinkedPrefix = "## Summary by [NightMe]("
