package render

import (
	"strings"
	"testing"
)

func TestSideBySide_PairsAndPads(t *testing.T) {
	var b strings.Builder
	left := twoColView{{"readonly", "no"}}
	right := twoColView{{"a", "yes"}, {"b", "yes"}}
	if err := SideBySide(&b, 0, left, right); err != nil {
		t.Fatalf("SideBySide: %v", err)
	}
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	// Header + the taller table's row count.
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d:\n%s", len(lines), b.String())
	}
	// The shorter table runs out; the right column must continue regardless.
	if !strings.Contains(lines[2], "b") {
		t.Errorf("the taller table's last row was dropped: %q", lines[2])
	}
	// Right column starts at the same offset on every line that has one.
	off := func(l string) int { return strings.Index(l, "a") }
	if off(lines[1]) <= 0 {
		t.Errorf("right table should be offset from the left one: %q", lines[1])
	}
}

func TestSideBySide_StacksWhenTooWide(t *testing.T) {
	var b strings.Builder
	wide := twoColView{{strings.Repeat("x", 40), strings.Repeat("y", 40)}}
	if err := SideBySide(&b, 60, wide, wide); err != nil {
		t.Fatalf("SideBySide: %v", err)
	}
	// A pair that would wrap is worse than two stacked tables, so it must stack: four
	// content lines (2 headers + 2 rows) plus the blank separator.
	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	if len(lines) != 5 {
		t.Fatalf("expected the pair to stack into 5 lines, got %d:\n%s", len(lines), b.String())
	}
	if lines[2] != "" {
		t.Errorf("stacked tables should be separated by a blank line, got %q", lines[2])
	}
}

func TestSideBySide_NoTrailingWhitespace(t *testing.T) {
	var b strings.Builder
	if err := SideBySide(&b, 0, twoColView{{"a", "1"}, {"bb", "2"}}, twoColView{{"c", "3"}}); err != nil {
		t.Fatalf("SideBySide: %v", err)
	}
	for _, line := range strings.Split(strings.TrimRight(b.String(), "\n"), "\n") {
		if strings.HasSuffix(line, " ") {
			t.Fatalf("line has trailing whitespace: %q", line)
		}
	}
}
