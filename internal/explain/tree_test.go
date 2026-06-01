package explain

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func planTreeFixture(t *testing.T, name string) (string, Diagnosis) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	out, err := Parse(raw)
	if err != nil {
		t.Fatalf("parse fixture %s: %v", name, err)
	}
	d := Diagnose(out)
	return PlanTree(out, d, out.ExecutionTime > 0), d
}

func TestPlanTree_MarksFlaggedNode(t *testing.T) {
	tree, _ := planTreeFixture(t, "seqscan_filter_noanalyze.json")
	// The flagged node — and only it — carries the marker, on the Seq Scan line.
	if strings.Count(tree, "← flagged") != 1 {
		t.Fatalf("expected exactly one flag marker, got:\n%s", tree)
	}
	for _, line := range strings.Split(tree, "\n") {
		if strings.Contains(line, "← flagged") && !strings.Contains(line, "Seq Scan") {
			t.Fatalf("marker should sit on the Seq Scan line, got %q", line)
		}
	}
	// Parallel-aware scans read like psql, and the filter predicate is shown as evidence.
	if !strings.Contains(tree, "Parallel Seq Scan on awards") {
		t.Fatalf("expected a parallel seq scan label, got:\n%s", tree)
	}
	if !strings.Contains(tree, "Filter: (pop_start") {
		t.Fatalf("expected the filter sub-line, got:\n%s", tree)
	}
	if !strings.Contains(tree, "workers planned: 2") {
		t.Fatalf("expected the worker count on the Gather, got:\n%s", tree)
	}
}

func TestPlanTree_HealthyPlanHasNoMarker(t *testing.T) {
	// A clean plan still renders (the most useful -v case), but nothing is flagged.
	clean := `[{"Plan":{"Node Type":"Index Scan","Relation Name":"orders","Index Name":"orders_pkey",
	"Index Cond":"(id = 1)","Total Cost":8.30,"Plan Rows":1}}]`
	out, err := Parse([]byte(clean))
	if err != nil {
		t.Fatal(err)
	}
	d := Diagnose(out)
	tree := PlanTree(out, d, false)
	if tree == "" {
		t.Fatal("a healthy plan should still render a tree under -v")
	}
	if strings.Contains(tree, "← flagged") {
		t.Fatalf("a healthy plan must not flag any node, got:\n%s", tree)
	}
	if !strings.Contains(tree, "Index Scan using orders_pkey on orders") || !strings.Contains(tree, "Index Cond: (id = 1)") {
		t.Fatalf("expected the index scan named with its cond, got:\n%s", tree)
	}
}
