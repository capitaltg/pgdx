// Package render is the single output layer every pgdx command shares (D2).
//
// Stream contract (D4): rendered DATA goes to the io.Writer passed in (the command
// wires this to stdout). Warnings, errors, and progress must NOT go through here —
// they go to stderr via the caller. This keeps `pgdx ... -o json | jq` clean: only
// data ever reaches stdout.
package render

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode/utf8"
)

// Format is an output format selected by -o/--output.
type Format string

const (
	FormatTable   Format = "table"   // default, human-readable
	FormatJSON    Format = "json"    // machine-readable, jq-clean
	FormatMermaid Format = "mermaid" // ER diagram; only `describe table` supports it
	FormatYAML    Format = "yaml"    // deferred (D-direction): not yet supported
	FormatWide    Format = "wide"    // deferred: not yet supported
)

// ParseFormat validates a -o value.
func ParseFormat(s string) (Format, error) {
	switch Format(s) {
	case FormatTable, FormatJSON:
		return Format(s), nil
	case FormatYAML, FormatWide:
		return "", fmt.Errorf("output format %q is not supported yet (v0.1 supports: table, json)", s)
	default:
		return "", fmt.Errorf("unknown output format %q (supported: table, json)", s)
	}
}

// Tabular is implemented by anything that can render as a table.
type Tabular interface {
	Headers() []string
	Rows() [][]string
}

// Align is a per-column alignment.
type Align int

const (
	AlignLeft  Align = iota // default
	AlignRight              // for numeric columns
)

// Aligned is an optional Tabular extension: a view that wants specific column
// alignment implements it. Columns default to left when not provided.
type Aligned interface {
	Tabular
	Aligns() []Align
}

// Render writes data to w in the requested format. For FormatJSON, any value is
// marshaled. For FormatTable, the value must implement Tabular.
func Render(w io.Writer, format Format, data any) error {
	switch format {
	case FormatJSON:
		enc := json.NewEncoder(w)
		enc.SetIndent("", "  ")
		return enc.Encode(data)
	case FormatTable:
		t, ok := data.(Tabular)
		if !ok {
			return fmt.Errorf("render: value of type %T cannot be shown as a table (use -o json)", data)
		}
		return renderTable(w, t)
	default:
		return fmt.Errorf("render: unsupported format %q", format)
	}
}

func renderTable(w io.Writer, t Tabular) error {
	headers := t.Headers()
	rows := t.Rows()
	ncol := len(headers)

	aligns := make([]Align, ncol) // default AlignLeft
	if a, ok := t.(Aligned); ok {
		copy(aligns, a.Aligns())
	}

	// Column widths = widest cell (header or any row), measured in runes so
	// multi-byte glyphs like "—" don't throw off alignment.
	widths := make([]int, ncol)
	for i, h := range headers {
		widths[i] = utf8.RuneCountInString(h)
	}
	for _, row := range rows {
		for i := 0; i < ncol && i < len(row); i++ {
			if l := utf8.RuneCountInString(row[i]); l > widths[i] {
				widths[i] = l
			}
		}
	}

	var b strings.Builder
	writeRow := func(cells []string) {
		for i := 0; i < ncol; i++ {
			cell := ""
			if i < len(cells) {
				cell = cells[i]
			}
			pad := widths[i] - utf8.RuneCountInString(cell)
			if pad < 0 {
				pad = 0
			}
			last := i == ncol-1
			if aligns[i] == AlignRight {
				b.WriteString(strings.Repeat(" ", pad))
				b.WriteString(cell)
			} else {
				b.WriteString(cell)
				if !last { // no trailing padding on a left-aligned final column
					b.WriteString(strings.Repeat(" ", pad))
				}
			}
			if !last {
				b.WriteString("  ") // column gap
			}
		}
		b.WriteByte('\n')
	}

	writeRow(headers)
	for _, row := range rows {
		writeRow(row)
	}
	_, err := io.WriteString(w, b.String())
	return err
}
