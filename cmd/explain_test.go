package cmd

import (
	"strings"
	"testing"
)

func TestWrapText(t *testing.T) {
	t.Run("short text is unchanged", func(t *testing.T) {
		if got := wrapText("a short line", "  ", 80); got != "a short line" {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("wraps and indents continuation lines", func(t *testing.T) {
		got := wrapText("one two three four five", "..", 9)
		lines := strings.Split(got, "\n")
		if len(lines) < 2 {
			t.Fatalf("expected wrapping, got %q", got)
		}
		// First line not indented; later lines carry the indent prefix.
		if strings.HasPrefix(lines[0], "..") {
			t.Fatalf("first line should not be indented: %q", lines[0])
		}
		for _, l := range lines[1:] {
			if !strings.HasPrefix(l, "..") {
				t.Fatalf("continuation line missing indent: %q", l)
			}
			// width is the content width; the indent is added on top of it.
			content := strings.TrimPrefix(l, "..")
			if len([]rune(content)) > 9 {
				t.Fatalf("content exceeds width: %q", content)
			}
		}
	})
	t.Run("empty", func(t *testing.T) {
		if got := wrapText("   ", "  ", 80); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})
}
