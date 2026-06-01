package explain

// Plan node types mirror the shape of `EXPLAIN (FORMAT JSON)` output.
//
// NOTE (D3/D5): the JSON shape varies across Postgres 13–17. This struct covers
// the fields v0.1 needs; the version matrix lives in the fixture-driven parser
// tests. Unknown fields are ignored by encoding/json, and unknown node types are
// handled gracefully by the diagnoser (treated as opaque, never a crash).

// ExplainOutput is the top-level array element returned by EXPLAIN (FORMAT JSON).
type ExplainOutput struct {
	Plan          PlanNode `json:"Plan"`
	PlanningTime  float64  `json:"Planning Time,omitempty"`  // ms
	ExecutionTime float64  `json:"Execution Time,omitempty"` // ms, only with ANALYZE
}

// PlanNode is one node in the query plan tree.
//
//	┌─────────────┐
//	│  PlanNode   │  NodeType, costs, est/actual rows
//	└──────┬──────┘
//	 Plans │ (children, recursive)
//	┌──────▼──────┐
//	│  PlanNode   │
//	└─────────────┘
type PlanNode struct {
	NodeType            string     `json:"Node Type"`
	ParallelAware       bool       `json:"Parallel Aware"`         // true => rendered "Parallel <type>"
	PartialMode         string     `json:"Partial Mode,omitempty"` // "Partial"/"Finalize" => two-phase aggregate
	WorkersPlanned      *int       `json:"Workers Planned"`        // set on Gather/Gather Merge
	RelationName        string     `json:"Relation Name,omitempty"`
	IndexName           string     `json:"Index Name,omitempty"`
	IndexCond           string     `json:"Index Cond,omitempty"` // empty => index scanned end-to-end (no bounds)
	StartupCost         float64    `json:"Startup Cost"`
	TotalCost           float64    `json:"Total Cost"`
	PlanRows            float64    `json:"Plan Rows"`    // estimated
	ActualRows          *float64   `json:"Actual Rows"`  // nil unless ANALYZE
	ActualLoops         *float64   `json:"Actual Loops"` // nil unless ANALYZE
	Filter              string     `json:"Filter,omitempty"`
	RowsRemovedByFilter *float64   `json:"Rows Removed by Filter"` // nil unless ANALYZE + a filter
	SortMethod          string     `json:"Sort Method,omitempty"`
	SortSpaceType       string     `json:"Sort Space Type,omitempty"` // "Disk" => spilled
	SortSpaceUsed       *float64   `json:"Sort Space Used"`           // KB
	Plans               []PlanNode `json:"Plans,omitempty"`
}

// Walk visits this node and all descendants, depth-first.
func (n *PlanNode) Walk(fn func(*PlanNode)) {
	fn(n)
	for i := range n.Plans {
		n.Plans[i].Walk(fn)
	}
}

// Costliest returns the descendant (including self) with the highest Total Cost.
func (n *PlanNode) Costliest() *PlanNode {
	worst := n
	n.Walk(func(c *PlanNode) {
		if c.TotalCost > worst.TotalCost {
			worst = c
		}
	})
	return worst
}
