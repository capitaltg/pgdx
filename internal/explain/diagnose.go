package explain

import (
	"fmt"
	"strings"
)

// Diagnosis is the plain-language result pgdx shows the user.
type Diagnosis struct {
	// Findings are concrete, high-confidence observations. Empty means "no obvious
	// problem found" — which we say out loud rather than inventing a cause (D7).
	Findings []Finding
	// Note explains a no-findings verdict ("why it looks fine"): the access method,
	// cost, and row estimate. Empty when there are findings.
	Note string `json:"note,omitempty"`
}

// Finding is one diagnosed issue.
type Finding struct {
	Title      string `json:"title"`
	Detail     string `json:"detail"`               // the evidence
	Suggestion string `json:"suggestion,omitempty"` // may be empty for an observation
	// IndexSuggestion, when non-empty, is a concrete candidate CREATE INDEX statement
	// addressing this finding. It is populated only when the caller asks for it (via
	// AddIndexSuggestions) and only for findings where columns can be extracted with
	// confidence. It is a STARTING POINT, not a guarantee — see suggest.go.
	IndexSuggestion string `json:"index_suggestion,omitempty"`

	// rel and filter retain the structured evidence needed to synthesize an index
	// suggestion on request. Unexported: they never serialize and never render.
	rel    string
	filter string
	// node points at the plan node this finding is about, so the verbose plan tree can
	// mark it (← flagged) and line the prose up with the plan. Unexported; never serializes.
	node *PlanNode
}

// HasFindings reports whether the diagnosis found anything worth saying.
func (d Diagnosis) HasFindings() bool { return len(d.Findings) > 0 }

// minFullScanCost gates the full-scan pattern: below this planner cost a sequential
// scan is cheap (small table) and flagging it would be a false positive.
const minFullScanCost = 1000.0

