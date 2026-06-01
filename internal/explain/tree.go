package explain

import (
	"fmt"
	"strings"
)

// PlanTree renders the parsed plan as an indented, annotated tree — the evidence behind
// the diagnosis, shown on demand (explain -v). It is built from the JSON pgdx already
// parsed, so it works in every mode (live, --pid, --analyze, and offline --plan) without
// re-running EXPLAIN, and it marks the node(s) the diagnosis flagged so the prose and the
// plan line up. Returns "" only if the plan is empty.
//
// This is deliberately NOT a byte-for-byte clone of psql's EXPLAIN text: it shows the
// fields pgdx reasons about (access method, cost, rows, filter/index cond, sort, parallel
// degree) and annotates the flagged node — which raw psql output can't do.
func PlanTree(out *ExplainOutput, d Diagnosis, analyzed bool) string {
	if out == nil {
		return ""
	}
	flagged := make(map[*PlanNode]bool, len(d.Findings))
	for i := range d.Findings {
		if n := d.Findings[i].node; n != nil {
			flagged[n] = true
		}
	}
	var b strings.Builder
	writePlanNode(&b, &out.Plan, 0, flagged, analyzed)
	return strings.TrimRight(b.String(), "\n")
}

// nodeLabel renders a node's headline the way psql does: a "Parallel " prefix for
// parallel-aware scans, then the object it touches ("<index> on <rel>" for index scans,
// "on <rel>" otherwise).
func nodeLabel(n *PlanNode) string {
	name := n.NodeType
	if n.ParallelAware {
		name = "Parallel " + name
	}
	switch {
	case n.IndexName != "" && strings.Contains(n.NodeType, "Index"):
		name += " using " + n.IndexName
		if n.RelationName != "" {
			name += " on " + n.RelationName
		}
	case n.RelationName != "":
		name += " on " + n.RelationName
	}
	return name
}

// writePlanNode renders one node and recurses into its children, depth-first.
func writePlanNode(b *strings.Builder, n *PlanNode, depth int, flagged map[*PlanNode]bool, analyzed bool) {
	indent := strings.Repeat("  ", depth)

	line := indent + nodeLabel(n)

	metrics := fmt.Sprintf("cost ~%.0f, rows %.0f", n.TotalCost, n.PlanRows)
	if analyzed && n.ActualRows != nil {
		metrics += fmt.Sprintf("; actual rows %.0f", *n.ActualRows)
	}
	line += fmt.Sprintf("  (%s)", metrics)

	if n.WorkersPlanned != nil {
		line += fmt.Sprintf("  [workers planned: %d]", *n.WorkersPlanned)
	}
	if flagged[n] {
		line += "   ← flagged"
	}
	b.WriteString(line + "\n")

	// Sub-lines (the predicates/sort detail the diagnosis keys on), indented one deeper.
	sub := strings.Repeat("  ", depth+1)
	if n.IndexCond != "" {
		b.WriteString(sub + "Index Cond: " + n.IndexCond + "\n")
	}
	if n.Filter != "" {
		b.WriteString(sub + "Filter: " + n.Filter + "\n")
	}
	if n.SortMethod != "" || n.SortSpaceType != "" {
		detail := "Sort Method: " + n.SortMethod
		if n.SortSpaceType == "Disk" {
			detail += " (spilled to disk)"
		}
		b.WriteString(sub + strings.TrimSpace(detail) + "\n")
	}

	for i := range n.Plans {
		writePlanNode(b, &n.Plans[i], depth+1, flagged, analyzed)
	}
}
