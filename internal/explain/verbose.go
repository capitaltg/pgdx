package explain

import (
	"fmt"
	"strings"
)

// Explanation returns plain-language notes about THIS plan, for `explain -vv`. Every
// bullet is derived from the parsed plan (and diagnosis), so it is always correct and
// works in every mode, including offline --plan. Returns nil when there's nothing useful
// to say (a trivial single-node plan with no parallelism, filter, or aggregation).
//
// The bullets target the misconceptions that actually trip people up reading EXPLAIN —
// cost-isn't-rows, per-worker counts, two-phase parallel aggregation — rather than
// narrating every node.
func Explanation(out *ExplainOutput, d Diagnosis, analyzed bool) []string {
	if out == nil {
		return nil
	}
	root := &out.Plan
	var b []string

	// Units: the #1 EXPLAIN misconception — the cost looks like a row count but isn't.
	if root.TotalCost > 0 {
		b = append(b, fmt.Sprintf(
			"Cost (~%.0f here) is the planner's estimate of effort — roughly the number of sequential page reads — NOT a row count.",
			root.TotalCost))
	}

	// What the dominant scan is actually doing, in words.
	if scan := PrimaryScanNode(out, d); scan != nil && strings.Contains(scan.NodeType, "Scan") {
		switch {
		case scan.Filter != "":
			b = append(b, fmt.Sprintf(
				"%q reads every row of %q and tests the filter on each; the planner estimates ~%s rows survive%s — so an index on the filtered column(s) could let it skip most of the table.",
				nodeLabel(scan), relOrUnknown(scan), compactNum(scan.PlanRows), perWorkerNote(scan)))
		case isFullRelationScan(scan):
			b = append(b, fmt.Sprintf(
				"%q reads %q in full — every row is visited%s, which is why the cost is dominated by this node.",
				nodeLabel(scan), relOrUnknown(scan), perWorkerNote(scan)))
		}
	}

	// Parallelism: per-worker row counts are a perennial source of confusion.
	if w := PlannedWorkers(out); w > 0 {
		b = append(b, fmt.Sprintf(
			"This plan is parallel (%d workers planned). On parallel nodes the 'rows' figure is PER WORKER — the table-wide total is roughly that times the number of participants.", w))
	}

	// Two-phase aggregation (how Postgres parallelizes COUNT/SUM/AVG).
	if hasPartialAggregate(root) {
		b = append(b,
			"The aggregate runs in two phases: each worker aggregates its own slice (Partial Aggregate), then the leader combines those partials (Finalize Aggregate). That two-step shape is how Postgres parallelizes COUNT/SUM/AVG.")
	}

	// Startup-heavy plans: most of the cost is paid before the first row appears.
	if root.TotalCost >= minFullScanCost && root.StartupCost >= 0.9*root.TotalCost {
		b = append(b, fmt.Sprintf(
			"Nearly all the cost is startup (~%.0f of ~%.0f): the query must finish aggregating/sorting before it can return even the first row.",
			root.StartupCost, root.TotalCost))
	}

	return b
}

// PrimaryScanNode returns the scan node the diagnosis is most about — a flagged scan if
// there is one, otherwise the costliest scan in the plan — or nil if there are no scans.
// `explain -vvv` uses it to pick which relation to profile.
func PrimaryScanNode(out *ExplainOutput, d Diagnosis) *PlanNode {
	for i := range d.Findings {
		if n := d.Findings[i].node; n != nil && strings.Contains(n.NodeType, "Scan") {
			return n
		}
	}
	var best *PlanNode
	out.Plan.Walk(func(n *PlanNode) {
		if strings.Contains(n.NodeType, "Scan") && (best == nil || n.TotalCost > best.TotalCost) {
			best = n
		}
	})
	return best
}

// PlannedWorkers returns the largest "Workers Planned" in the plan (0 if not parallel).
func PlannedWorkers(out *ExplainOutput) int {
	max := 0
	out.Plan.Walk(func(n *PlanNode) {
		if n.WorkersPlanned != nil && *n.WorkersPlanned > max {
			max = *n.WorkersPlanned
		}
	})
	return max
}

// hasPartialAggregate reports whether any node is a partial aggregate — the tell-tale of
// the two-phase parallel aggregation Postgres uses for COUNT/SUM/AVG.
func hasPartialAggregate(root *PlanNode) bool {
	found := false
	root.Walk(func(n *PlanNode) {
		if n.NodeType == "Aggregate" && n.PartialMode == "Partial" {
			found = true
		}
	})
	return found
}

// ScanCostInputs are the catalog facts behind a scan's planner cost, fetched live for the
// -vvv breakdown (they aren't present in EXPLAIN output, which is why -vvv needs a connection).
type ScanCostInputs struct {
	Reltuples       float64
	Relpages        int64
	SeqPageCost     float64
	CPUTupleCost    float64
	CPUOperatorCost float64
	Workers         int // parallel workers planned feeding this node (0 = none)
}

// SeqScanCost decomposes a (parallel) sequential scan's total cost into the terms
// Postgres's model sums. It is APPROXIMATE: the cost model shifts across versions and the
// filter's per-row cost is estimated as a single comparison. The catalog facts the caller
// shows alongside it are exact; this arithmetic is illustrative.
type SeqScanCost struct {
	DiskCost   float64 `json:"disk_cost"`           // seq_page_cost * relpages (page reads; NOT divided among workers)
	CPURaw     float64 `json:"cpu_cost_raw"`        // (cpu_tuple_cost + filter) * reltuples, before parallel division
	Divisor    float64 `json:"parallel_divisor"`    // parallel divisor (1.0 when not parallel)
	CPUDivided float64 `json:"cpu_cost"`            // CPURaw / Divisor
	Total      float64 `json:"reconstructed_total"` // DiskCost + CPUDivided — should land near node.TotalCost
	HasFilter  bool    `json:"has_filter"`
}

// DecomposeScanCost reconstructs a scan node's cost from catalog facts. The bool is false
// for node types we don't model with confidence (index scans, joins, …) — callers should
// then show the catalog facts alone rather than a possibly-wrong breakdown.
func DecomposeScanCost(n *PlanNode, in ScanCostInputs) (SeqScanCost, bool) {
	if n == nil || n.NodeType != "Seq Scan" {
		return SeqScanCost{}, false
	}
	var c SeqScanCost
	c.DiskCost = in.SeqPageCost * float64(in.Relpages)
	qual := 0.0
	if n.Filter != "" {
		c.HasFilter = true
		qual = in.CPUOperatorCost // one comparison, approximately
	}
	c.CPURaw = (in.CPUTupleCost + qual) * in.Reltuples
	c.Divisor = 1.0
	if n.ParallelAware && in.Workers > 0 {
		c.Divisor = ParallelDivisor(in.Workers)
	}
	c.CPUDivided = c.CPURaw / c.Divisor
	c.Total = c.DiskCost + c.CPUDivided
	return c, true
}

// ParallelDivisor mirrors Postgres's get_parallel_divisor: the worker count plus a reduced
// leader share (the leader also helps scan, but contributes less as worker count rises;
// the contribution floors at zero). For 2 workers this is 2.4.
func ParallelDivisor(workers int) float64 {
	d := float64(workers)
	if leader := 1.0 - 0.3*float64(workers); leader > 0 {
		d += leader
	}
	return d
}