// Diagnose applies the high-confidence pattern set to a parsed plan.
//
// D7 (eng review): only patterns with strong signal, otherwise "no obvious problem
// found". The costliest node is usually a symptom, not the cause, so each pattern
// keys on concrete plan evidence (a disk sort, a filter that discards most rows, a
// full-table scan dominating cost) rather than just "this node is expensive".
//
// Parallel plans report Actual Rows / Rows Removed / sort space PER WORKER, so the
// patterns use ratios and qualitative signals, not absolute counts, and de-dup the
// per-worker copies of the same finding.
func Diagnose(out *ExplainOutput) Diagnosis {
	var d Diagnosis
	root := &out.Plan
	rootCost := root.TotalCost

	sawSortSpill := false
	hasAggregate := false
	var fullScan *PlanNode     // costliest no-filter seq scan dominating the plan
	var filteredScan *PlanNode // costliest filtered seq scan dominating the plan, sans ANALYZE evidence

	root.Walk(func(n *PlanNode) {
		if n.NodeType == "Aggregate" {
			hasAggregate = true
		}
		switch {
		// Pattern 1: a sort that spilled to disk (work_mem too small). High confidence.
		case n.NodeType == "Sort" && n.SortSpaceType == "Disk":
			if !sawSortSpill {
				sawSortSpill = true
				space := ""
				if n.SortSpaceUsed != nil {
					space = fmt.Sprintf(" (~%s)", humanizeKB(*n.SortSpaceUsed))
				}
				method := n.SortMethod
				if method == "" {
					method = "external"
				}
				d.Findings = append(d.Findings, Finding{
					Title:      "Sort spilled to disk",
					Detail:     fmt.Sprintf("A %s sort%s exceeded work_mem and spilled to disk — disk sorts are far slower than in-memory ones.", method, space),
					Suggestion: "Raise work_mem for this query/session, or reduce what's sorted (filter rows earlier, select fewer/narrower columns, or add an index that already provides the order).",
					node:       n,
				})
			}

		// Pattern 2: a sequential scan whose filter discards the vast majority of rows
		// it reads — the classic missing-index signature. With ANALYZE we measure it from
		// rows-removed (high confidence); without ANALYZE we fall back to the planner's own
		// estimate — an expensive filtered scan dominating the plan — and say so honestly.
		case n.NodeType == "Seq Scan" && n.Filter != "":
			if n.RowsRemovedByFilter != nil {
				removed := *n.RowsRemovedByFilter
				var returned float64
				if n.ActualRows != nil {
					returned = *n.ActualRows
				}
				if removed >= 1000 && removed >= 10*(returned+1) {
					d.Findings = append(d.Findings, Finding{
						Title: fmt.Sprintf("Sequential scan filters out most rows on %q", relOrUnknown(n)),
						Detail: fmt.Sprintf("The scan discarded ~%s rows to return ~%s (filter: %s)%s. Reading most of a table to keep a few rows is the classic missing-index signature.",
							compactNum(removed), compactNum(returned), n.Filter, perWorkerNote(n)),
						Suggestion: fmt.Sprintf("Consider an index covering the filtered column(s) in: %s", n.Filter),
						rel:        n.RelationName,
						filter:     n.Filter,
						node:       n,
					})
				}
			} else if rootCost > 0 && n.TotalCost >= 0.5*rootCost && n.TotalCost >= minFullScanCost {
				// No ANALYZE: keep the single costliest such scan; emit one finding below.
				if filteredScan == nil || n.TotalCost > filteredScan.TotalCost {
					filteredScan = n
				}
			}

		// Pattern 3 (candidate): a full-relation scan dominating plan cost, AND genuinely
		// expensive. This is a seq scan with no filter OR an index scan with no bounds and
		// no filter — both read the whole relation. The latter is how Postgres satisfies an
		// unfiltered COUNT(*) when any index is available (a full Index Only Scan): just as
		// expensive as a seq scan, but the old pattern missed it and we'd wrongly say "fine".
		// The cost gate is the key D7 guard: a full scan of a tiny table is correct and
		// cheap, NOT a problem — flagging it is confidently wrong.
		case isFullRelationScan(n) &&
			rootCost > 0 && n.TotalCost >= 0.5*rootCost && n.TotalCost >= minFullScanCost:
			if fullScan == nil || n.TotalCost > fullScan.TotalCost {
				fullScan = n
			}
		}
	})

	if fullScan != nil {
		// An index scan reading the whole index (no bounds) is still a full-relation read,
		// but it isn't a "sequential scan" — name it for what it is so the verdict matches
		// what the user sees in the plan. Postgres counts via a full Index Only Scan when an
		// index is available, which is exactly the case the old seq-scan-only pattern missed.
		viaIndex := fullScan.NodeType != "Seq Scan"
		var f Finding
		if viaIndex {
			f.Title = fmt.Sprintf("Full index scan on %q dominates the query", relOrUnknown(fullScan))
		} else {
			f.Title = fmt.Sprintf("Full sequential scan on %q dominates the query", relOrUnknown(fullScan))
		}

		// Tailor the advice: an aggregate (COUNT/SUM over the whole table) can't be helped by
		// an index at all; a plain full SELECT can often be helped by a filter/LIMIT/index.
		switch {
		case hasAggregate && viaIndex:
			f.Detail = fmt.Sprintf("The plan counts over the whole table by reading index %q end-to-end (~%s entries), and that scan is the bulk of the cost. "+
				"Scanning the entire index avoids a table fetch but is NOT cheap — Postgres still reads every entry, and no index can shortcut an unfiltered COUNT(*).",
				idxOrRel(fullScan), compactNum(fullScan.PlanRows))
			f.Suggestion = "If an exact, live count isn't required, pg_class.reltuples gives an instant estimate. For a fast exact count, maintain a summary/counter table (e.g. updated by triggers)."
		case hasAggregate:
			f.Detail = "The plan aggregates over the entire table with no filter, and that scan is the bulk of the cost. " +
				"An unfiltered aggregate like COUNT(*) over a whole table can't be sped up by an index — Postgres must read every row."
			f.Suggestion = "If an exact, live count isn't required, pg_class.reltuples gives an instant estimate. For a fast exact count, maintain a summary/counter table (e.g. updated by triggers)."
		case viaIndex:
			f.Detail = fmt.Sprintf("The plan reads index %q end-to-end with no bounds, and that scan is the bulk of the cost on a large table.", idxOrRel(fullScan))
			f.Suggestion = "If you don't need every row, add a WHERE filter or a LIMIT so Postgres can seek into the index instead of scanning all of it."
		default:
			f.Detail = "The plan reads the entire table with no filter, and that scan is the bulk of the cost on a large table."
			f.Suggestion = "If you don't need every row, add a WHERE filter or a LIMIT. If you do filter, an index on the filtered column(s) avoids the full scan."
		}
		f.node = fullScan
		d.Findings = append(d.Findings, f)
	}

	if filteredScan != nil {
		// We have no rows-removed evidence (no --analyze), so we can't measure selectivity —
		// say so plainly rather than over-claim. But an expensive filtered scan dominating
		// the plan is the typical missing-index shape and worth surfacing, with --analyze as
		// the confirming next step. rel/filter are set so --suggest-index can still offer DDL.
		d.Findings = append(d.Findings, Finding{
			Title: fmt.Sprintf("Expensive sequential scan with a filter on %q dominates the query", relOrUnknown(filteredScan)),
			Detail: fmt.Sprintf("Postgres reads the whole table to apply the filter (%s), and that scan is the bulk of the cost (~%.0f). "+
				"Without --analyze I can't measure how many rows the filter discards, but a scan this expensive to satisfy a filter is the typical missing-index shape.",
				filteredScan.Filter, filteredScan.TotalCost),
			Suggestion: fmt.Sprintf("Re-run with --analyze to confirm how selective the filter is, then consider an index covering the filtered column(s) in: %s", filteredScan.Filter),
			rel:        filteredScan.RelationName,
			filter:     filteredScan.Filter,
			node:       filteredScan,
		})
	}

	// When nothing fired, explain WHY the plan looks fine instead of a bare shrug.
	if !d.HasFindings() {
		d.Note = healthyReason(out)
	}
	return d
}

