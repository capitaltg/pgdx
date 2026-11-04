package render

import (
	"fmt"
	"io"
	"strings"
)

// SideBySide renders two tables in adjacent columns, padding the shorter one with blank
// lines. It exists to buy back vertical space when two small, related tables would
// otherwise stack — a role's two membership directions, or its database and schema
// privileges.
//
// maxWidth is the terminal budget: if the pair would exceed it, they're stacked instead
// (separated by a blank line), because a wrapped side-by-side table is far worse to read
// than two stacked ones. Pass 0 to accept any width.
//
// Each table keeps its own column alignment; only the gap between them is added here.
func SideBySide(w io.Writer, maxWidth int, left, right Tabular) error {
	l, err := tableLines(left)
	if err != nil {
		return err
	}
	r, err := tableLines(right)
	if err != nil {
		return err
	}

	const gap = 4 // wide enough that two tables don't read as one
	lw := widestLine(l)
	if maxWidth > 0 && lw+gap+widestLine(r) > maxWidth {
		// Too wide to pair: stack them, which is the layout SideBySide was avoiding but
		// still strictly more readable than wrapping.
		return writeAll(w, append(append(l, ""), r...))
	}

	out := make([]string, 0, max(len(l), len(r)))
	for i := 0; i < len(l) || i < len(r); i++ {
		var lc, rc string
		if i < len(l) {
			lc = l[i]
		}
		if i < len(r) {
			rc = r[i]
		}
		if rc == "" {
			// Nothing to the right — don't emit trailing whitespace.
			out = append(out, lc)
			continue
		}
		out = append(out, strings.TrimRight(lc+strings.Repeat(" ", lw-visibleWidth(lc)+gap)+rc, " "))
	}
	return writeAll(w, out)
}

// tableLines renders a table to its individual lines, so a caller can lay it out.
func tableLines(t Tabular) ([]string, error) {
	var b strings.Builder
	if err := renderTable(&b, t); err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(b.String(), "\n"), "\n"), nil
}

func widestLine(lines []string) int {
	n := 0
	for _, l := range lines {
		if v := visibleWidth(l); v > n {
			n = v
		}
	}
	return n
}

func writeAll(w io.Writer, lines []string) error {
	for _, l := range lines {
		if _, err := fmt.Fprintln(w, l); err != nil {
			return err
		}
	}
	return nil
}
