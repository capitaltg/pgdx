package explain

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func mustRead(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return raw
}

func TestExplanation_FilteredParallelScan(t *testing.T) {
	out, err := Parse(mustRead(t, "seqscan_filter_noanalyze.json"))
	if err != nil {
		t.Fatal(err)
	}
	d := Diagnose(out)
	bullets := strings.Join(Explanation(out, d, false), "\n")

	for _, want := range []string{
		"NOT a row count", // cost-isn't-rows
		"reads every row", // the filtered scan, in words
		"PER WORKER",      // parallel per-worker note
		"two phases",      // partial/finalize aggregation
	} {
		if !strings.Contains(bullets, want) {
			t.Errorf("explanation missing %q, got:\n%s", want, bullets)
		}
	}
}

func TestExplanation_TrivialPlan_FewBullets(t *testing.T) {
	// A single cheap index scan: no parallelism, no aggregate — only the units note.
	out, _ := Parse([]byte(`[{"Plan":{"Node Type":"Index Scan","Relation Name":"orders",
	"Index Name":"orders_pkey","Index Cond":"(id = 1)","Total Cost":8.30,"Plan Rows":1}}]`))
	bullets := Explanation(out, Diagnose(out), false)
	if len(bullets) != 1 || !strings.Contains(bullets[0], "NOT a row count") {
		t.Fatalf("expected just the units note, got %v", bullets)
	}
}

func TestDecomposeScanCost_MatchesPlannerSeqScan(t *testing.T) {
	// The real awards filtered-scan plan: reconstruct its cost from the catalog facts and
	// confirm we land on the planner's number (this is the math behind explain -vvv).
	out, _ := Parse(mustRead(t, "seqscan_filter_noanalyze.json"))
	scan := PrimaryScanNode(out, Diagnose(out))
	if scan == nil || scan.NodeType != "Seq Scan" {
		t.Fatalf("expected the seq scan to be primary, got %+v", scan)
	}
	cost, ok := DecomposeScanCost(scan, ScanCostInputs{
		Reltuples: 51744316, Relpages: 4277026,
		SeqPageCost: 1.0, CPUTupleCost: 0.01, CPUOperatorCost: 0.0025,
		Workers: PlannedWorkers(out),
	})
	if !ok {
		t.Fatal("seq scan should be decomposable")
	}
	if cost.Divisor != 2.4 {
		t.Errorf("divisor for 2 workers should be 2.4, got %v", cost.Divisor)
	}
	// Reconstructed total must match the plan's scan cost within rounding.
	if math.Abs(cost.Total-scan.TotalCost) > 1.0 {
		t.Errorf("reconstructed cost %.2f should match plan cost %.2f", cost.Total, scan.TotalCost)
	}
}

func TestDecomposeScanCost_UnsupportedNode(t *testing.T) {
	// Index-only scans aren't decomposed — callers show catalog facts only.
	out, _ := Parse(mustRead(t, "count_star_index_only.json"))
	scan := PrimaryScanNode(out, Diagnose(out))
	if _, ok := DecomposeScanCost(scan, ScanCostInputs{Reltuples: 1, Relpages: 1, SeqPageCost: 1}); ok {
		t.Fatalf("an index-only scan must not be decomposed, node was %q", scan.NodeType)
	}
}

func TestParallelDivisor(t *testing.T) {
	cases := map[int]float64{1: 1.7, 2: 2.4, 3: 3.1, 4: 4.0}
	for workers, want := range cases {
		if got := ParallelDivisor(workers); math.Abs(got-want) > 1e-9 {
			t.Errorf("ParallelDivisor(%d) = %v, want %v", workers, got, want)
		}
	}
}
