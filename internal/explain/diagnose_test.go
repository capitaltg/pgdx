package explain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Fixtures are real EXPLAIN (FORMAT JSON) outputs captured from Postgres 16
// (internal/explain/testdata). This is the seed of the D5 fixture set; the user's
// 20-30 real plans expand it and tune false-positive rate.
func diagnoseFixture(t *testing.T, name string) Diagnosis {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	out, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	return Diagnose(out)
}

func hasFindingContaining(d Diagnosis, substr string) bool {
	for _, f := range d.Findings {
		if strings.Contains(f.Title, substr) || strings.Contains(f.Detail, substr) || strings.Contains(f.Suggestion, substr) {
			return true
		}
	}
	return false
}

func TestDiagnose_CountStar_FullScan(t *testing.T) {
	for _, fx := range []string{"count_star.json", "count_star_analyze.json"} {
		d := diagnoseFixture(t, fx)
		if !d.HasFindings() {
			t.Fatalf("%s: expected a finding, got none", fx)
		}
		if !hasFindingContaining(d, "Full sequential scan") {
			t.Fatalf("%s: expected a full-scan finding, got %+v", fx, d.Findings)
		}
		if !hasFindingContaining(d, "reltuples") {
			t.Fatalf("%s: full-scan finding should suggest the reltuples estimate, got %+v", fx, d.Findings)
		}
		// It must NOT wrongly suggest an index for an unfiltered count(*).
		if hasFindingContaining(d, "missing-index") {
			t.Fatalf("%s: must not suggest an index for an unfiltered count(*)", fx)
		}
	}
}

func TestDiagnose_CountStar_IndexOnlyFullScan(t *testing.T) {
	// Real plan: count(*) on a table with an index, so Postgres reads the WHOLE index
	// (a full Index Only Scan, no bounds) rather than the heap. This dominates cost and
	// is slow — we must flag it, not reassure the user with "no table fetch needed".
	d := diagnoseFixture(t, "count_star_index_only.json")
	if !d.HasFindings() {
		t.Fatalf("expected a finding for a full index-only count scan, got none (note: %q)", d.Note)
	}
	if !hasFindingContaining(d, "Full index scan") {
		t.Fatalf("expected a full-index-scan finding, got %+v", d.Findings)
	}
	// Name the index that's being read end-to-end, and keep the right count advice.
	if !hasFindingContaining(d, "awards_date_signed_idx") {
		t.Fatalf("finding should name the index scanned end-to-end, got %+v", d.Findings)
	}
	if !hasFindingContaining(d, "reltuples") {
		t.Fatalf("full index-only count should still suggest the reltuples estimate, got %+v", d.Findings)
	}
	// It must NOT call this a sequential scan (it isn't) nor suggest a missing index.
	if hasFindingContaining(d, "sequential") {
		t.Fatalf("an index-only scan must not be called a sequential scan, got %+v", d.Findings)
	}
	if hasFindingContaining(d, "missing-index") {
		t.Fatalf("must not suggest an index for an unfiltered count(*), got %+v", d.Findings)
	}
}

func TestDiagnose_SelectiveIndexScan_NoFalsePositive(t *testing.T) {
	// An expensive index scan WITH bounds (Index Cond) seeks to specific rows — it is the
	// intended access path and must NOT be flagged as a full scan (D7).
	sel := `[{"Plan":{"Node Type":"Index Scan","Relation Name":"awards","Index Name":"awards_pkey",
	"Index Cond":"(id = 42)","Startup Cost":0.56,"Total Cost":50000.00,"Plan Rows":1}}]`
	out, err := Parse([]byte(sel))
	if err != nil {
		t.Fatal(err)
	}
	d := Diagnose(out)
	if d.HasFindings() {
		t.Fatalf("a bounded (Index Cond) index scan must not be flagged as a full scan, got %+v", d.Findings)
	}
}

func TestDiagnose_MissingIndex(t *testing.T) {
	d := diagnoseFixture(t, "seqscan_missing_index.json")
	if !hasFindingContaining(d, "filters out most rows") {
		t.Fatalf("expected a missing-index finding, got %+v", d.Findings)
	}
	if !hasFindingContaining(d, "index") {
		t.Fatalf("missing-index finding should suggest an index, got %+v", d.Findings)
	}
}

func TestDiagnose_FilteredSeqScan_NoAnalyze(t *testing.T) {
	// Real plan (no --analyze): count(*) with a filter Postgres can't index, so it does a
	// multi-million-cost Seq Scan with a Filter. Without rows-removed evidence the old code
	// fell through to healthyReason and wrongly reassured "its cost is low". We must flag it.
	d := diagnoseFixture(t, "seqscan_filter_noanalyze.json")
	if !d.HasFindings() {
		t.Fatalf("expected a finding for an expensive filtered seq scan, got none (note: %q)", d.Note)
	}
	if !hasFindingContaining(d, "filter") {
		t.Fatalf("expected a filtered-scan finding, got %+v", d.Findings)
	}
	// It must be honest that selectivity is unmeasured and point at --analyze.
	if !hasFindingContaining(d, "--analyze") {
		t.Fatalf("a no-analyze finding should recommend --analyze to confirm, got %+v", d.Findings)
	}
	// The contradictory "cost is low" reassurance must NOT appear anywhere.
	if strings.Contains(d.Note, "cost is low") {
		t.Fatalf("must not reassure 'cost is low' on an expensive scan, note=%q", d.Note)
	}
	// One finding only — a parallel plan must not duplicate it.
	scans := 0
	for _, f := range d.Findings {
		if strings.Contains(f.Title, "filter") {
			scans++
		}
	}
	if scans != 1 {
		t.Fatalf("expected exactly one filtered-scan finding, got %d: %+v", scans, d.Findings)
	}
}

