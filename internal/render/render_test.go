package render

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

type tab struct {
	headers []string
	rows    [][]string
}

func (t tab) Headers() []string { return t.headers }
func (t tab) Rows() [][]string  { return t.rows }

func TestRenderJSON(t *testing.T) {
	var buf bytes.Buffer
	data := map[string]any{"name": "orders", "rows": 100}
	if err := Render(&buf, FormatJSON, data); err != nil {
		t.Fatalf("Render: %v", err)
	}
	// Must be valid JSON — the whole point of -o json | jq.
	var back map[string]any
	if err := json.Unmarshal(buf.Bytes(), &back); err != nil {
		t.Fatalf("output is not valid JSON: %v\n%s", err, buf.String())
	}
	if back["name"] != "orders" {
		t.Fatalf("round-trip lost data: %v", back)
	}
}

func TestRenderTable(t *testing.T) {
	var buf bytes.Buffer
	data := tab{headers: []string{"NAME", "ROWS"}, rows: [][]string{{"orders", "100"}, {"customers", "5"}}}
	if err := Render(&buf, FormatTable, data); err != nil {
		t.Fatalf("Render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{"NAME", "ROWS", "orders", "customers"} {
		if !strings.Contains(out, want) {
			t.Fatalf("table missing %q:\n%s", want, out)
		}
	}
}

func TestRenderTableRejectsNonTabular(t *testing.T) {
	var buf bytes.Buffer
	if err := Render(&buf, FormatTable, map[string]any{"x": 1}); err == nil {
		t.Fatal("expected error rendering non-Tabular as table")
	}
}

func TestParseFormat(t *testing.T) {
	for _, ok := range []string{"table", "json"} {
		if _, err := ParseFormat(ok); err != nil {
			t.Fatalf("ParseFormat(%q) errored: %v", ok, err)
		}
	}
	for _, bad := range []string{"yaml", "wide", "xml", ""} {
		if _, err := ParseFormat(bad); err == nil {
			t.Fatalf("ParseFormat(%q) should have errored", bad)
		}
	}
}
