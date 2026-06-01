package explain

import "testing"

// A real EXPLAIN (FORMAT JSON) fragment (PG 16). The D5 fixture set (13–17) will
// expand this into testdata/ files; this inline case proves the parser wiring.
const samplePlanJSON = `[
  {
    "Plan": {
      "Node Type": "Seq Scan",
      "Relation Name": "orders",
      "Startup Cost": 0.00,
      "Total Cost": 21250.00,
      "Plan Rows": 100,
      "Filter": "(customer_id = 42)",
      "Plans": [
        {
          "Node Type": "Index Scan",
          "Relation Name": "customers",
          "Index Name": "customers_pkey",
          "Startup Cost": 0.29,
          "Total Cost": 8.30,
          "Plan Rows": 1
        }
      ]
    },
    "Planning Time": 0.123
  }
]`

func TestParse(t *testing.T) {
	out, err := Parse([]byte(samplePlanJSON))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if out.Plan.NodeType != "Seq Scan" {
		t.Fatalf("root node type = %q, want Seq Scan", out.Plan.NodeType)
	}
	if out.Plan.RelationName != "orders" {
		t.Fatalf("relation = %q, want orders", out.Plan.RelationName)
	}
	if len(out.Plan.Plans) != 1 {
		t.Fatalf("expected 1 child, got %d", len(out.Plan.Plans))
	}
	// Costliest must find the expensive Seq Scan, not the cheap Index Scan child.
	worst := out.Plan.Costliest()
	if worst.NodeType != "Seq Scan" || worst.TotalCost != 21250.00 {
		t.Fatalf("costliest = %s @ %.2f, want Seq Scan @ 21250.00", worst.NodeType, worst.TotalCost)
	}
}

func TestParse_Errors(t *testing.T) {
	if _, err := Parse([]byte(`not json`)); err == nil {
		t.Fatal("expected error on invalid JSON")
	}
	if _, err := Parse([]byte(`[]`)); err == nil {
		t.Fatal("expected error on empty array")
	}
}
