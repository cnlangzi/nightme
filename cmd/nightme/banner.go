// Package main — shared banner text for nightme.
//
// The banner mirrors the project logo (logo.png): two interlocking
// rings forming an infinity glyph, drawn with box-drawing characters
// so it renders correctly in any UTF-8 terminal without depending on
// a colour library. Width is held to 23 columns so it fits inside
// the standard 80-column terminal the docs/SPEC.md calls out.
//
// Two helpers expose it:
//
//   - banner is the raw ASCII art (constant; no allocation).
//   - bannerWithVersion prepends the art to the standard
//     version.String() line so REPL startup and `nightme --version`
//     share the same header.
package main

import (
	"strings"

	"github.com/cnlangzi/nightme/internal/version"
)

// banner is the ASCII rendering of the nightme logo, hand-drawn
// from logo.png. Two interlocking ovals form an infinity glyph —
// the left ring (cyan in the source PNG) and the right ring (dark
// grey in the source PNG) cross at the centre.
//
// Pure ASCII (no Unicode blocks): `.` and `:` fill the soft
// outline, `-` and `=` trace the ring edges, `+` shades the
// highlight side, and `*` / `#` lay the deeper fill. Held at
// ~13 rows × ~31 columns so it fits comfortably inside the
// 80-column target the docs/SPEC.md calls out.
const banner = `
        ...     .:==-:.
    .::::::::=##++++++++=-
  ..:::::::::::+##+++++++++:
 .::::::::::::---*#*++++++++-
 ::::::   .--------:  .=+++++.
 :---:.     :====:     .*++++.
 :----:   :********:   =****+.
 .-------=*##***************=
  .------==+*##***********#=
    :----===++*##########+.
       .:---:..   -**+-.
`

// bannerWithVersion returns the ASCII art followed by the standard
// version line. Used as the header for the REPL banner and the
// `nightme --version` template so they share an identical look.
//
// The newline between the art and the version line is part of the
// banner constant above; we only add the trailing newline that the
// caller expects to terminate the version line.
func bannerWithVersion() string {
	var b strings.Builder
	b.Grow(len(banner) + 1 + len(version.String()))
	b.WriteString(banner)
	b.WriteByte('\n')
	b.WriteString(version.String())
	return b.String()
}