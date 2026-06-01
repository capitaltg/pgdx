package snapshot

import (
	"testing"

	"github.com/capitaltg/pgdx/internal/catalog"
)

func qid(n int64) *int64 { return &n }

func TestDiffStatements(t *testing.T) {
	older := &Snapshot{Statements: []catalog.StmtStatRow{
		{QueryID: qid(1), Query: "select a", Calls: 10, TotalMs: 100, Rows: 10, SharedRead: 5},
		{QueryID: qid(2), Query: "select b", Calls: 5, TotalMs: 50},
	}}
	newer := &Snapshot{Statements: []catalog.StmtStatRow{
		{QueryID: qid(1), Query: "select a", Calls: 20, TotalMs: 400, Rows: 20, SharedRead: 25}, // +10 calls, +300ms
		{QueryID: qid(2), Query: "select b", Calls: 5, TotalMs: 50},                             // unchanged → dropped
		{QueryID: qid(3), Query: "select c", Calls: 3, TotalMs: 30},                             // new
	}}

	got := DiffStatements(older, newer)
	if len(got) != 2 {
		t.Fatalf("want 2 changed statements, got %d: %+v", len(got), got)
	}
	// Sorted by added total time, so query 1 (+300ms) comes before query 3 (+30ms).
	if *got[0].QueryID != 1 || got[0].Calls != 10 || got[0].TotalMs != 300 {
		t.Fatalf("first mover wrong: %+v", got[0])
	}
	if got[0].MeanMs != 30 { // 300ms / 10 calls
		t.Fatalf("mean of interval = %v, want 30", got[0].MeanMs)
	}
	if got[0].SharedRead != 20 {
		t.Fatalf("delta shared_read = %d, want 20", got[0].SharedRead)
	}
	if *got[1].QueryID != 3 || !got[1].IsNew {
		t.Fatalf("second mover should be the new query: %+v", got[1])
	}
}

func TestDiffStatementsHandlesReset(t *testing.T) {
	older := &Snapshot{Statements: []catalog.StmtStatRow{
		{QueryID: qid(1), Query: "select a", Calls: 100, TotalMs: 1000},
	}}
	// Counters went backwards → pg_stat_statements was reset between snapshots.
	newer := &Snapshot{Statements: []catalog.StmtStatRow{
		{QueryID: qid(1), Query: "select a", Calls: 3, TotalMs: 30},
	}}
	got := DiffStatements(older, newer)
	if len(got) != 1 || !got[0].IsNew {
		t.Fatalf("a reset should be treated as fresh accumulation (IsNew): %+v", got)
	}
	if got[0].Calls != 3 || got[0].TotalMs != 30 {
		t.Fatalf("after reset the delta should equal the new totals, got %+v", got[0])
	}
}

func TestDiffTables(t *testing.T) {
	older := &Snapshot{Tables: []catalog.TableStatRow{
		{Schema: "public", Name: "orders", Ins: 100, Upd: 10, SeqScan: 2},
		{Schema: "public", Name: "quiet", Ins: 5},
	}}
	newer := &Snapshot{Tables: []catalog.TableStatRow{
		{Schema: "public", Name: "orders", Ins: 250, Upd: 40, Del: 5, SeqScan: 2}, // +150 ins,+30 upd,+5 del
		{Schema: "public", Name: "quiet", Ins: 5},                                 // unchanged
		{Schema: "public", Name: "fresh", Ins: 7},                                 // new
	}}
	got := DiffTables(older, newer)
	if len(got) != 2 {
		t.Fatalf("want 2 active tables, got %d: %+v", len(got), got)
	}
	if got[0].Name != "orders" || got[0].Writes != 185 {
		t.Fatalf("busiest table wrong: %+v", got[0])
	}
	if got[1].Name != "fresh" || !got[1].IsNew {
		t.Fatalf("second should be the new table: %+v", got[1])
	}
}

func TestStmtKeyFallsBackToQueryText(t *testing.T) {
	// With no queryid, matching is by query text so a diff still works.
	older := &Snapshot{Statements: []catalog.StmtStatRow{{Query: "select x", Calls: 1, TotalMs: 10}}}
	newer := &Snapshot{Statements: []catalog.StmtStatRow{{Query: "select x", Calls: 4, TotalMs: 40}}}
	got := DiffStatements(older, newer)
	if len(got) != 1 || got[0].Calls != 3 || got[0].IsNew {
		t.Fatalf("text-keyed diff wrong: %+v", got)
	}
}
