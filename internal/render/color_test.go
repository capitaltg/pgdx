package render

import (
	"strings"
	"testing"
)

func TestVisibleWidth_SkipsANSI(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want int
	}{
		{"plain", "abc", 3},
		{"multibyte", "—✓○", 3},
		{"colored", "\x1b[32m✓\x1b[0m", 1},
		{"colored word", "\x1b[2m(none)\x1b[0m", 6},
		{"mixed", "a\x1b[1mb\x1b[0mc", 3},
		{"bare escape is not counted", "\x1b", 0},
	}
	for _, tc := range cases {
		if got := visibleWidth(tc.in); got != tc.want {
			t.Errorf("%s: visibleWidth(%q) = %d, want %d", tc.name, tc.in, got, tc.want)
		}
	}
}

// The whole reason visibleWidth exists: a colored cell must occupy the same number of
// columns as the same text uncolored, or every column after it shifts.
func TestRenderTable_ColoredCellsStayAligned(t *testing.T) {
	plainOut := renderToString(t, twoColView{{"a", "yes"}, {"bb", "no"}})

	SetColor(true)
	defer SetColor(false)
	coloredOut := renderToString(t, twoColView{{"a", Green("yes")}, {"bb", Dim("no")}})

	strip := func(s string) string {
		var b strings.Builder
		for i := 0; i < len(s); {
			if s[i] == 0x1b {
				for i < len(s) && s[i] != 'm' {
					i++
				}
				i++
				continue
			}
			b.WriteByte(s[i])
			i++
		}
		return b.String()
	}
	if strip(coloredOut) != plainOut {
		t.Fatalf("colored layout differs from plain once escapes are stripped:\n%q\n%q",
			strip(coloredOut), plainOut)
	}
}

func TestColorDisabledByDefault(t *testing.T) {
	// Library/test callers that never opt in must see plain text, so golden output and
	// piped output stay byte-identical.
	SetColor(false)
	if got := Green("x"); got != "x" {
		t.Fatalf("Green with color off = %q, want plain", got)
	}
	if got := Flag(true); got != "✓" {
		t.Fatalf("Flag with color off = %q, want a bare glyph", got)
	}
}

func TestFlag_DistinguishesTrueFromFalse(t *testing.T) {
	SetColor(false)
	if Flag(true) == Flag(false) {
		t.Fatal("Flag must differ by glyph, not color alone — color is off when piped")
	}
}

func TestRenderTable_NoTrailingWhitespace(t *testing.T) {
	// A view with an empty final column would otherwise pad every line with blanks.
	out := renderToString(t, threeColView{{"a", "1", ""}, {"bb", "2", ""}})
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if strings.HasSuffix(line, " ") {
			t.Fatalf("line has trailing whitespace: %q", line)
		}
	}
}

type twoColView [][]string

func (v twoColView) Headers() []string { return []string{"NAME", "OK"} }
func (v twoColView) Rows() [][]string  { return v }

type threeColView [][]string

func (v threeColView) Headers() []string { return []string{"NAME", "N", ""} }
func (v threeColView) Rows() [][]string  { return v }

func renderToString(t *testing.T, v Tabular) string {
	t.Helper()
	var b strings.Builder
	if err := renderTable(&b, v); err != nil {
		t.Fatalf("renderTable: %v", err)
	}
	return b.String()
}