func TestDiagnose_CheapFilteredSeqScan_NoFalsePositive(t *testing.T) {
	// A cheap filtered seq scan (small table) with no ANALYZE must NOT be flagged, and the
	// healthy verdict may still call it cheap — that claim is true here.
	cheap := `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"tiny","Filter":"(x = 1)",
	"Startup Cost":0.00,"Total Cost":12.50,"Plan Rows":1}}]`
	out, _ := Parse([]byte(cheap))
	d := Diagnose(out)
	if d.HasFindings() {
		t.Fatalf("a cheap filtered scan must not be flagged without analyze, got %+v", d.Findings)
	}
	if !strings.Contains(d.Note, "small table or cheap scan") {
		t.Fatalf("expected the benign cheap-scan note, got %q", d.Note)
	}
}

func TestDiagnose_SortSpill(t *testing.T) {
	d := diagnoseFixture(t, "sort_spill.json")
	if !hasFindingContaining(d, "Sort spilled to disk") {
		t.Fatalf("expected a sort-spill finding, got %+v", d.Findings)
	}
	if !hasFindingContaining(d, "work_mem") {
		t.Fatalf("sort-spill finding should mention work_mem, got %+v", d.Findings)
	}
	// Parallel plan reports the spill per worker — must be reported once, not 3x.
	spills := 0
	for _, f := range d.Findings {
		if strings.Contains(f.Title, "Sort spilled") {
			spills++
		}
	}
	if spills != 1 {
		t.Fatalf("sort spill should be de-duped to one finding, got %d", spills)
	}
}

func TestDiagnose_CleanPlan_NoObviousProblem(t *testing.T) {
	// A cheap index scan returning a row: no pattern should fire.
	clean := `[{"Plan":{"Node Type":"Index Scan","Relation Name":"orders","Index Name":"orders_pkey",
	"Startup Cost":0.29,"Total Cost":8.30,"Plan Rows":1}}]`
	out, err := Parse([]byte(clean))
	if err != nil {
		t.Fatal(err)
	}
	d := Diagnose(out)
	if d.HasFindings() {
		t.Fatalf("clean index scan should yield no findings, got %+v", d.Findings)
	}
	// The verdict must explain WHY it's fine, naming the index it uses.
	if !strings.Contains(d.Note, "index scan") || !strings.Contains(d.Note, "orders_pkey") {
		t.Fatalf("expected a 'why it's healthy' note naming the index, got %q", d.Note)
	}
}

func TestDiagnose_TinyTableScan_NoFalsePositive(t *testing.T) {
	// A full seq scan of a tiny, cheap table is correct — must NOT be flagged (D7).
	tiny := `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"tiny",
	"Startup Cost":0.00,"Total Cost":1.05,"Plan Rows":5}}]`
	out, _ := Parse([]byte(tiny))
	d := Diagnose(out)
	if d.HasFindings() {
		t.Fatalf("cheap full scan of a tiny table must not be flagged, got %+v", d.Findings)
	}
	if !strings.Contains(d.Note, "small table or cheap scan") {
		t.Fatalf("expected a benign note for the tiny scan, got %q", d.Note)
	}
}

func TestDiagnose_LargeSelectScan_NotCountAdvice(t *testing.T) {
	// A large no-filter scan WITHOUT an aggregate should advise filter/LIMIT, not reltuples.
	big := `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"events",
	"Startup Cost":0.00,"Total Cost":25000.00,"Plan Rows":1000000}}]`
	out, _ := Parse([]byte(big))
	d := Diagnose(out)
	if !hasFindingContaining(d, "Full sequential scan") {
		t.Fatalf("expected a full-scan finding for a large scan, got %+v", d.Findings)
	}
	if hasFindingContaining(d, "reltuples") {
		t.Fatalf("non-aggregate scan must NOT give count(*)/reltuples advice, got %+v", d.Findings)
	}
	if !hasFindingContaining(d, "LIMIT") {
		t.Fatalf("large plain scan should suggest a filter/LIMIT, got %+v", d.Findings)
	}
}

func TestCompactNum(t *testing.T) {
	cases := map[float64]string{500: "500", 4200: "4.2k", 1_234_567: "1.2M"}
	for in, want := range cases {
		if got := compactNum(in); got != want {
			t.Fatalf("compactNum(%v) = %q, want %q", in, got, want)
		}
	}
}