// healthyReason explains a clean verdict: the dominant access method, the estimated
// cost, and rows out. This turns "No obvious problem found" into something the user
// can act on ("it's using the pkey index, ~1 row, cheap — nothing to do here").
func healthyReason(out *ExplainOutput) string {
	root := &out.Plan
	var access *PlanNode // costliest scan node
	root.Walk(func(n *PlanNode) {
		if strings.Contains(n.NodeType, "Scan") {
			if access == nil || n.TotalCost > access.TotalCost {
				access = n
			}
		}
	})

	var why string
	if access != nil {
		switch {
		case strings.Contains(access.NodeType, "Index Only Scan"):
			why = fmt.Sprintf("the main access is an index-only scan using %q (no table fetch needed)", idxOrRel(access))
		case strings.Contains(access.NodeType, "Index Scan"):
			why = fmt.Sprintf("the main access is an index scan using %q", idxOrRel(access))
		case strings.Contains(access.NodeType, "Bitmap"):
			why = fmt.Sprintf("the main access uses a bitmap index scan on %q", relOrUnknown(access))
		case access.NodeType == "Seq Scan":
			// Only call a seq scan cheap when it actually is — claiming "cost is low" on a
			// multi-million-cost scan is the contradiction this verdict used to print.
			if access.TotalCost < minFullScanCost {
				why = fmt.Sprintf("the main access is a sequential scan on %q, but its cost is low (small table or cheap scan)", relOrUnknown(access))
			} else {
				why = fmt.Sprintf("the main access is a sequential scan on %q that isn't the bulk of the plan's cost", relOrUnknown(access))
			}
		default:
			why = fmt.Sprintf("the main access is a %s", access.NodeType)
		}
	}

	cost := fmt.Sprintf("estimated cost ~%.0f, ~%s rows out", root.TotalCost, compactNum(root.PlanRows))
	if why == "" {
		return cost + "."
	}
	return why + "; " + cost + "."
}

// isFullRelationScan reports whether a node reads its whole relation: a Seq Scan with
// no filter, or an Index/Index Only Scan with neither bounds (Index Cond) nor a filter.
// A selective index scan (Index Cond set) seeks to a few rows and must NOT be flagged —
// keeping this conservative is what stops false positives (D7).
func isFullRelationScan(n *PlanNode) bool {
	switch n.NodeType {
	case "Seq Scan":
		return n.Filter == ""
	case "Index Only Scan", "Index Scan":
		return n.Filter == "" && n.IndexCond == ""
	default:
		return false
	}
}

func idxOrRel(n *PlanNode) string {
	if n.IndexName != "" {
		return n.IndexName
	}
	return relOrUnknown(n)
}

func relOrUnknown(n *PlanNode) string {
	if n.RelationName != "" {
		return n.RelationName
	}
	return n.NodeType
}

// perWorkerNote flags that counts are per-worker in a parallel scan, so the user
// isn't confused when "rows removed" looks smaller than the table.
func perWorkerNote(n *PlanNode) string {
	if n.ActualLoops != nil && *n.ActualLoops > 1 {
		return " (per parallel worker)"
	}
	return ""
}

// compactNum renders a row count approximately: 1234567 -> "1.2M", 4200 -> "4.2k".
func compactNum(n float64) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", n/1_000_000)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", n/1_000)
	default:
		return fmt.Sprintf("%.0f", n)
	}
}

// humanizeKB turns a kilobyte count into a readable size.
func humanizeKB(kb float64) string {
	switch {
	case kb >= 1_048_576:
		return fmt.Sprintf("%.1f GB", kb/1_048_576)
	case kb >= 1024:
		return fmt.Sprintf("%.1f MB", kb/1024)
	default:
		return fmt.Sprintf("%.0f kB", kb)
	}
}
