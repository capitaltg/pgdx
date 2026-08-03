package render

import (
	"strings"
	"unicode/utf8"
)

// Color for terminal output, deliberately minimal: pgdx uses it only to make boolean
// columns scannable, never to carry information that isn't also in the text. A pipe, a
// redirect, NO_COLOR, or -o json all disable it, so `pgdx ... | grep` and golden tests
// see plain ASCII.
//
// Because escape sequences are invisible but occupy bytes and runes, every width
// measurement in this package goes through visibleWidth — colored cells must align
// exactly like uncolored ones.

// ANSI SGR sequences. Dim (2) rather than red for "no": a false flag is ordinary, not an
// error, and red-on-everything is what makes a table hard to read.
const (
	ansiReset = "\x1b[0m"
	ansiGreen = "\x1b[32m"
	ansiDim   = "\x1b[2m"
	ansiBold  = "\x1b[1m"
)

// colorEnabled is process-wide, set once from the command layer, which is the only place
// that knows whether stdout is a terminal and which -o format was chosen.
var colorEnabled bool

// SetColor enables or disables colored output. Off by default, so any caller that never
// opts in (tests, library use) gets plain text.
func SetColor(on bool) { colorEnabled = on }

// ColorEnabled reports the current setting, for callers deciding between a glyph and a
// word.
func ColorEnabled() bool { return colorEnabled }

func paint(s, code string) string {
	if !colorEnabled || s == "" {
		return s
	}
	return code + s + ansiReset
}

// Green and Dim are the two emphases pgdx uses in tables.
func Green(s string) string { return paint(s, ansiGreen) }
func Dim(s string) string   { return paint(s, ansiDim) }
func Bold(s string) string  { return paint(s, ansiBold) }

// Flag renders a boolean table cell: a green check when true, a dim circle when false.
// The glyphs match the ✓ / ○ vocabulary `pgdx status` already uses, and both are one
// column wide so a row of them lines up.
func Flag(b bool) string {
	if b {
		return Green("✓")
	}
	return Dim("○")
}

// visibleWidth is the on-screen width of s in columns, skipping ANSI escape sequences.
// Without this, a colored cell measures ~9 runes wider than it looks and every column
// after it is misaligned.
func visibleWidth(s string) int {
	if !strings.Contains(s, "\x1b") {
		return utf8.RuneCountInString(s)
	}
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			// Skip a CSI sequence: ESC [ ... <final byte in @-~>. Anything malformed is
			// consumed to the end rather than counted, which is the safe direction.
			j := i + 1
			if j < len(s) && s[j] == '[' {
				j++
				for j < len(s) && (s[j] < '@' || s[j] > '~') {
					j++
				}
				if j < len(s) {
					j++ // the final byte
				}
			}
			i = j
			continue
		}
		_, size := utf8.DecodeRuneInString(s[i:])
		n++
		i += size
	}
	return n
}

// VisibleWidth exposes the display-width measurement for callers laying tables out
// side by side.
func VisibleWidth(s string) int { return visibleWidth(s) }
