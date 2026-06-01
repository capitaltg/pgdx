package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/capitaltg/pgdx/internal/catalog"
)

func TestPrintSummaryNotes(t *testing.T) {
	if got := partitionedNote(0); got != "" {
		t.Fatalf("zero partitions should have no note, got %q", got)
	}
	if got := partitionedNote(3); got != "(3 partitioned)" {
		t.Fatalf("partitionedNote(3) = %q", got)
	}
	if got := indexHealthNote(&catalog.DatabaseSummary{}); got != "" {
		t.Fatalf("a clean index set should produce no health note, got %q", got)
	}
	if got := indexHealthNote(&catalog.DatabaseSummary{UnusedIndexes: 2, RedundantIndexes: 1}); !strings.Contains(got, "2 unused") || !strings.Contains(got, "1 redundant") {
		t.Fatalf("indexHealthNote rollup wrong: %q", got)
	}
}

func TestTopTablesView(t *testing.T) {
	v := topTablesView{{Schema: "public", Name: "orders", SizeBytes: 1 << 20}}
	if len(v.Headers()) != len(v.Aligns()) {
		t.Fatal("headers and aligns must match width")
	}
	if got := v.Rows()[0][2]; got != "1.0 MB" {
		t.Fatalf("size cell = %q, want 1.0 MB", got)
	}
}

// printSummaryBody renders the connection-independent overview; verify its content.
func TestPrintSummaryStreams(t *testing.T) {
	var out bytes.Buffer
	s := &catalog.DatabaseSummary{
		Database: "shop", Encoding: "UTF8", SizeBytes: 2 << 30,
		TableBytes: 1 << 30, IndexBytes: 512 << 20,
		Schemas: 3, Tables: 42, Partitioned: 4, Views: 5, MaterializedViews: 2,
		Indexes: 88, Sequences: 12, Functions: 20, Extensions: 4,
		UnusedIndexes: 6, UnusedIndexBytes: 100 << 20, RedundantIndexes: 3,
		EstBloatBytes: 340 << 20,
		TopTables:     []catalog.TableSize{{Schema: "public", Name: "orders", SizeBytes: 600 << 20}},
	}
	printSummaryBody(&out, s)

	o := out.String()
	for _, want := range []string{"Objects:", "Tables", "42", "(4 partitioned)", "88", "6 unused", "Largest tables:", "orders"} {
		if !strings.Contains(o, want) {
			t.Fatalf("summary stdout missing %q:\n%s", want, o)
		}
	}
}
