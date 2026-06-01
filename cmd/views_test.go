package cmd

import (
	"testing"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/db"
)

func TestHitPct(t *testing.T) {
	if got := hitPct(-1); got != "—" {
		t.Fatalf("no-blocks should render as dash, got %q", got)
	}
	if got := hitPct(99.6); got != "100%" {
		t.Fatalf("hitPct(99.6) = %q, want 100%%", got)
	}
}

func TestTempTraffic(t *testing.T) {
	if got := tempTraffic(0, 0); got != "0" {
		t.Fatalf("no temp traffic should be 0, got %q", got)
	}
	// 1 block read + 1 written = 2 * 8192 bytes = 16 KB.
	if got := tempTraffic(1, 1); got != "16.0 kB" {
		t.Fatalf("tempTraffic(1,1) = %q, want 16.0 kB", got)
	}
}

func TestSlowQueriesViewColumns(t *testing.T) {
	v := slowQueriesView{rows: []catalog.SlowQuery{{Calls: 3, TotalMs: 1500, MeanMs: 500, StddevMs: 10, Rows: 9, HitPct: 95, TempWritten: 2, Query: "select 1"}}}
	if len(v.Headers()) != len(v.Aligns()) {
		t.Fatalf("headers (%d) and aligns (%d) must match", len(v.Headers()), len(v.Aligns()))
	}
	row := v.Rows()[0]
	if len(row) != len(v.Headers()) {
		t.Fatalf("row has %d cells, headers have %d", len(row), len(v.Headers()))
	}
}

func TestQueryResultViewRendersValues(t *testing.T) {
	res := &db.QueryResult{
		Columns: []string{"id", "name", "raw"},
		Rows:    [][]any{{int64(1), nil, []byte("bytes")}},
	}
	v := queryResultView{res: res}
	row := v.Rows()[0]
	if row[0] != "1" || row[1] != "—" || row[2] != "bytes" {
		t.Fatalf("value rendering wrong: %v", row)
	}
}

// Each new table view should expose matching header/align widths so columns line up.
func TestNewViewsAlignmentWidths(t *testing.T) {
	if h, a := (slotsView{}).Headers(), (slotsView{}).Aligns(); len(h) != len(a) {
		t.Fatalf("slotsView headers=%d aligns=%d", len(h), len(a))
	}
	if h, a := (redundantIndexesView{}).Headers(), (redundantIndexesView{}).Aligns(); len(h) != len(a) {
		t.Fatalf("redundantIndexesView headers=%d aligns=%d", len(h), len(a))
	}
	if h, a := (columnStatsView{}).Headers(), (columnStatsView{}).Aligns(); len(h) != len(a) {
		t.Fatalf("columnStatsView headers=%d aligns=%d", len(h), len(a))
	}
	if h, a := (stmtDiffView{}).Headers(), (stmtDiffView{}).Aligns(); len(h) != len(a) {
		t.Fatalf("stmtDiffView headers=%d aligns=%d", len(h), len(a))
	}
	if h, a := (tableDiffView{}).Headers(), (tableDiffView{}).Aligns(); len(h) != len(a) {
		t.Fatalf("tableDiffView headers=%d aligns=%d", len(h), len(a))
	}
	// referencedByView has no Aligns (defaults to left); just confirm it qualifies schemas.
	rv := referencedByView{{Schema: "app", Table: "orders", Constraint: "fk", Definition: "FOREIGN KEY (x)"}}
	if got := rv.Rows()[0][0]; got != "app.orders" {
		t.Fatalf("referencedByView should qualify a non-public schema: got %q", got)
	}
	_ = catalog.Reference{}
}
