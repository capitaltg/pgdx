package snapshot

import (
	"sort"
	"strconv"

	"github.com/capitaltg/pgdx/internal/catalog"
)

// StmtDelta is one query's change between two snapshots. Counters are cumulative, so
// every field is newer-minus-older; MeanMs is the average time of just the calls made in
// the interval (dTotalMs/dCalls) — the honest "how slow was it during this window".
type StmtDelta struct {
	QueryID     *int64  `json:"queryid"`
	Query       string  `json:"query"`
	Calls       int64   `json:"calls"`
	TotalMs     float64 `json:"total_ms"`
	MeanMs      float64 `json:"mean_ms"`
	Rows        int64   `json:"rows"`
	SharedRead  int64   `json:"shared_blks_read"`
	TempWritten int64   `json:"temp_blks_written"`
	WalBytes    int64   `json:"wal_bytes"`
	IsNew       bool    `json:"is_new"` // first seen in the newer snapshot (or counters reset)
}

// stmtKey identifies a statement across snapshots: the queryid when present, else the
// (normalized) query text, so diffing still works when query-id computation is off.
func stmtKey(r catalog.StmtStatRow) string {
	if r.QueryID != nil {
		return "id:" + strconv.FormatInt(*r.QueryID, 10)
	}
	return "q:" + r.Query
}

// DiffStatements returns per-query deltas between two snapshots, busiest-by-added-time
// first. Queries with no new calls are omitted. A query whose counters went backwards
// (pg_stat_statements was reset between snapshots) is treated as brand-new accumulation
// and flagged IsNew, so a reset never produces misleading negative deltas.
func DiffStatements(older, newer *Snapshot) []StmtDelta {
	prev := make(map[string]catalog.StmtStatRow, len(older.Statements))
	for _, r := range older.Statements {
		prev[stmtKey(r)] = r
	}
	var out []StmtDelta
	for _, n := range newer.Statements {
		o, ok := prev[stmtKey(n)]
		reset := n.Calls < o.Calls // counters can only grow; a drop means a reset
		isNew := !ok || reset
		var base catalog.StmtStatRow
		if !isNew {
			base = o
		}
		d := StmtDelta{
			QueryID:     n.QueryID,
			Query:       n.Query,
			Calls:       n.Calls - base.Calls,
			TotalMs:     n.TotalMs - base.TotalMs,
			Rows:        n.Rows - base.Rows,
			SharedRead:  n.SharedRead - base.SharedRead,
			TempWritten: n.TempWritten - base.TempWritten,
			WalBytes:    n.WalBytes - base.WalBytes,
			IsNew:       isNew,
		}
		if d.Calls <= 0 && d.TotalMs <= 0 {
			continue // nothing happened for this query in the interval
		}
		if d.Calls > 0 {
			d.MeanMs = d.TotalMs / float64(d.Calls)
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TotalMs > out[j].TotalMs })
	return out
}

// TableDelta is one table's change between two snapshots.
type TableDelta struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	SeqScan int64  `json:"seq_scan"`
	IdxScan int64  `json:"idx_scan"`
	Ins     int64  `json:"inserts"`
	Upd     int64  `json:"updates"`
	Del     int64  `json:"deletes"`
	Writes  int64  `json:"writes"` // ins+upd+del in the interval (the headline)
	IsNew   bool   `json:"is_new"`
}

// DiffTables returns per-table deltas, most-written first. Tables with no activity in the
// interval are omitted. Like statements, a backwards counter (stats reset) is treated as
// fresh accumulation.
func DiffTables(older, newer *Snapshot) []TableDelta {
	prev := make(map[string]catalog.TableStatRow, len(older.Tables))
	for _, r := range older.Tables {
		prev[r.Schema+"."+r.Name] = r
	}
	var out []TableDelta
	for _, n := range newer.Tables {
		o, ok := prev[n.Schema+"."+n.Name]
		reset := n.Ins < o.Ins || n.SeqScan < o.SeqScan
		isNew := !ok || reset
		var base catalog.TableStatRow
		if !isNew {
			base = o
		}
		d := TableDelta{
			Schema:  n.Schema,
			Name:    n.Name,
			SeqScan: n.SeqScan - base.SeqScan,
			IdxScan: n.IdxScan - base.IdxScan,
			Ins:     n.Ins - base.Ins,
			Upd:     n.Upd - base.Upd,
			Del:     n.Del - base.Del,
			IsNew:   isNew,
		}
		d.Writes = d.Ins + d.Upd + d.Del
		if d.Writes == 0 && d.SeqScan == 0 && d.IdxScan == 0 {
			continue
		}
		out = append(out, d)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Writes != out[j].Writes {
			return out[i].Writes > out[j].Writes
		}
		return out[i].SeqScan+out[i].IdxScan > out[j].SeqScan+out[j].IdxScan
	})
	return out
}
