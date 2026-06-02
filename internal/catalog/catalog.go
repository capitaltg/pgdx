// Package catalog reads Postgres system catalogs for the browse commands
// (`pgdx get ...`). It is read-only and depends only on catalogs every role can
// see (pg_class, pg_namespace), so it needs no special privileges.
package catalog

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Querier is the slice of a connection catalog queries need. *db.Conn satisfies it.
type Querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Table is one row of `pgdx get tables`.
type Table struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Owner   string `json:"owner"`
	EstRows int64  `json:"est_rows"`    // planner estimate from pg_class.reltuples; -1 = never analyzed
	Size    string `json:"size"`        // pg_size_pretty(pg_total_relation_size)
	LiveTup int64  `json:"live_tuples"` // pg_stat_all_tables.n_live_tup
	DeadTup int64  `json:"dead_tuples"` // n_dead_tup — high ratio => bloat / needs vacuum
}

// buildTablesQuery returns the SQL (and args) for listing tables. With an empty
// schema it covers every non-system schema; otherwise it filters to one.
//
// We exclude system schemas with the anchored regex `^pg_` (so pg_catalog,
// pg_toast, pg_temp_* are dropped but a user schema like "pgdata" is kept) plus
// information_schema. relkind 'r'/'p' = ordinary and partitioned tables.
func buildTablesQuery(schema string) (string, []any) {
	q := `SELECT n.nspname AS schema,
       c.relname AS name,
       pg_catalog.pg_get_userbyid(c.relowner) AS owner,
       c.reltuples::bigint AS est_rows,
       pg_catalog.pg_size_pretty(pg_catalog.pg_total_relation_size(c.oid)) AS size,
       COALESCE(s.n_live_tup, 0) AS live_tup,
       COALESCE(s.n_dead_tup, 0) AS dead_tup
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r', 'p')
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'`
	var args []any
	if schema != "" {
		q += "\n  AND n.nspname = $1"
		args = append(args, schema)
	}
	q += "\nORDER BY n.nspname, c.relname"
	return q, args
}

// Index is one row of `pgdx get indexes`.
type Index struct {
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Name       string `json:"name"`
	Unique     bool   `json:"unique"`
	Size       string `json:"size"`       // pg_size_pretty(pg_relation_size)
	Scans      int64  `json:"scans"`      // idx_scan from pg_stat_user_indexes; 0 = never used
	Definition string `json:"definition"` // pg_get_indexdef — shown in -o json, not the table view
}

// buildIndexesQuery returns the SQL (and args) for listing indexes, optionally
// filtered by schema and/or table. Scan counts come from pg_stat_user_indexes so
// callers can spot unused indexes (scans = 0). Filters are parameterized.
// indexSorts maps a --sort key to a complete ORDER BY expression (direction included, so
// numeric keys sort biggest/most-used first). Every key carries the schema/table/name
// ordering as a tiebreaker, so equal-valued rows come out in a stable, readable order.
var indexSorts = map[string]string{
	"name":  "n.nspname, t.relname, i.relname",
	"size":  "pg_catalog.pg_relation_size(i.oid) DESC, n.nspname, t.relname, i.relname",
	"scans": "COALESCE(s.idx_scan, 0) DESC, n.nspname, t.relname, i.relname",
}

// IndexSortKeys lists the valid `get indexes --sort` values (for the usage error message).
var IndexSortKeys = []string{"name", "size", "scans"}

// ValidIndexSort reports whether key is an accepted --sort value.
func ValidIndexSort(key string) bool {
	_, ok := indexSorts[key]
	return ok
}

func buildIndexesQuery(schema, table string, unusedOnly bool, sortKey string) (string, []any) {
	q := `SELECT n.nspname AS schema,
       t.relname AS tablename,
       i.relname AS name,
       ix.indisunique AS is_unique,
       pg_catalog.pg_size_pretty(pg_catalog.pg_relation_size(i.oid)) AS size,
       COALESCE(s.idx_scan, 0) AS scans,
       pg_catalog.pg_get_indexdef(i.oid) AS definition
FROM pg_catalog.pg_index ix
JOIN pg_catalog.pg_class i ON i.oid = ix.indexrelid
JOIN pg_catalog.pg_class t ON t.oid = ix.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid = i.relnamespace
LEFT JOIN pg_catalog.pg_stat_user_indexes s ON s.indexrelid = ix.indexrelid
WHERE n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'`
	var args []any
	if schema != "" {
		args = append(args, schema)
		q += fmt.Sprintf("\n  AND n.nspname = $%d", len(args))
	}
	if table != "" {
		args = append(args, table)
		q += fmt.Sprintf("\n  AND t.relname = $%d", len(args))
	}
	if unusedOnly {
		// Never-scanned, non-unique indexes = the safe drop candidates. Unique indexes
		// (incl. PKs) are excluded: they earn their keep enforcing constraints even at 0 scans.
		q += "\n  AND COALESCE(s.idx_scan, 0) = 0\n  AND NOT ix.indisunique"
	}
	orderBy, ok := indexSorts[sortKey]
	if !ok {
		orderBy = indexSorts["name"]
	}
	q += "\nORDER BY " + orderBy
	return q, args
}

// StatsResetAge returns when the current database's cumulative stats (incl. idx_scan)
// last reset, and the elapsed seconds. resetText is "" / ageSec -1 when never reset
// (counters span the full stats history). Used to judge how trustworthy a "0 scans" is.
func StatsResetAge(ctx context.Context, q Querier) (resetText string, ageSec float64, err error) {
	const sql = `SELECT COALESCE(stats_reset::text, ''),
       COALESCE(EXTRACT(epoch FROM (now() - stats_reset)), -1)::float8
FROM pg_catalog.pg_stat_database
WHERE datname = current_database()`
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return "", -1, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&resetText, &ageSec); err != nil {
			return "", -1, err
		}
	}
	return resetText, ageSec, rows.Err()
}

// ListIndexes returns indexes, optionally filtered by schema and/or table name and ordered
// by sortKey (one of IndexSortKeys; unknown falls back to "name"). With unusedOnly, only
// never-scanned non-unique indexes (drop candidates) are returned.
func ListIndexes(ctx context.Context, q Querier, schema, table string, unusedOnly bool, sortKey string) ([]Index, error) {
	sql, args := buildIndexesQuery(schema, table, unusedOnly, sortKey)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Index
	for rows.Next() {
		var ix Index
		if err := rows.Scan(&ix.Schema, &ix.Table, &ix.Name, &ix.Unique, &ix.Size, &ix.Scans, &ix.Definition); err != nil {
			return nil, err
		}
		out = append(out, ix)
	}
	return out, rows.Err()
}

// RedundantIndex is one row of `pgdx get indexes --redundant`: a non-unique index whose
// leading columns are already covered by another index on the same table, so it earns
// nothing on reads while still costing write amplification and disk.
type RedundantIndex struct {
	Schema    string `json:"schema"`
	Table     string `json:"table"`
	Name      string `json:"name"`
	Size      string `json:"size"`
	Scans     int64  `json:"scans"`
	CoveredBy string `json:"covered_by"` // the index that makes this one redundant
	Reason    string `json:"reason"`     // "duplicate of X" | "leading columns are a prefix of X"
}

// idxKeyRow is the raw per-index data redundancy analysis needs.
type idxKeyRow struct {
	schema, table, name, am string
	cols                    []int16
	unique, special         bool // special = partial (predicate) or expression index — never compared
	sizeBytes, scans        int64
}

func buildRedundantIndexesQuery(schema, table string) (string, []any) {
	// indkey::text is the space-separated attnum list (e.g. "1 2"); a 0 entry marks an
	// expression column. We bring back the predicate/expression flags so the analyzer can
	// refuse to compare partial or expression indexes (whose equivalence isn't decidable
	// from columns alone). Only valid, ready indexes are considered.
	q := `SELECT n.nspname, t.relname, i.relname, am.amname,
       ix.indkey::text,
       ix.indisunique,
       (ix.indpred IS NOT NULL OR ix.indexprs IS NOT NULL) AS special,
       pg_catalog.pg_relation_size(i.oid)::bigint,
       COALESCE(s.idx_scan, 0)
FROM pg_catalog.pg_index ix
JOIN pg_catalog.pg_class i ON i.oid = ix.indexrelid
JOIN pg_catalog.pg_class t ON t.oid = ix.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid = i.relnamespace
JOIN pg_catalog.pg_am am ON am.oid = i.relam
LEFT JOIN pg_catalog.pg_stat_user_indexes s ON s.indexrelid = ix.indexrelid
WHERE n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'
  AND ix.indisvalid AND ix.indisready`
	var args []any
	if schema != "" {
		args = append(args, schema)
		q += fmt.Sprintf("\n  AND n.nspname = $%d", len(args))
	}
	if table != "" {
		args = append(args, table)
		q += fmt.Sprintf("\n  AND t.relname = $%d", len(args))
	}
	q += "\nORDER BY n.nspname, t.relname, i.relname"
	return q, args
}

// parseIndkey parses an indkey::text value ("1 2 3") into attnums. A 0 entry (an
// expression column) makes the whole key non-comparable, signaled by ok=false.
func parseIndkey(s string) (cols []int16, ok bool) {
	for _, f := range strings.Fields(s) {
		n, err := strconv.Atoi(f)
		if err != nil || n == 0 {
			return nil, false
		}
		cols = append(cols, int16(n))
	}
	return cols, len(cols) > 0
}

// isColPrefix reports whether a is a (non-strict) prefix of b: a[i]==b[i] for all of a.
func isColPrefix(a, b []int16) bool {
	if len(a) > len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// redundancyOf returns why index a is redundant given covering index b, or ok=false.
// a must be non-unique, plain (not partial/expression), same table and access method as
// b, and its column list a prefix of b's. For an exact-column duplicate, only the "loser"
// of a stable tie-break is flagged, so the same pair never reports both indexes.
func redundancyOf(a, b idxKeyRow) (reason string, ok bool) {
	if a.name == b.name || a.table != b.table || a.schema != b.schema || a.am != b.am {
		return "", false
	}
	if a.special || b.special || a.unique {
		return "", false // never flag a unique/PK, or reason about partial/expression indexes
	}
	if !isColPrefix(a.cols, b.cols) {
		return "", false
	}
	if len(a.cols) < len(b.cols) {
		return "leading columns are a prefix of " + b.name, true
	}
	// Equal columns: a duplicate. Keep exactly one — b wins if it's unique, else the one
	// with more scans, then smaller, then lexically-smaller name; a is flagged when it loses.
	if duplicateLoser(a, b) {
		return "duplicate of " + b.name, true
	}
	return "", false
}

func duplicateLoser(a, b idxKeyRow) bool {
	if a.unique != b.unique {
		return !a.unique // a is the loser when it's the non-unique one
	}
	if a.scans != b.scans {
		return a.scans < b.scans
	}
	if a.sizeBytes != b.sizeBytes {
		return a.sizeBytes > b.sizeBytes
	}
	return a.name > b.name
}

// ListRedundantIndexes returns non-unique indexes whose leading columns are already
// covered by another index on the same table (a superset or an exact duplicate). Only
// plain B-tree-style column indexes are compared; partial and expression indexes are
// left out (their equivalence can't be judged from columns alone). Optional schema/table
// filters narrow the scope.
func ListRedundantIndexes(ctx context.Context, q Querier, schema, table string) ([]RedundantIndex, error) {
	sql, args := buildRedundantIndexesQuery(schema, table)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var all []idxKeyRow
	for rows.Next() {
		var r idxKeyRow
		var indkey string
		if err := rows.Scan(&r.schema, &r.table, &r.name, &r.am, &indkey, &r.unique, &r.special, &r.sizeBytes, &r.scans); err != nil {
			return nil, err
		}
		cols, ok := parseIndkey(indkey)
		if !ok {
			r.special = true // treat un-parseable / expression keys as non-comparable
		}
		r.cols = cols
		all = append(all, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	var out []RedundantIndex
	for i := range all {
		a := all[i]
		var best idxKeyRow
		var bestReason string
		for j := range all {
			if i == j {
				continue
			}
			reason, ok := redundancyOf(a, all[j])
			if !ok {
				continue
			}
			// Prefer the most general covering index (most columns); a tie keeps the first.
			if bestReason == "" || len(all[j].cols) > len(best.cols) {
				best, bestReason = all[j], reason
			}
		}
		if bestReason != "" {
			out = append(out, RedundantIndex{
				Schema: a.schema, Table: a.table, Name: a.name,
				Size: byteSize(a.sizeBytes), Scans: a.scans,
				CoveredBy: best.name, Reason: bestReason,
			})
		}
	}
	return out, nil
}

// byteSize renders a byte count like pg_size_pretty (kept here so catalog has no
// dependency on the cmd-layer formatter).
func byteSize(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d bytes", n)
	}
	val := float64(n)
	units := []string{"kB", "MB", "GB", "TB"}
	i := -1
	for val >= unit && i < len(units)-1 {
		val /= unit
		i++
	}
	return fmt.Sprintf("%.0f %s", val, units[i])
}

// Activity is one row of `pgdx get activity` (a session in pg_stat_activity).
type Activity struct {
	PID           int32   `json:"pid"`
	User          string  `json:"user"`
	Database      string  `json:"database"`
	ClientAddr    string  `json:"client_addr"` // client IP, "local" for unix-socket connections
	State         string  `json:"state"`
	WaitEventType string  `json:"wait_event_type,omitempty"`
	WaitEvent     string  `json:"wait_event,omitempty"`
	DurationSec   float64 `json:"duration_sec"`         // seconds in the CURRENT state: query runtime if active, else time idle; -1 = unknown
	BlockedBy     string  `json:"blocked_by,omitempty"` // comma-separated PIDs blocking this one
	Query         string  `json:"query"`
}

// HasMonitorPrivilege reports whether the current role can see other sessions'
// query text (superuser or pg_monitor member). Without it, pg_stat_activity masks
// other users' queries — so callers should warn (D8).
func HasMonitorPrivilege(ctx context.Context, q Querier) (bool, error) {
	const sql = `SELECT current_setting('is_superuser')::bool
                    OR pg_catalog.pg_has_role(current_user, 'pg_monitor', 'MEMBER')`
	var ok bool
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&ok); err != nil {
			return false, err
		}
	}
	return ok, rows.Err()
}

// buildActivityQuery lists sessions. By default it shows only client backends doing
// something (state <> 'idle') and excludes pgdx's own session; --all includes idle
// connections and background workers.
//
// datname, when non-empty, scopes the (cluster-wide) list to one database. It's a
// FILTER on the catalog view, not a connection target — so it works from any database
// the role can reach and needs no CONNECT on the named one. It's parameterized.
//
// sort "blocked" (default): blocked sessions first (the symptom you're chasing in an
// incident), then longest-running. sort "duration": longest-running first.
func buildActivityQuery(all bool, sort, datname string, minDurationSec float64) (string, []any) {
	// DURATION is time in the CURRENT state, not always since query_start: for an active
	// session that's how long its query has run (query_start); for idle / idle-in-tx it's
	// how long it's sat in that state (state_change). Using query_start for an idle row is
	// the classic trap — it makes a long-parked pool connection look like a query that ran
	// for minutes. Pairing the metric with STATE keeps it honest: "active 60s" = a 60s
	// query; "idle 60s" = a connection idle for 60s.
	const durExpr = "(now() - CASE WHEN state = 'active' THEN query_start ELSE state_change END)"
	q := `SELECT pid,
       COALESCE(usename, ''),
       COALESCE(datname, ''),
       COALESCE(host(client_addr), 'local'),
       COALESCE(state, ''),
       COALESCE(wait_event_type, ''),
       COALESCE(wait_event, ''),
       COALESCE(EXTRACT(epoch FROM ` + durExpr + `), -1)::float8,
       array_to_string(pg_catalog.pg_blocking_pids(pid), ','),
       COALESCE(query, '')
FROM pg_catalog.pg_stat_activity
WHERE pid <> pg_catalog.pg_backend_pid()`
	if !all {
		q += "\n  AND backend_type = 'client backend'\n  AND state IS DISTINCT FROM 'idle'"
	}
	var args []any
	if datname != "" {
		args = append(args, datname)
		q += fmt.Sprintf("\n  AND datname = $%d", len(args))
	}
	// --min-duration: keep only sessions whose time-in-current-state is at least this many
	// seconds. Read with the default (idle hidden) view, that means "queries running longer
	// than N" — the long-running-query filter. The threshold is parameterized (no injection).
	if minDurationSec > 0 {
		args = append(args, minDurationSec)
		q += fmt.Sprintf("\n  AND COALESCE(EXTRACT(epoch FROM %s), 0) >= $%d", durExpr, len(args))
	}
	byDuration := "COALESCE(EXTRACT(epoch FROM " + durExpr + "), 0) DESC"
	if sort == "duration" {
		q += "\nORDER BY " + byDuration
	} else {
		// Blocked sessions (non-empty pg_blocking_pids) float to the top, then by duration.
		q += "\nORDER BY (cardinality(pg_catalog.pg_blocking_pids(pid)) > 0) DESC, " + byDuration
	}
	return q, args
}

// ListActivity returns current sessions, ordered per sort ("blocked" or "duration"),
// optionally scoped to one database (datname; "" = whole cluster) and to sessions that
// have been in their current state at least minDurationSec seconds (0 = no filter).
func ListActivity(ctx context.Context, q Querier, all bool, sort, datname string, minDurationSec float64) ([]Activity, error) {
	sql, args := buildActivityQuery(all, sort, datname, minDurationSec)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Activity
	for rows.Next() {
		var a Activity
		if err := rows.Scan(&a.PID, &a.User, &a.Database, &a.ClientAddr, &a.State,
			&a.WaitEventType, &a.WaitEvent, &a.DurationSec, &a.BlockedBy, &a.Query); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SlowQuery is one row of `pgdx get slow-queries` (from pg_stat_statements).
//
// Beyond the headline total/mean, it carries the axes a tuning session actually
// re-sorts by: stddev (a high one means the query is sometimes catastrophic, not
// uniformly slow), shared-buffer hit/read (HitPct — a low ratio means the query is
// thrashing the cache, reading from disk), temp blocks (sorts/hashes spilling to
// disk), and WAL bytes (write amplification). All column names are stable across the
// supported PG13–17 range (the blk_*_time columns, which were renamed in PG17, are
// deliberately avoided).
type SlowQuery struct {
	Calls       int64   `json:"calls"`
	TotalMs     float64 `json:"total_ms"`
	MeanMs      float64 `json:"mean_ms"`
	StddevMs    float64 `json:"stddev_ms"`
	MinMs       float64 `json:"min_ms"`
	MaxMs       float64 `json:"max_ms"`
	Rows        int64   `json:"rows"`
	SharedHit   int64   `json:"shared_blks_hit"`
	SharedRead  int64   `json:"shared_blks_read"`
	TempRead    int64   `json:"temp_blks_read"`
	TempWritten int64   `json:"temp_blks_written"`
	WalBytes    int64   `json:"wal_bytes"`
	HitPct      float64 `json:"cache_hit_pct"` // 100*hit/(hit+read); -1 when the query touched no shared blocks
	Query       string  `json:"query"`
}

// slowQuerySorts maps a validated --sort key to the pg_stat_statements expression to
// order by (always DESC — "worst first"). Mapping a whitelisted key to fixed SQL keeps
// the sort un-injectable. "io" ranks by physical reads (cache misses); "temp" by total
// temp-block traffic (on-disk sorts/hashes). "max" surfaces the worst single-execution
// latency spike; "stddev" the most inconsistent (usually-fast, occasionally-terrible) queries.
var slowQuerySorts = map[string]string{
	"total":  "total_exec_time",
	"mean":   "mean_exec_time",
	"max":    "max_exec_time",
	"stddev": "stddev_exec_time",
	"calls":  "calls",
	"rows":   "rows",
	"io":     "shared_blks_read",
	"temp":   "(temp_blks_read + temp_blks_written)",
}

// SlowQuerySortKeys lists the valid --sort values (for the usage error message).
var SlowQuerySortKeys = []string{"total", "mean", "max", "stddev", "calls", "rows", "io", "temp"}

// PgStatStatementsAvailable reports whether the pg_stat_statements view exists in
// the current database (the extension is loaded + created). When false, callers
// degrade gracefully with enablement guidance instead of erroring (D3).
func PgStatStatementsAvailable(ctx context.Context, q Querier) (bool, error) {
	var ok bool
	rows, err := q.Query(ctx, "SELECT to_regclass('pg_stat_statements') IS NOT NULL")
	if err != nil {
		return false, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&ok); err != nil {
			return false, err
		}
	}
	return ok, rows.Err()
}

// buildSlowQueriesQuery returns the top-queries SQL ordered by the given (validated)
// sort key. total_exec_time / mean_exec_time / stddev_exec_time are the PG13+ column
// names (the supported range); the I/O-time columns renamed in PG17 are not used.
//
// pg_stat_statements is cluster-wide (every database's statements in one view), so with
// currentDBOnly it filters by dbid to the connected database — otherwise which database
// you connect to (or pass via -d) would never change the list.
func buildSlowQueriesQuery(sortKey string, currentDBOnly bool) string {
	orderBy, ok := slowQuerySorts[sortKey]
	if !ok {
		orderBy = slowQuerySorts["total"]
	}
	where := ""
	if currentDBOnly {
		where = "\nWHERE dbid = (SELECT oid FROM pg_catalog.pg_database WHERE datname = current_database())"
	}
	return `SELECT calls, total_exec_time, mean_exec_time, stddev_exec_time,
       min_exec_time, max_exec_time, rows,
       shared_blks_hit, shared_blks_read, temp_blks_read, temp_blks_written, wal_bytes,
       query
FROM pg_stat_statements` + where + `
ORDER BY ` + orderBy + ` DESC
LIMIT $1`
}

// ValidSlowQuerySort reports whether key is an accepted --sort value.
func ValidSlowQuerySort(key string) bool {
	_, ok := slowQuerySorts[key]
	return ok
}

// ListSlowQueries returns the top queries ordered by the given sort key (one of
// SlowQuerySortKeys; defaults to "total" if unknown). With currentDBOnly it reports only
// statements executed in the connected database; otherwise the whole cluster.
func ListSlowQueries(ctx context.Context, q Querier, limit int, sortKey string, currentDBOnly bool) ([]SlowQuery, error) {
	rows, err := q.Query(ctx, buildSlowQueriesQuery(sortKey, currentDBOnly), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SlowQuery
	for rows.Next() {
		var s SlowQuery
		if err := rows.Scan(&s.Calls, &s.TotalMs, &s.MeanMs, &s.StddevMs,
			&s.MinMs, &s.MaxMs, &s.Rows,
			&s.SharedHit, &s.SharedRead, &s.TempRead, &s.TempWritten, &s.WalBytes,
			&s.Query); err != nil {
			return nil, err
		}
		if touched := s.SharedHit + s.SharedRead; touched > 0 {
			s.HitPct = 100 * float64(s.SharedHit) / float64(touched)
		} else {
			s.HitPct = -1 // never read a shared block (e.g. a pure-compute query)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ResetStatStatements discards all accumulated pg_stat_statements counters
// (pg_stat_statements_reset()). It needs pg_read_all_stats / superuser. This resets
// statistics only — it never touches table data.
func ResetStatStatements(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, "SELECT pg_stat_statements_reset()")
	if err != nil {
		return err
	}
	rows.Close()
	return rows.Err()
}

// ---- Snapshots (cumulative counters captured for later diffing) ----

// StmtStatRow is one pg_stat_statements row captured in a snapshot, keyed by queryid so
// two snapshots can be matched and subtracted. The counters are cumulative since the
// last reset — only the delta between two snapshots is meaningful.
type StmtStatRow struct {
	QueryID     *int64  `json:"queryid"` // nil if the server isn't computing query ids
	Query       string  `json:"query"`
	Calls       int64   `json:"calls"`
	TotalMs     float64 `json:"total_ms"`
	Rows        int64   `json:"rows"`
	SharedRead  int64   `json:"shared_blks_read"`
	TempWritten int64   `json:"temp_blks_written"`
	WalBytes    int64   `json:"wal_bytes"`
}

// SnapshotStatements captures the full pg_stat_statements view for diffing. Caller must
// have verified the extension is present (PgStatStatementsAvailable).
func SnapshotStatements(ctx context.Context, q Querier) ([]StmtStatRow, error) {
	const sql = `SELECT queryid, query, calls, total_exec_time, rows,
       shared_blks_read, temp_blks_written, wal_bytes
FROM pg_stat_statements`
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StmtStatRow
	for rows.Next() {
		var r StmtStatRow
		if err := rows.Scan(&r.QueryID, &r.Query, &r.Calls, &r.TotalMs, &r.Rows,
			&r.SharedRead, &r.TempWritten, &r.WalBytes); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// TableStatRow is one pg_stat_user_tables row captured in a snapshot, keyed by
// schema.name. Like statement counters, these are cumulative since the last reset.
type TableStatRow struct {
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	SeqScan int64  `json:"seq_scan"`
	IdxScan int64  `json:"idx_scan"`
	Ins     int64  `json:"inserts"`
	Upd     int64  `json:"updates"`
	Del     int64  `json:"deletes"`
	LiveTup int64  `json:"live_tuples"`
	DeadTup int64  `json:"dead_tuples"`
}

// SnapshotTables captures per-table read/write counters for diffing.
func SnapshotTables(ctx context.Context, q Querier) ([]TableStatRow, error) {
	const sql = `SELECT schemaname, relname,
       COALESCE(seq_scan, 0), COALESCE(idx_scan, 0),
       COALESCE(n_tup_ins, 0), COALESCE(n_tup_upd, 0), COALESCE(n_tup_del, 0),
       COALESCE(n_live_tup, 0), COALESCE(n_dead_tup, 0)
FROM pg_catalog.pg_stat_user_tables`
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableStatRow
	for rows.Next() {
		var r TableStatRow
		if err := rows.Scan(&r.Schema, &r.Name, &r.SeqScan, &r.IdxScan,
			&r.Ins, &r.Upd, &r.Del, &r.LiveTup, &r.DeadTup); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// LockWait is one waiting (ungranted) lock request — the actionable lock info.
type LockWait struct {
	PID       int32  `json:"pid"`
	User      string `json:"user"`
	LockType  string `json:"lock_type"`
	Mode      string `json:"mode"`
	Object    string `json:"object"` // relation name, or the lock type for non-relation locks
	BlockedBy string `json:"blocked_by"`
	Query     string `json:"query"`
}

func buildLockWaitsQuery() string {
	// Only ungranted locks = sessions actually waiting. pg_blocking_pids names the
	// holders; the relation (if any) is resolved via pg_class.
	return `SELECT l.pid,
       COALESCE(a.usename, ''),
       l.locktype,
       COALESCE(l.mode, ''),
       COALESCE(c.relname, l.locktype),
       array_to_string(pg_catalog.pg_blocking_pids(l.pid), ','),
       COALESCE(a.query, '')
FROM pg_catalog.pg_locks l
JOIN pg_catalog.pg_stat_activity a ON a.pid = l.pid
LEFT JOIN pg_catalog.pg_class c ON c.oid = l.relation
WHERE NOT l.granted
ORDER BY l.pid`
}

// ListLockWaits returns the sessions currently waiting on a lock and who blocks them.
func ListLockWaits(ctx context.Context, q Querier) ([]LockWait, error) {
	rows, err := q.Query(ctx, buildLockWaitsQuery())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LockWait
	for rows.Next() {
		var w LockWait
		if err := rows.Scan(&w.PID, &w.User, &w.LockType, &w.Mode, &w.Object, &w.BlockedBy, &w.Query); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// Column is one column of a table (for describe).
type Column struct {
	Name     string `json:"name"`
	Type     string `json:"type"`
	Nullable bool   `json:"nullable"`
	Default  string `json:"default,omitempty"`
}

// Constraint is one constraint on a table (for describe).
type Constraint struct {
	Name       string `json:"name"`
	Type       string `json:"type"` // primary key | foreign key | unique | check | exclusion
	Definition string `json:"definition"`
}

// Partition is one child partition of a partitioned table.
type Partition struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Bound  string `json:"bound"` // pg_get_expr(relpartbound): "FOR VALUES FROM (...) TO (...)"
}

// PartitionInfo describes a table's role in partitioning, if any.
type PartitionInfo struct {
	IsPartitioned bool        `json:"is_partitioned"`         // this table is a partitioned parent
	Strategy      string      `json:"strategy,omitempty"`     // range | list | hash
	Key           string      `json:"key,omitempty"`          // pg_get_partkeydef
	Partitions    []Partition `json:"partitions,omitempty"`   // children (when partitioned)
	ParentTable   string      `json:"parent_table,omitempty"` // set if this table IS a partition
	Bound         string      `json:"bound,omitempty"`        // this table's own bound (when a partition)
}

// TableHealth is the maintenance / bloat picture for a table.
type TableHealth struct {
	LiveTuples      int64   `json:"live_tuples"`
	DeadTuples      int64   `json:"dead_tuples"`
	DeadRatio       float64 `json:"dead_ratio"` // 0..1; high => bloat, needs VACUUM
	LastVacuum      string  `json:"last_vacuum,omitempty"`
	LastAutovacuum  string  `json:"last_autovacuum,omitempty"`
	LastAnalyze     string  `json:"last_analyze,omitempty"`
	LastAutoanalyze string  `json:"last_autoanalyze,omitempty"`
}

// Reference is an incoming foreign key: another table whose FK points at this one.
// Knowing "what references this table" is the question to answer before dropping or
// truncating it.
type Reference struct {
	Schema     string `json:"schema"`     // schema of the referencing table
	Table      string `json:"table"`      // the referencing table
	Constraint string `json:"constraint"` // FK constraint name
	Definition string `json:"definition"` // pg_get_constraintdef (the FK columns)
}

// ColumnStat is one column's planner statistics from pg_stats — what the planner uses
// to estimate row counts. A wrong n_distinct or stale stats are a leading cause of the
// row-estimate blowups `explain` flags, so this closes that loop.
type ColumnStat struct {
	Column      string  `json:"column"`
	NullFrac    float64 `json:"null_frac"`                  // fraction of rows where the column is NULL
	NDistinct   float64 `json:"n_distinct"`                 // distinct values; negative = a multiple of row count
	AvgWidth    int     `json:"avg_width"`                  // average stored width in bytes
	Correlation float64 `json:"correlation"`                // -1..1; closeness of physical to logical order (index scan cost)
	MCV         string  `json:"most_common_vals,omitempty"` // most_common_vals, truncated for display
}

// TableDetail is the full `pgdx describe table` view.
type TableDetail struct {
	Schema       string         `json:"schema"`
	Name         string         `json:"name"`
	Owner        string         `json:"owner"`
	Size         string         `json:"size"`
	EstRows      int64          `json:"est_rows"`
	Columns      []Column       `json:"columns"`
	Indexes      []Index        `json:"indexes"`
	Constraints  []Constraint   `json:"constraints"`
	ReferencedBy []Reference    `json:"referenced_by,omitempty"` // incoming FKs (tables pointing here)
	Partition    *PartitionInfo `json:"partitioning,omitempty"`  // nil if not involved in partitioning
	Health       *TableHealth   `json:"health,omitempty"`        // nil if no stats (e.g. partitioned parent)
	ColumnStats  []ColumnStat   `json:"column_stats,omitempty"`  // only populated with --stats
}

// SplitQualified splits "schema.name" into its parts. "name" alone yields an empty
// schema (meaning: resolve across schemas).
func SplitQualified(s string) (schema, name string) {
	if i := strings.IndexByte(s, '.'); i >= 0 {
		return s[:i], s[i+1:]
	}
	return "", s
}

func buildResolveQuery(schema, name string) (string, []any) {
	q := `SELECT n.nspname, c.oid,
       pg_catalog.pg_get_userbyid(c.relowner),
       c.reltuples::bigint,
       pg_catalog.pg_size_pretty(pg_catalog.pg_total_relation_size(c.oid))
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND c.relname = $1
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'`
	args := []any{name}
	if schema != "" {
		args = append(args, schema)
		q += fmt.Sprintf("\n  AND n.nspname = $%d", len(args))
	}
	q += "\nORDER BY n.nspname"
	return q, args
}

func buildColumnsQuery() string {
	return `SELECT a.attname,
       pg_catalog.format_type(a.atttypid, a.atttypmod),
       a.attnotnull,
       COALESCE(pg_catalog.pg_get_expr(d.adbin, d.adrelid), '')
FROM pg_catalog.pg_attribute a
LEFT JOIN pg_catalog.pg_attrdef d ON d.adrelid = a.attrelid AND d.adnum = a.attnum
WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped
ORDER BY a.attnum`
}

func buildConstraintsQuery() string {
	return `SELECT conname, contype::text, pg_catalog.pg_get_constraintdef(oid, true)
FROM pg_catalog.pg_constraint
WHERE conrelid = $1
ORDER BY contype, conname`
}

func constraintType(t string) string {
	switch t {
	case "p":
		return "primary key"
	case "f":
		return "foreign key"
	case "u":
		return "unique"
	case "c":
		return "check"
	case "x":
		return "exclusion"
	default:
		return t
	}
}

// AmbiguousTableError means a bare table name matched more than one schema.
type AmbiguousTableError struct {
	Name    string
	Schemas []string
}

func (e *AmbiguousTableError) Error() string {
	return fmt.Sprintf("table %q exists in multiple schemas (%s); qualify it as schema.name",
		e.Name, strings.Join(e.Schemas, ", "))
}

// DescribeTable resolves a table (qualified "schema.name" or bare "name") and
// returns its columns, indexes, and constraints. A bare name that matches multiple
// schemas returns *AmbiguousTableError.
// ResolvedTable identifies a single table after resolving a (possibly bare) name.
type ResolvedTable struct {
	Schema  string
	Name    string
	OID     uint32
	Owner   string
	Size    string
	EstRows int64
}

// ResolveTable resolves a "schema.name" or bare "name" to exactly one table. A bare
// name matching multiple schemas returns *AmbiguousTableError; no match is an error.
// Shared by describe table and vacuum so they agree on schema resolution.
func ResolveTable(ctx context.Context, q Querier, qualified string) (*ResolvedTable, error) {
	schema, name := SplitQualified(qualified)
	rq, rargs := buildResolveQuery(schema, name)
	rows, err := q.Query(ctx, rq, rargs...)
	if err != nil {
		return nil, err
	}
	var matches []ResolvedTable
	for rows.Next() {
		m := ResolvedTable{Name: name}
		if err := rows.Scan(&m.Schema, &m.OID, &m.Owner, &m.EstRows, &m.Size); err != nil {
			rows.Close()
			return nil, err
		}
		matches = append(matches, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch {
	case len(matches) == 0:
		return nil, fmt.Errorf("table %q not found", qualified)
	case len(matches) > 1:
		schemas := make([]string, len(matches))
		for i, m := range matches {
			schemas[i] = m.Schema
		}
		return nil, &AmbiguousTableError{Name: name, Schemas: schemas}
	}
	return &matches[0], nil
}

func DescribeTable(ctx context.Context, q Querier, qualified string) (*TableDetail, error) {
	rt, err := ResolveTable(ctx, q, qualified)
	if err != nil {
		return nil, err
	}
	detail := &TableDetail{
		Schema: rt.Schema, Name: rt.Name, Owner: rt.Owner, Size: rt.Size, EstRows: rt.EstRows,
	}

	// Columns.
	crows, err := q.Query(ctx, buildColumnsQuery(), rt.OID)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var c Column
		var notnull bool
		if err := crows.Scan(&c.Name, &c.Type, &notnull, &c.Default); err != nil {
			crows.Close()
			return nil, err
		}
		c.Nullable = !notnull
		detail.Columns = append(detail.Columns, c)
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return nil, err
	}

	// Constraints.
	conrows, err := q.Query(ctx, buildConstraintsQuery(), rt.OID)
	if err != nil {
		return nil, err
	}
	for conrows.Next() {
		var con Constraint
		var ctype string
		if err := conrows.Scan(&con.Name, &ctype, &con.Definition); err != nil {
			conrows.Close()
			return nil, err
		}
		con.Type = constraintType(ctype)
		detail.Constraints = append(detail.Constraints, con)
	}
	conrows.Close()
	if err := conrows.Err(); err != nil {
		return nil, err
	}

	// Indexes (reuse the list query, scoped to this table).
	idx, err := ListIndexes(ctx, q, rt.Schema, rt.Name, false, "name")
	if err != nil {
		return nil, err
	}
	detail.Indexes = idx

	// Incoming foreign keys (what references this table) — the "safe to drop?" question.
	refs, err := gatherReferencedBy(ctx, q, rt.OID)
	if err != nil {
		return nil, err
	}
	detail.ReferencedBy = refs

	// Partitioning (parent and/or child).
	pi, err := gatherPartitionInfo(ctx, q, rt.OID)
	if err != nil {
		return nil, err
	}
	detail.Partition = pi

	// Maintenance / bloat health.
	h, err := gatherTableHealth(ctx, q, rt.OID)
	if err != nil {
		return nil, err
	}
	detail.Health = h

	return detail, nil
}

const tableHealthQuery = `SELECT COALESCE(n_live_tup, 0), COALESCE(n_dead_tup, 0),
       COALESCE(last_vacuum::text, ''), COALESCE(last_autovacuum::text, ''),
       COALESCE(last_analyze::text, ''), COALESCE(last_autoanalyze::text, '')
FROM pg_catalog.pg_stat_all_tables
WHERE relid = $1`

// gatherTableHealth returns vacuum/analyze timing and live/dead tuple counts, or nil
// when the table has no stats row (e.g. a partitioned parent with no storage).
func gatherTableHealth(ctx context.Context, q Querier, oid uint32) (*TableHealth, error) {
	rows, err := q.Query(ctx, tableHealthQuery, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, rows.Err()
	}
	var h TableHealth
	if err := rows.Scan(&h.LiveTuples, &h.DeadTuples,
		&h.LastVacuum, &h.LastAutovacuum, &h.LastAnalyze, &h.LastAutoanalyze); err != nil {
		return nil, err
	}
	if total := h.LiveTuples + h.DeadTuples; total > 0 {
		h.DeadRatio = float64(h.DeadTuples) / float64(total)
	}
	return &h, rows.Err()
}

const referencedByQuery = `SELECT n.nspname, c.relname, con.conname,
       pg_catalog.pg_get_constraintdef(con.oid, true)
FROM pg_catalog.pg_constraint con
JOIN pg_catalog.pg_class c ON c.oid = con.conrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE con.confrelid = $1 AND con.contype = 'f'
ORDER BY n.nspname, c.relname, con.conname`

// gatherReferencedBy returns the foreign keys that point AT this table (conrelid is the
// referencing table, confrelid is the one being referenced).
func gatherReferencedBy(ctx context.Context, q Querier, oid uint32) ([]Reference, error) {
	rows, err := q.Query(ctx, referencedByQuery, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Reference
	for rows.Next() {
		var r Reference
		if err := rows.Scan(&r.Schema, &r.Table, &r.Constraint, &r.Definition); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const columnStatsQuery = `SELECT s.attname, s.null_frac, s.n_distinct, s.avg_width,
       COALESCE(s.correlation, 0),
       COALESCE(left(array_to_string(s.most_common_vals, ', '), 60), '')
FROM pg_catalog.pg_stats s
WHERE s.schemaname = $1 AND s.tablename = $2
ORDER BY (SELECT a.attnum FROM pg_catalog.pg_attribute a
          JOIN pg_catalog.pg_class c ON c.oid = a.attrelid
          JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
          WHERE n.nspname = s.schemaname AND c.relname = s.tablename AND a.attname = s.attname)`

// GatherColumnStats returns per-column planner statistics for a table from pg_stats.
// pg_stats only exposes rows the caller is allowed to see (owner / pg_read_all_stats /
// superuser), so a non-privileged user may get fewer columns back — that's expected, not
// an error. An empty result usually means the table was never ANALYZEd.
func GatherColumnStats(ctx context.Context, q Querier, schema, table string) ([]ColumnStat, error) {
	rows, err := q.Query(ctx, columnStatsQuery, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ColumnStat
	for rows.Next() {
		var c ColumnStat
		if err := rows.Scan(&c.Column, &c.NullFrac, &c.NDistinct, &c.AvgWidth, &c.Correlation, &c.MCV); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

const partStrategyQuery = `SELECT CASE p.partstrat
         WHEN 'r' THEN 'range' WHEN 'l' THEN 'list' WHEN 'h' THEN 'hash'
         ELSE p.partstrat::text END,
       pg_catalog.pg_get_partkeydef($1)
FROM pg_catalog.pg_partitioned_table p
WHERE p.partrelid = $1`

const childPartitionsQuery = `SELECT n.nspname, c.relname, COALESCE(pg_catalog.pg_get_expr(c.relpartbound, c.oid), '')
FROM pg_catalog.pg_inherits i
JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE i.inhparent = $1
ORDER BY c.relname`

const parentPartitionQuery = `SELECT pn.nspname || '.' || pc.relname,
       COALESCE(pg_catalog.pg_get_expr(c.relpartbound, c.oid), '')
FROM pg_catalog.pg_inherits i
JOIN pg_catalog.pg_class pc ON pc.oid = i.inhparent
JOIN pg_catalog.pg_namespace pn ON pn.oid = pc.relnamespace
JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
WHERE i.inhrelid = $1`

// gatherPartitionInfo returns partition details for a table, or nil if it is neither
// a partitioned table nor a partition. (A table can be both, under sub-partitioning.)
func gatherPartitionInfo(ctx context.Context, q Querier, oid uint32) (*PartitionInfo, error) {
	pi := &PartitionInfo{}
	involved := false

	// Is this a partitioned (parent) table?
	srows, err := q.Query(ctx, partStrategyQuery, oid)
	if err != nil {
		return nil, err
	}
	if srows.Next() {
		if err := srows.Scan(&pi.Strategy, &pi.Key); err != nil {
			srows.Close()
			return nil, err
		}
		pi.IsPartitioned = true
		involved = true
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, err
	}

	if pi.IsPartitioned {
		crows, err := q.Query(ctx, childPartitionsQuery, oid)
		if err != nil {
			return nil, err
		}
		for crows.Next() {
			var p Partition
			if err := crows.Scan(&p.Schema, &p.Name, &p.Bound); err != nil {
				crows.Close()
				return nil, err
			}
			pi.Partitions = append(pi.Partitions, p)
		}
		crows.Close()
		if err := crows.Err(); err != nil {
			return nil, err
		}
	}

	// Is this table itself a partition of some parent?
	prows, err := q.Query(ctx, parentPartitionQuery, oid)
	if err != nil {
		return nil, err
	}
	if prows.Next() {
		if err := prows.Scan(&pi.ParentTable, &pi.Bound); err != nil {
			prows.Close()
			return nil, err
		}
		involved = true
	}
	prows.Close()
	if err := prows.Err(); err != nil {
		return nil, err
	}

	if !involved {
		return nil, nil
	}
	return pi, nil
}

// IndexDetail is the full `pgdx describe index` view.
type IndexDetail struct {
	Schema        string `json:"schema"`
	Name          string `json:"name"`
	Table         string `json:"table"`
	Method        string `json:"method"` // btree, gin, gist, hash, ...
	Unique        bool   `json:"unique"`
	Primary       bool   `json:"primary"`
	Valid         bool   `json:"valid"` // false => incomplete/failed build; the index is not used
	Size          string `json:"size"`
	Definition    string `json:"definition"`
	Scans         int64  `json:"scans"`
	TuplesRead    int64  `json:"tuples_read"`
	TuplesFetched int64  `json:"tuples_fetched"`
}

func buildIndexResolveQuery(schema, name string) (string, []any) {
	q := `SELECT n.nspname, i.relname, t.relname, am.amname,
       ix.indisunique, ix.indisprimary, ix.indisvalid,
       pg_catalog.pg_size_pretty(pg_catalog.pg_relation_size(i.oid)),
       pg_catalog.pg_get_indexdef(i.oid),
       COALESCE(s.idx_scan, 0), COALESCE(s.idx_tup_read, 0), COALESCE(s.idx_tup_fetch, 0)
FROM pg_catalog.pg_class i
JOIN pg_catalog.pg_index ix ON ix.indexrelid = i.oid
JOIN pg_catalog.pg_class t ON t.oid = ix.indrelid
JOIN pg_catalog.pg_namespace n ON n.oid = i.relnamespace
JOIN pg_catalog.pg_am am ON am.oid = i.relam
LEFT JOIN pg_catalog.pg_stat_user_indexes s ON s.indexrelid = i.oid
WHERE i.relkind IN ('i', 'I')
  AND i.relname = $1
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'`
	args := []any{name}
	if schema != "" {
		args = append(args, schema)
		q += fmt.Sprintf("\n  AND n.nspname = $%d", len(args))
	}
	q += "\nORDER BY n.nspname"
	return q, args
}

// DescribeIndex resolves an index (qualified "schema.name" or bare "name") and
// returns its metadata. A bare name matching multiple schemas is an error.
func DescribeIndex(ctx context.Context, q Querier, qualified string) (*IndexDetail, error) {
	schema, name := SplitQualified(qualified)
	sql, args := buildIndexResolveQuery(schema, name)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	var matches []IndexDetail
	for rows.Next() {
		var d IndexDetail
		if err := rows.Scan(&d.Schema, &d.Name, &d.Table, &d.Method,
			&d.Unique, &d.Primary, &d.Valid, &d.Size, &d.Definition,
			&d.Scans, &d.TuplesRead, &d.TuplesFetched); err != nil {
			rows.Close()
			return nil, err
		}
		matches = append(matches, d)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch {
	case len(matches) == 0:
		return nil, fmt.Errorf("index %q not found", qualified)
	case len(matches) > 1:
		schemas := make([]string, len(matches))
		for i, m := range matches {
			schemas[i] = m.Schema
		}
		return nil, fmt.Errorf("index %q exists in multiple schemas (%s); qualify it as schema.name",
			name, strings.Join(schemas, ", "))
	}
	return &matches[0], nil
}

// ---- Settings (pg_settings) ----

type Setting struct {
	Name        string `json:"name"`
	Value       string `json:"value"`
	Unit        string `json:"unit,omitempty"`
	Source      string `json:"source"` // default | configuration file | ...
	Description string `json:"description"`
}

// CuratedSettings is the operationally interesting subset shown by `get settings` with
// no arguments (the ones that actually affect performance/capacity).
var CuratedSettings = []string{
	"max_connections", "shared_buffers", "effective_cache_size",
	"work_mem", "maintenance_work_mem",
	"random_page_cost", "effective_io_concurrency",
	"default_statistics_target", "max_parallel_workers_per_gather", "max_worker_processes",
	"wal_level", "max_wal_size", "min_wal_size", "checkpoint_timeout",
	"autovacuum", "autovacuum_vacuum_scale_factor", "autovacuum_analyze_scale_factor",
}

// ListSettings returns server settings. With all=true, every setting; otherwise if
// patterns are given, names matching any (case-insensitive substring); otherwise the
// curated set.
func ListSettings(ctx context.Context, q Querier, patterns []string, all bool) ([]Setting, error) {
	q1 := `SELECT name, setting, COALESCE(unit, ''), source, short_desc FROM pg_catalog.pg_settings`
	var args []any
	switch {
	case all:
		// no filter
	case len(patterns) > 0:
		like := make([]string, len(patterns))
		for i, p := range patterns {
			like[i] = "%" + p + "%"
		}
		args = append(args, like)
		q1 += " WHERE name ILIKE ANY($1)"
	default:
		args = append(args, CuratedSettings)
		q1 += " WHERE name = ANY($1)"
	}
	q1 += " ORDER BY name"

	rows, err := q.Query(ctx, q1, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Setting
	for rows.Next() {
		var s Setting
		if err := rows.Scan(&s.Name, &s.Value, &s.Unit, &s.Source, &s.Description); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- Connections ----

// ConnStat is a grouped connection count (default summary).
type ConnStat struct {
	Database    string `json:"database"`
	User        string `json:"user"`
	Application string `json:"application"`
	State       string `json:"state"`
	Count       int64  `json:"count"`
}

// ConnDetail is one backend (--detail mode).
type ConnDetail struct {
	PID         int32   `json:"pid"`
	User        string  `json:"user"`
	Database    string  `json:"database"`
	Application string  `json:"application"`
	ClientAddr  string  `json:"client_addr"`
	State       string  `json:"state"`
	AgeSec      float64 `json:"age_sec"`  // since backend_start
	IdleSec     float64 `json:"idle_sec"` // since state_change (time in current state)
}

// IdleInTx is an idle-in-transaction backend — it holds locks/snapshots and blocks
// vacuum, so these are surfaced specifically.
type IdleInTx struct {
	PID     int32   `json:"pid"`
	User    string  `json:"user"`
	IdleSec float64 `json:"idle_sec"`
	Query   string  `json:"query"`
}

// ConnFilter narrows the connection listing. Empty fields are ignored.
type ConnFilter struct {
	User  string // exact match on usename
	State string // exact match on state (e.g. "active", "idle in transaction")
	App   string // substring (ILIKE) match on application_name
}

// clause appends this filter's conditions to a query, growing args, and returns the SQL
// fragment (e.g. " AND usename = $2"). Parameterized — no injection.
func (f ConnFilter) clause(args *[]any) string {
	var c string
	if f.User != "" {
		*args = append(*args, f.User)
		c += fmt.Sprintf(" AND usename = $%d", len(*args))
	}
	if f.State != "" {
		*args = append(*args, f.State)
		c += fmt.Sprintf(" AND state = $%d", len(*args))
	}
	if f.App != "" {
		*args = append(*args, "%"+f.App+"%")
		c += fmt.Sprintf(" AND application_name ILIKE $%d", len(*args))
	}
	return c
}

// ConnUsage returns the client-backend count and the max_connections limit. We count
// only client backends because max_connections limits those (autovacuum/background
// workers have separate, reserved slots) — counting everything overstates usage.
// Note: usage is the GLOBAL capacity figure — filters (ConnFilter) do not apply here.
func ConnUsage(ctx context.Context, q Querier) (used, max int64, err error) {
	rows, err := q.Query(ctx, "SELECT (SELECT count(*) FROM pg_catalog.pg_stat_activity WHERE backend_type = 'client backend'), current_setting('max_connections')::bigint")
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&used, &max); err != nil {
			return 0, 0, err
		}
	}
	return used, max, rows.Err()
}

// ListConnections groups current backends by database, user, application, and state,
// optionally narrowed by filter.
func ListConnections(ctx context.Context, q Querier, filter ConnFilter) ([]ConnStat, error) {
	var args []any
	sql := `SELECT COALESCE(datname, '[no db]'),
       COALESCE(usename, '[none]'),
       COALESCE(NULLIF(application_name, ''), '[none]'),
       COALESCE(state, '[none]'),
       count(*)
FROM pg_catalog.pg_stat_activity
WHERE backend_type = 'client backend'` + filter.clause(&args) + `
GROUP BY 1, 2, 3, 4
ORDER BY count(*) DESC, 1`
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnStat
	for rows.Next() {
		var c ConnStat
		if err := rows.Scan(&c.Database, &c.User, &c.Application, &c.State, &c.Count); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListConnectionDetail returns one row per backend, optionally narrowed by filter.
func ListConnectionDetail(ctx context.Context, q Querier, filter ConnFilter) ([]ConnDetail, error) {
	var args []any
	sql := `SELECT pid,
       COALESCE(usename, ''),
       COALESCE(datname, ''),
       COALESCE(NULLIF(application_name, ''), ''),
       COALESCE(host(client_addr), 'local'),
       COALESCE(state, ''),
       COALESCE(EXTRACT(epoch FROM (now() - backend_start)), -1)::float8,
       COALESCE(EXTRACT(epoch FROM (now() - state_change)), -1)::float8
FROM pg_catalog.pg_stat_activity
WHERE backend_type = 'client backend'
  AND pid <> pg_catalog.pg_backend_pid()` + filter.clause(&args) + `
ORDER BY state, now() - backend_start DESC`
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ConnDetail
	for rows.Next() {
		var c ConnDetail
		if err := rows.Scan(&c.PID, &c.User, &c.Database, &c.Application, &c.ClientAddr,
			&c.State, &c.AgeSec, &c.IdleSec); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// TopClientAddr returns the client address holding the most client-backend connections,
// its count, and the total client-backend count. It's the signal behind the "you may be
// behind a connection pooler" note: when one address (the pooler host) owns the large
// majority of backends, the connections pgdx sees are pooled server connections, not end
// clients. "local" (unix-socket) is reported like any other address.
func TopClientAddr(ctx context.Context, q Querier) (addr string, count, total int64, err error) {
	const sql = `SELECT COALESCE(host(client_addr), 'local') AS addr, count(*)
FROM pg_catalog.pg_stat_activity
WHERE backend_type = 'client backend'
GROUP BY 1
ORDER BY count(*) DESC`
	rows, qerr := q.Query(ctx, sql)
	if qerr != nil {
		return "", 0, 0, qerr
	}
	defer rows.Close()
	first := true
	for rows.Next() {
		var a string
		var n int64
		if err := rows.Scan(&a, &n); err != nil {
			return "", 0, 0, err
		}
		if first {
			addr, count = a, n
			first = false
		}
		total += n
	}
	return addr, count, total, rows.Err()
}

// ListIdleInTx returns idle-in-transaction backends, longest-idle first.
func ListIdleInTx(ctx context.Context, q Querier) ([]IdleInTx, error) {
	const sql = `SELECT pid, COALESCE(usename, ''),
       COALESCE(EXTRACT(epoch FROM (now() - state_change)), -1)::float8,
       COALESCE(query, '')
FROM pg_catalog.pg_stat_activity
WHERE state LIKE 'idle in transaction%'
ORDER BY state_change ASC`
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []IdleInTx
	for rows.Next() {
		var x IdleInTx
		if err := rows.Scan(&x.PID, &x.User, &x.IdleSec, &x.Query); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// ---- Progress (pg_stat_progress_*) ----

type Progress struct {
	PID       int32   `json:"pid"`
	Operation string  `json:"operation"` // vacuum | create index | analyze | cluster
	Table     string  `json:"table"`
	Phase     string  `json:"phase"`
	Percent   float64 `json:"percent"`
}

func buildProgressQuery() string {
	// Union the progress views into one shape. Each computes % from its own
	// done/total columns; all cast to float8 for a clean scan. All four views exist
	// in the supported PG13-17 range.
	return `SELECT v.pid, 'vacuum', COALESCE(c.relname, ''), v.phase,
       (CASE WHEN v.heap_blks_total > 0 THEN 100.0 * v.heap_blks_scanned / v.heap_blks_total ELSE 0 END)::float8
FROM pg_catalog.pg_stat_progress_vacuum v LEFT JOIN pg_catalog.pg_class c ON c.oid = v.relid
UNION ALL
SELECT ci.pid, 'create index', COALESCE(c.relname, ''), ci.phase,
       (CASE WHEN ci.blocks_total > 0 THEN 100.0 * ci.blocks_done / ci.blocks_total
             WHEN ci.tuples_total > 0 THEN 100.0 * ci.tuples_done / ci.tuples_total ELSE 0 END)::float8
FROM pg_catalog.pg_stat_progress_create_index ci LEFT JOIN pg_catalog.pg_class c ON c.oid = ci.relid
UNION ALL
SELECT a.pid, 'analyze', COALESCE(c.relname, ''), a.phase,
       (CASE WHEN a.sample_blks_total > 0 THEN 100.0 * a.sample_blks_scanned / a.sample_blks_total ELSE 0 END)::float8
FROM pg_catalog.pg_stat_progress_analyze a LEFT JOIN pg_catalog.pg_class c ON c.oid = a.relid
UNION ALL
SELECT cl.pid, 'cluster', COALESCE(c.relname, ''), cl.phase,
       (CASE WHEN cl.heap_blks_total > 0 THEN 100.0 * cl.heap_blks_scanned / cl.heap_blks_total ELSE 0 END)::float8
FROM pg_catalog.pg_stat_progress_cluster cl LEFT JOIN pg_catalog.pg_class c ON c.oid = cl.relid
ORDER BY 1`
}

// ListProgress returns in-flight maintenance operations (vacuum/create index/analyze/cluster).
func ListProgress(ctx context.Context, q Querier) ([]Progress, error) {
	rows, err := q.Query(ctx, buildProgressQuery())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Progress
	for rows.Next() {
		var p Progress
		if err := rows.Scan(&p.PID, &p.Operation, &p.Table, &p.Phase, &p.Percent); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ---- Replication (pg_stat_replication) ----

type Replication struct {
	PID          int32   `json:"pid"`
	Application  string  `json:"application"`
	ClientAddr   string  `json:"client_addr"`
	State        string  `json:"state"`
	SyncState    string  `json:"sync_state"` // async | sync | quorum | potential
	LagBytes     int64   `json:"lag_bytes"`  // sent but not yet replayed
	ReplayLagSec float64 `json:"replay_lag_sec"`
}

// ListReplication returns connected standbys and their lag. Empty unless this server is
// a primary with downstream replicas. Uses sent_lsn (not pg_current_wal_lsn) so it never
// errors when run against a standby.
func ListReplication(ctx context.Context, q Querier) ([]Replication, error) {
	const sql = `SELECT pid,
       COALESCE(NULLIF(application_name, ''), ''),
       COALESCE(host(client_addr), 'local'),
       COALESCE(state, ''),
       COALESCE(sync_state, ''),
       COALESCE(pg_catalog.pg_wal_lsn_diff(sent_lsn, replay_lsn), 0)::bigint,
       COALESCE(EXTRACT(epoch FROM replay_lag), -1)::float8
FROM pg_catalog.pg_stat_replication
ORDER BY pid`
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Replication
	for rows.Next() {
		var r Replication
		if err := rows.Scan(&r.PID, &r.Application, &r.ClientAddr, &r.State, &r.SyncState, &r.LagBytes, &r.ReplayLagSec); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReplicationSlot is one row of `pgdx get replication --slots` (pg_replication_slots).
//
// A slot persists the WAL its consumer hasn't confirmed yet — so an INACTIVE slot
// (a replica that detached, or a logical consumer that died) silently pins WAL and
// can fill the disk. That's the incident this surfaces, distinct from standby lag.
type ReplicationSlot struct {
	Name          string `json:"slot_name"`
	Type          string `json:"slot_type"`          // physical | logical
	Database      string `json:"database"`           // logical slots only
	Plugin        string `json:"plugin,omitempty"`   // logical slots: output plugin
	Active        bool   `json:"active"`             // a consumer is currently attached
	RetainedBytes int64  `json:"retained_wal_bytes"` // WAL held back from removal (restart_lsn → now)
	WalStatus     string `json:"wal_status"`         // reserved | extended | unreserved | lost (PG13+)
}

// ListReplicationSlots returns every replication slot with how much WAL it retains.
// Retained WAL is measured from the slot's restart_lsn to the server's current WAL
// position; on a standby that's the last replayed LSN (pg_current_wal_lsn() errors in
// recovery), so the expression branches on pg_is_in_recovery() to work on either role.
func ListReplicationSlots(ctx context.Context, q Querier) ([]ReplicationSlot, error) {
	const sql = `SELECT slot_name,
       slot_type,
       COALESCE(database, ''),
       COALESCE(plugin, ''),
       COALESCE(active, false),
       COALESCE(pg_catalog.pg_wal_lsn_diff(
           CASE WHEN pg_catalog.pg_is_in_recovery()
                THEN pg_catalog.pg_last_wal_replay_lsn()
                ELSE pg_catalog.pg_current_wal_lsn() END,
           restart_lsn), 0)::bigint,
       COALESCE(wal_status, '')
FROM pg_catalog.pg_replication_slots
ORDER BY active, slot_name`
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReplicationSlot
	for rows.Next() {
		var s ReplicationSlot
		if err := rows.Scan(&s.Name, &s.Type, &s.Database, &s.Plugin, &s.Active, &s.RetainedBytes, &s.WalStatus); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- Checkpoint activity (status: forced vs scheduled checkpoints) ----

// CheckpointStats counts scheduled (timed) vs requested (forced) checkpoints since the
// stats were last reset. A high requested share means checkpoints are being forced by
// WAL volume before checkpoint_timeout — usually a sign max_wal_size is too small.
type CheckpointStats struct {
	Timed     int64 `json:"timed"`     // num_timed: hit checkpoint_timeout
	Requested int64 `json:"requested"` // num_requested: forced (e.g. max_wal_size reached)
}

// CheckpointActivity reads checkpoint counters. PostgreSQL 17 moved them from
// pg_stat_bgwriter (checkpoints_timed/checkpoints_req) to pg_stat_checkpointer
// (num_timed/num_requested); we pick the right source per server so the same call works
// across PG13–17.
func CheckpointActivity(ctx context.Context, q Querier) (CheckpointStats, error) {
	sql := `SELECT checkpoints_timed, checkpoints_req FROM pg_catalog.pg_stat_bgwriter`
	var has bool
	if r, err := q.Query(ctx, "SELECT to_regclass('pg_catalog.pg_stat_checkpointer') IS NOT NULL"); err == nil {
		if r.Next() {
			_ = r.Scan(&has)
		}
		r.Close()
	}
	if has { // PG17+
		sql = `SELECT num_timed, num_requested FROM pg_catalog.pg_stat_checkpointer`
	}
	var cs CheckpointStats
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return cs, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&cs.Timed, &cs.Requested); err != nil {
			return cs, err
		}
	}
	return cs, rows.Err()
}

// ---- Backend signaling (cancel / kill) ----

// Backend is a session's identity, shown before cancelling/terminating it.
type Backend struct {
	User     string
	Database string
	State    string
	Query    string
}

// CurrentBackendPID returns pgdx's own backend PID (so we can refuse to signal ourselves).
func CurrentBackendPID(ctx context.Context, q Querier) (int32, error) {
	var pid int32
	rows, err := q.Query(ctx, "SELECT pg_catalog.pg_backend_pid()")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&pid); err != nil {
			return 0, err
		}
	}
	return pid, rows.Err()
}

// BackendInfo looks up a backend by PID. found is false if no such session exists.
func BackendInfo(ctx context.Context, q Querier, pid int32) (*Backend, bool, error) {
	rows, err := q.Query(ctx, `SELECT COALESCE(usename, ''), COALESCE(datname, ''),
       COALESCE(state, ''), COALESCE(query, '')
FROM pg_catalog.pg_stat_activity WHERE pid = $1`, pid)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return nil, false, rows.Err()
	}
	var b Backend
	if err := rows.Scan(&b.User, &b.Database, &b.State, &b.Query); err != nil {
		return nil, false, err
	}
	return &b, true, rows.Err()
}

// CancelBackend cancels the running query on a backend (pg_cancel_backend) — gentle, the
// session survives. Returns whether the signal was delivered.
func CancelBackend(ctx context.Context, q Querier, pid int32) (bool, error) {
	return signalBackend(ctx, q, "SELECT pg_catalog.pg_cancel_backend($1)", pid)
}

// TerminateBackend terminates a backend (pg_terminate_backend) — drops the connection
// and rolls back its transaction. Returns whether the signal was delivered.
func TerminateBackend(ctx context.Context, q Querier, pid int32) (bool, error) {
	return signalBackend(ctx, q, "SELECT pg_catalog.pg_terminate_backend($1)", pid)
}

func signalBackend(ctx context.Context, q Querier, sql string, pid int32) (bool, error) {
	var ok bool
	rows, err := q.Query(ctx, sql, pid)
	if err != nil {
		return false, err // includes permission errors (need superuser / pg_signal_backend)
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&ok); err != nil {
			return false, err
		}
	}
	return ok, rows.Err()
}

// ---- Roles (users) ----

type Role struct {
	Name        string   `json:"name"`
	Superuser   bool     `json:"superuser"`
	CreateDB    bool     `json:"create_db"`
	CreateRole  bool     `json:"create_role"`
	Login       bool     `json:"login"`
	Replication bool     `json:"replication"`
	ConnLimit   int64    `json:"conn_limit"` // -1 = unlimited
	ValidUntil  string   `json:"valid_until,omitempty"`
	MemberOf    []string `json:"member_of,omitempty"`
	Sessions    int64    `json:"sessions"` // CURRENT open connections by this role (not last-login;
	//                                          core Postgres does not track last login)
}

func buildRolesQuery() string {
	// pg_roles (not pg_authid) so no password hash is read. Built-in pg_* roles excluded.
	// Sessions = current backends for this role; Postgres keeps no historical last-login.
	return `SELECT r.rolname, r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolcanlogin,
       r.rolreplication, r.rolconnlimit, COALESCE(r.rolvaliduntil::text, ''),
       ARRAY(SELECT g.rolname FROM pg_catalog.pg_auth_members m
             JOIN pg_catalog.pg_roles g ON g.oid = m.roleid
             WHERE m.member = r.oid ORDER BY g.rolname),
       (SELECT count(*) FROM pg_catalog.pg_stat_activity a WHERE a.usename = r.rolname)
FROM pg_catalog.pg_roles r
WHERE r.rolname !~ '^pg_'
ORDER BY r.rolname`
}

// ListRoles lists database roles (users), excluding built-in pg_* roles.
func ListRoles(ctx context.Context, q Querier) ([]Role, error) {
	rows, err := q.Query(ctx, buildRolesQuery())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Role
	for rows.Next() {
		var r Role
		if err := rows.Scan(&r.Name, &r.Superuser, &r.CreateDB, &r.CreateRole, &r.Login,
			&r.Replication, &r.ConnLimit, &r.ValidUntil, &r.MemberOf, &r.Sessions); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ---- Databases ----

type Database struct {
	Name        string `json:"name"`
	Owner       string `json:"owner"`
	Encoding    string `json:"encoding"`
	SizeBytes   int64  `json:"size_bytes"` // -1 when the role lacks CONNECT on that database
	Connections int64  `json:"connections"`
	Commits     int64  `json:"commits"` // xact_commit since stats_reset — activity proxy
	Writes      int64  `json:"writes"`  // tup ins+upd+del since stats_reset
	// --- wide columns (always populated; surfaced in the table only with --wide) ---
	BlksHit    int64  `json:"blks_hit"`    // shared-buffer hits since stats_reset
	BlksRead   int64  `json:"blks_read"`   // disk block reads since stats_reset (HIT% = hit/(hit+read))
	Rollbacks  int64  `json:"rollbacks"`   // xact_rollback since stats_reset
	Deadlocks  int64  `json:"deadlocks"`   // deadlocks detected since stats_reset
	TempBytes  int64  `json:"temp_bytes"`  // bytes spilled to temp files (work_mem pressure)
	StatsReset string `json:"stats_reset"` // when the cumulative counters were last reset ("" if never)
}

// buildDatabasesQuery lists non-template databases. Size needs CONNECT privilege on the
// target db, so it's guarded per-row (CASE short-circuits, so pg_database_size is never
// called without privilege) — one inaccessible db won't fail the listing. sort "size"
// orders biggest-first (unsizable dbs sort last); otherwise by name.
func buildDatabasesQuery(sort string) string {
	const sizeExpr = `CASE WHEN has_database_privilege(current_user, d.datname, 'CONNECT')
            THEN pg_catalog.pg_database_size(d.oid) ELSE -1 END`
	q := `SELECT d.datname,
       pg_catalog.pg_get_userbyid(d.datdba),
       pg_catalog.pg_encoding_to_char(d.encoding),
       (` + sizeExpr + `)::bigint,
       (SELECT count(*) FROM pg_catalog.pg_stat_activity a WHERE a.datname = d.datname),
       COALESCE(s.xact_commit, 0),
       COALESCE(s.tup_inserted, 0) + COALESCE(s.tup_updated, 0) + COALESCE(s.tup_deleted, 0),
       COALESCE(s.blks_hit, 0),
       COALESCE(s.blks_read, 0),
       COALESCE(s.xact_rollback, 0),
       COALESCE(s.deadlocks, 0),
       COALESCE(s.temp_bytes, 0),
       COALESCE(s.stats_reset::text, '')
FROM pg_catalog.pg_database d
LEFT JOIN pg_catalog.pg_stat_database s ON s.datid = d.oid
WHERE NOT d.datistemplate`
	if sort == "size" {
		q += "\nORDER BY (" + sizeExpr + ") DESC, d.datname"
	} else {
		q += "\nORDER BY d.datname"
	}
	return q
}

// ListDatabases lists non-template databases in the cluster, ordered per sort.
func ListDatabases(ctx context.Context, q Querier, sort string) ([]Database, error) {
	rows, err := q.Query(ctx, buildDatabasesQuery(sort))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Database
	for rows.Next() {
		var d Database
		if err := rows.Scan(&d.Name, &d.Owner, &d.Encoding, &d.SizeBytes, &d.Connections, &d.Commits, &d.Writes,
			&d.BlksHit, &d.BlksRead, &d.Rollbacks, &d.Deadlocks, &d.TempBytes, &d.StatsReset); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// ---- Extensions ----

type Extension struct {
	Name             string `json:"name"`
	InstalledVersion string `json:"installed_version"`
	DefaultVersion   string `json:"default_version"` // if it differs, an upgrade is available
	Schema           string `json:"schema"`
	Description      string `json:"description"`
}

func buildExtensionsQuery() string {
	// pg_available_extensions carries the default (latest) version + comment, so we can
	// flag when an installed extension has an upgrade available.
	return `SELECT e.extname,
       e.extversion,
       COALESCE(ae.default_version, ''),
       n.nspname,
       COALESCE(ae.comment, '')
FROM pg_catalog.pg_extension e
JOIN pg_catalog.pg_namespace n ON n.oid = e.extnamespace
LEFT JOIN pg_catalog.pg_available_extensions ae ON ae.name = e.extname
ORDER BY e.extname`
}

// ListExtensions returns the installed extensions.
func ListExtensions(ctx context.Context, q Querier) ([]Extension, error) {
	rows, err := q.Query(ctx, buildExtensionsQuery())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Extension
	for rows.Next() {
		var e Extension
		if err := rows.Scan(&e.Name, &e.InstalledVersion, &e.DefaultVersion, &e.Schema, &e.Description); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// AvailableExtension is one row of `pgdx get extensions --available`: an extension
// present on the server's filesystem that CREATE EXTENSION can install.
type AvailableExtension struct {
	Name             string `json:"name"`
	DefaultVersion   string `json:"default_version"`
	InstalledVersion string `json:"installed_version,omitempty"` // "" when not yet installed
	Installed        bool   `json:"installed"`
	Trusted          bool   `json:"trusted"` // installable by a non-superuser with CREATE on the database (PG13+)
	Description      string `json:"description"`
}

// buildAvailableExtensionsQuery lists every extension installable on this server (from
// pg_available_extensions — i.e. what's on disk under SHAREDIR/extension), with whether
// it's already installed and whether its default version is "trusted" (a non-superuser
// can CREATE it). This answers "what CAN I install", vs `get extensions` ("what IS").
func buildAvailableExtensionsQuery() string {
	return `SELECT ae.name,
       ae.default_version,
       COALESCE(ae.installed_version, ''),
       ae.installed_version IS NOT NULL,
       COALESCE(aev.trusted, false),
       COALESCE(ae.comment, '')
FROM pg_catalog.pg_available_extensions ae
LEFT JOIN pg_catalog.pg_available_extension_versions aev
       ON aev.name = ae.name AND aev.version = ae.default_version
ORDER BY ae.name`
}

// ListAvailableExtensions returns all installable extensions on the server.
func ListAvailableExtensions(ctx context.Context, q Querier) ([]AvailableExtension, error) {
	rows, err := q.Query(ctx, buildAvailableExtensionsQuery())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AvailableExtension
	for rows.Next() {
		var a AvailableExtension
		if err := rows.Scan(&a.Name, &a.DefaultVersion, &a.InstalledVersion, &a.Installed, &a.Trusted, &a.Description); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- Views (and materialized views) ----

type View struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Type   string `json:"type"` // view | materialized
	Owner  string `json:"owner"`
	Size   string `json:"size"` // meaningful for matviews; ~0 for plain views
}

func buildViewsQuery(schema string) (string, []any) {
	q := `SELECT n.nspname, c.relname,
       CASE c.relkind WHEN 'v' THEN 'view' WHEN 'm' THEN 'materialized' ELSE c.relkind::text END,
       pg_catalog.pg_get_userbyid(c.relowner),
       pg_catalog.pg_size_pretty(pg_catalog.pg_total_relation_size(c.oid))
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('v', 'm')
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'`
	var args []any
	if schema != "" {
		args = append(args, schema)
		q += fmt.Sprintf("\n  AND n.nspname = $%d", len(args))
	}
	q += "\nORDER BY n.nspname, c.relname"
	return q, args
}

func ListViews(ctx context.Context, q Querier, schema string) ([]View, error) {
	sql, args := buildViewsQuery(schema)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []View
	for rows.Next() {
		var v View
		if err := rows.Scan(&v.Schema, &v.Name, &v.Type, &v.Owner, &v.Size); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ---- Sequences ----

type Sequence struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	DataType  string `json:"data_type"`
	Increment int64  `json:"increment"`
	LastValue *int64 `json:"last_value"` // nil if the sequence has never been used
	MaxValue  int64  `json:"max_value"`  // ceiling the sequence can reach (its MAXVALUE)
}

func buildSequencesQuery(schema string) (string, []any) {
	q := `SELECT schemaname, sequencename, data_type::text, increment_by, last_value, max_value
FROM pg_catalog.pg_sequences
WHERE schemaname !~ '^pg_'
  AND schemaname <> 'information_schema'`
	var args []any
	if schema != "" {
		args = append(args, schema)
		q += fmt.Sprintf("\n  AND schemaname = $%d", len(args))
	}
	q += "\nORDER BY schemaname, sequencename"
	return q, args
}

func ListSequences(ctx context.Context, q Querier, schema string) ([]Sequence, error) {
	sql, args := buildSequencesQuery(schema)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Sequence
	for rows.Next() {
		var s Sequence
		if err := rows.Scan(&s.Schema, &s.Name, &s.DataType, &s.Increment, &s.LastValue, &s.MaxValue); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- Functions / procedures ----

type Function struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
	Kind   string `json:"kind"` // func | proc | agg | window
	Args   string `json:"args"`
	Result string `json:"result"`
}

func buildFunctionsQuery(schema string) (string, []any) {
	q := `SELECT n.nspname, p.proname,
       CASE p.prokind WHEN 'f' THEN 'func' WHEN 'p' THEN 'proc' WHEN 'a' THEN 'agg' WHEN 'w' THEN 'window' ELSE p.prokind::text END,
       pg_catalog.pg_get_function_arguments(p.oid),
       pg_catalog.pg_get_function_result(p.oid)
FROM pg_catalog.pg_proc p
JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
WHERE n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'`
	var args []any
	if schema != "" {
		args = append(args, schema)
		q += fmt.Sprintf("\n  AND n.nspname = $%d", len(args))
	}
	q += "\nORDER BY n.nspname, p.proname"
	return q, args
}

func ListFunctions(ctx context.Context, q Querier, schema string) ([]Function, error) {
	sql, args := buildFunctionsQuery(schema)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Function
	for rows.Next() {
		var f Function
		if err := rows.Scan(&f.Schema, &f.Name, &f.Kind, &f.Args, &f.Result); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ---- Schemas ----

type Schema struct {
	Name   string `json:"name"`
	Owner  string `json:"owner"`
	Tables int64  `json:"tables"`
}

func buildSchemasQuery() string {
	return `SELECT n.nspname,
       pg_catalog.pg_get_userbyid(n.nspowner),
       (SELECT count(*) FROM pg_catalog.pg_class c WHERE c.relnamespace = n.oid AND c.relkind IN ('r', 'p'))
FROM pg_catalog.pg_namespace n
WHERE n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'
ORDER BY n.nspname`
}

func ListSchemas(ctx context.Context, q Querier) ([]Schema, error) {
	rows, err := q.Query(ctx, buildSchemasQuery())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Schema
	for rows.Next() {
		var s Schema
		if err := rows.Scan(&s.Name, &s.Owner, &s.Tables); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ---- describe view ----

type ViewDetail struct {
	Schema     string   `json:"schema"`
	Name       string   `json:"name"`
	Type       string   `json:"type"` // view | materialized
	Owner      string   `json:"owner"`
	Populated  bool     `json:"populated"` // matviews: false until first REFRESH
	Size       string   `json:"size"`
	Columns    []Column `json:"columns"`
	Definition string   `json:"definition"`
}

// DescribeView resolves a view or materialized view (bare or schema.name).
func DescribeView(ctx context.Context, q Querier, qualified string) (*ViewDetail, error) {
	schema, name := SplitQualified(qualified)
	q1 := `SELECT n.nspname, c.oid, c.relkind::text,
       pg_catalog.pg_get_userbyid(c.relowner),
       c.relispopulated,
       pg_catalog.pg_size_pretty(pg_catalog.pg_total_relation_size(c.oid)),
       pg_catalog.pg_get_viewdef(c.oid, true)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('v', 'm')
  AND c.relname = $1
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'`
	args := []any{name}
	if schema != "" {
		args = append(args, schema)
		q1 += fmt.Sprintf("\n  AND n.nspname = $%d", len(args))
	}
	q1 += "\nORDER BY n.nspname"

	rows, err := q.Query(ctx, q1, args...)
	if err != nil {
		return nil, err
	}
	type vm struct {
		d    ViewDetail
		oid  uint32
		kind string
	}
	var matches []vm
	for rows.Next() {
		var m vm
		if err := rows.Scan(&m.d.Schema, &m.oid, &m.kind, &m.d.Owner, &m.d.Populated, &m.d.Size, &m.d.Definition); err != nil {
			rows.Close()
			return nil, err
		}
		m.d.Name = name
		if m.kind == "m" {
			m.d.Type = "materialized"
		} else {
			m.d.Type = "view"
		}
		matches = append(matches, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	switch {
	case len(matches) == 0:
		return nil, fmt.Errorf("view %q not found", qualified)
	case len(matches) > 1:
		schemas := make([]string, len(matches))
		for i, m := range matches {
			schemas[i] = m.d.Schema
		}
		return nil, fmt.Errorf("view %q exists in multiple schemas (%s); qualify it as schema.name",
			name, strings.Join(schemas, ", "))
	}
	m := matches[0]

	crows, err := q.Query(ctx, buildColumnsQuery(), m.oid)
	if err != nil {
		return nil, err
	}
	for crows.Next() {
		var c Column
		var notnull bool
		if err := crows.Scan(&c.Name, &c.Type, &notnull, &c.Default); err != nil {
			crows.Close()
			return nil, err
		}
		c.Nullable = !notnull
		m.d.Columns = append(m.d.Columns, c)
	}
	crows.Close()
	if err := crows.Err(); err != nil {
		return nil, err
	}
	return &m.d, nil
}

// ServerVersionNum returns the server version as an integer (e.g. 160003 for 16.3),
// from the server_version_num GUC — used to gate features like EXPLAIN (GENERIC_PLAN),
// which requires PostgreSQL 16+ (>= 160000).
func ServerVersionNum(ctx context.Context, q Querier) (int, error) {
	rows, err := q.Query(ctx, "SELECT current_setting('server_version_num')::int")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	var n int
	if rows.Next() {
		if err := rows.Scan(&n); err != nil {
			return 0, err
		}
	}
	return n, rows.Err()
}

// RelCostStats are the planner inputs behind a scan node's cost — the catalog facts that
// EXPLAIN output does not carry. Used by `pgdx explain -vvv` to ground the cost breakdown.
type RelCostStats struct {
	Relname    string  `json:"relation"`
	Reltuples  float64 `json:"reltuples"` // planner row estimate (pg_class)
	Relpages   int64   `json:"relpages"`  // pages the planner believes the heap occupies
	SizePretty string  `json:"heap_size"` // pg_size_pretty(pg_relation_size)
}

// RelationCostStats reads reltuples/relpages/heap size for one relation. The name is
// resolved with ::regclass, so it honors search_path and quoting exactly as the planner did.
func RelationCostStats(ctx context.Context, q Querier, relname string) (*RelCostStats, error) {
	rows, err := q.Query(ctx,
		`SELECT reltuples::float8, relpages::bigint,
		        pg_catalog.pg_size_pretty(pg_catalog.pg_relation_size(oid))
		 FROM pg_catalog.pg_class WHERE oid = $1::regclass`, relname)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	s := RelCostStats{Relname: relname}
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("relation %q not found in pg_class", relname)
	}
	if err := rows.Scan(&s.Reltuples, &s.Relpages, &s.SizePretty); err != nil {
		return nil, err
	}
	return &s, rows.Err()
}

// CostSettings are the planner cost knobs the -vvv breakdown multiplies through.
type CostSettings struct {
	SeqPageCost     float64 `json:"seq_page_cost"`
	CPUTupleCost    float64 `json:"cpu_tuple_cost"`
	CPUOperatorCost float64 `json:"cpu_operator_cost"`
}

// PlannerCostSettings reads the per-session cost knobs used in scan cost estimation.
func PlannerCostSettings(ctx context.Context, q Querier) (CostSettings, error) {
	var cs CostSettings
	rows, err := q.Query(ctx,
		`SELECT name, setting::float8 FROM pg_catalog.pg_settings
		 WHERE name IN ('seq_page_cost','cpu_tuple_cost','cpu_operator_cost')`)
	if err != nil {
		return cs, err
	}
	defer rows.Close()
	for rows.Next() {
		var name string
		var val float64
		if err := rows.Scan(&name, &val); err != nil {
			return cs, err
		}
		switch name {
		case "seq_page_cost":
			cs.SeqPageCost = val
		case "cpu_tuple_cost":
			cs.CPUTupleCost = val
		case "cpu_operator_cost":
			cs.CPUOperatorCost = val
		}
	}
	return cs, rows.Err()
}

// ServerTime returns the server's current timestamp (its clock, not the client's) as
// text — used for the `pgdx status` header so the snapshot is stamped accurately.
func ServerTime(ctx context.Context, q Querier) (string, error) {
	rows, err := q.Query(ctx, "SELECT now()::timestamptz(0)::text")
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var s string
	if rows.Next() {
		if err := rows.Scan(&s); err != nil {
			return "", err
		}
	}
	return s, rows.Err()
}

// ---- Bloat leaderboard (get bloat) ----

// TableBloat is one row of `pgdx get bloat`: a table with an estimate of how much
// space VACUUM could make reusable, derived from the dead-tuple ratio.
type TableBloat struct {
	Schema     string  `json:"schema"`
	Name       string  `json:"name"`
	SizeBytes  int64   `json:"size_bytes"`  // pg_total_relation_size (heap+indexes+toast)
	TableBytes int64   `json:"table_bytes"` // pg_table_size (heap+toast) — the waste base
	LiveTup    int64   `json:"live_tuples"`
	DeadTup    int64   `json:"dead_tuples"`
	DeadRatio  float64 `json:"dead_ratio"`            // 0..1
	WasteBytes int64   `json:"est_waste_bytes"`       // dead_ratio * table_bytes (estimate)
	LastVacuum string  `json:"last_vacuum,omitempty"` // most recent (auto)vacuum, "" if never
}

// buildTableBloatQuery ranks tables by estimated reclaimable bytes (dead-tuple
// fraction × heap size). It deliberately uses the already-collected pg_stat counters
// rather than scanning pages (pgstattuple): free, and it never adds load to a database
// that may already be struggling (D6). The estimate is approximate — it answers "where
// is VACUUM most worth running", not "exactly N bytes are wasted".
func buildTableBloatQuery(schema string, limit int) (string, []any) {
	q := `SELECT n.nspname, c.relname,
       pg_catalog.pg_total_relation_size(c.oid)::bigint,
       pg_catalog.pg_table_size(c.oid)::bigint,
       COALESCE(s.n_live_tup, 0),
       COALESCE(s.n_dead_tup, 0),
       COALESCE(GREATEST(s.last_vacuum, s.last_autovacuum)::text, '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r', 'm')
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'
  AND COALESCE(s.n_dead_tup, 0) > 0`
	var args []any
	if schema != "" {
		args = append(args, schema)
		q += fmt.Sprintf("\n  AND n.nspname = $%d", len(args))
	}
	// Order by the same estimate we report: dead fraction × heap size, biggest first.
	q += `
ORDER BY (CASE WHEN (COALESCE(s.n_live_tup,0) + COALESCE(s.n_dead_tup,0)) > 0
               THEN COALESCE(s.n_dead_tup,0)::float8 / (COALESCE(s.n_live_tup,0) + COALESCE(s.n_dead_tup,0))
                    * pg_catalog.pg_table_size(c.oid)
               ELSE 0 END) DESC`
	args = append(args, limit)
	q += fmt.Sprintf("\nLIMIT $%d", len(args))
	return q, args
}

// ListTableBloat returns the bloat leaderboard, biggest estimated waste first.
func ListTableBloat(ctx context.Context, q Querier, schema string, limit int) ([]TableBloat, error) {
	sql, args := buildTableBloatQuery(schema, limit)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableBloat
	for rows.Next() {
		var b TableBloat
		if err := rows.Scan(&b.Schema, &b.Name, &b.SizeBytes, &b.TableBytes,
			&b.LiveTup, &b.DeadTup, &b.LastVacuum); err != nil {
			return nil, err
		}
		if total := b.LiveTup + b.DeadTup; total > 0 {
			b.DeadRatio = float64(b.DeadTup) / float64(total)
			b.WasteBytes = int64(b.DeadRatio * float64(b.TableBytes))
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// ---- Long-running transactions (get transaction-age) ----

// LongTxn is one open transaction, oldest first. A long-lived transaction (even an
// idle one) holds back the xmin horizon and prevents VACUUM from cleaning dead tuples
// across the whole database — a leading cause of creeping bloat.
type LongTxn struct {
	PID       int32   `json:"pid"`
	User      string  `json:"user"`
	Database  string  `json:"database"`
	State     string  `json:"state"`
	XactSec   float64 `json:"xact_age_sec"`  // since xact_start
	StateSec  float64 `json:"state_age_sec"` // since state_change (e.g. how long idle-in-tx)
	WaitEvent string  `json:"wait_event,omitempty"`
	Query     string  `json:"query"`
}

// buildLongTxnQuery lists backends with an open transaction, oldest first. minSec
// filters out transactions younger than the threshold (0 = show all open ones).
func buildLongTxnQuery(minSec float64) (string, []any) {
	q := `SELECT pid,
       COALESCE(usename, ''),
       COALESCE(datname, ''),
       COALESCE(state, ''),
       COALESCE(EXTRACT(epoch FROM (now() - xact_start)), -1)::float8,
       COALESCE(EXTRACT(epoch FROM (now() - state_change)), -1)::float8,
       COALESCE(wait_event, ''),
       COALESCE(query, '')
FROM pg_catalog.pg_stat_activity
WHERE xact_start IS NOT NULL
  AND pid <> pg_catalog.pg_backend_pid()`
	var args []any
	if minSec > 0 {
		args = append(args, minSec)
		q += fmt.Sprintf("\n  AND now() - xact_start >= make_interval(secs => $%d)", len(args))
	}
	q += "\nORDER BY xact_start ASC"
	return q, args
}

// ListLongTransactions returns open transactions oldest-first, optionally filtered to
// those at least minSec old.
func ListLongTransactions(ctx context.Context, q Querier, minSec float64) ([]LongTxn, error) {
	sql, args := buildLongTxnQuery(minSec)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LongTxn
	for rows.Next() {
		var t LongTxn
		if err := rows.Scan(&t.PID, &t.User, &t.Database, &t.State,
			&t.XactSec, &t.StateSec, &t.WaitEvent, &t.Query); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- XID wraparound risk (get vacuum-health) ----

// WraparoundRisk is one relation's transaction-ID age relative to the threshold at
// which autovacuum is forced to do an (expensive, anti-wraparound) freeze. Left
// unchecked, XID exhaustion shuts the database down to read-only — so this is the most
// urgent class of vacuum problem.
type WraparoundRisk struct {
	Schema      string  `json:"schema"`
	Name        string  `json:"name"`
	XIDAge      int64   `json:"xid_age"`              // age(relfrozenxid): transactions since last freeze
	MaxAge      int64   `json:"freeze_max_age"`       // autovacuum_freeze_max_age
	PctToFreeze float64 `json:"pct_to_forced_vacuum"` // XIDAge / MaxAge * 100
	Size        string  `json:"size"`
	LastVacuum  string  `json:"last_vacuum,omitempty"`
	Owner       string  `json:"owner,omitempty"` // for a TOAST relation: the "schema.table" it backs
}

// buildWraparoundQuery ranks relations by transaction-ID age, oldest first. Includes
// ordinary tables, matviews and TOAST relations (any of which can be the culprit);
// partitioned parents (relkind 'p') have no storage / frozenxid so are excluded.
// Wraparound is a cluster-wide concern, so system schemas are intentionally NOT
// filtered out — a pg_catalog or pg_toast table can be the one in danger.
//
// Each TOAST relation is resolved back to the table it backs (owner), so a TOAST row is
// actionable (you can see which table to vacuum) and so the optional schema filter can
// keep a TOAST relation whose owning table is in that schema — a plain nspname match
// would drop pg_toast.* and hide the very risk this command exists to surface.
func buildWraparoundQuery(schema string, limit int) (string, []any) {
	q := `SELECT n.nspname, c.relname,
       pg_catalog.age(c.relfrozenxid)::bigint,
       current_setting('autovacuum_freeze_max_age')::bigint,
       pg_catalog.pg_size_pretty(pg_catalog.pg_total_relation_size(c.oid)),
       COALESCE(GREATEST(s.last_vacuum, s.last_autovacuum)::text, ''),
       COALESCE(ons.nspname || '.' || owner.relname, '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid
LEFT JOIN pg_catalog.pg_class owner ON owner.reltoastrelid = c.oid
LEFT JOIN pg_catalog.pg_namespace ons ON ons.oid = owner.relnamespace
WHERE c.relkind IN ('r', 'm', 't')
  AND c.relfrozenxid <> 0`
	var args []any
	if schema != "" {
		args = append(args, schema)
		// Match the relation's own schema OR — for a TOAST relation — the schema of the
		// table it backs, so the filter never hides a TOAST relation that is the real
		// wraparound risk for one of this schema's tables.
		q += fmt.Sprintf("\n  AND (n.nspname = $%d OR ons.nspname = $%d)", len(args), len(args))
	}
	q += "\nORDER BY pg_catalog.age(c.relfrozenxid) DESC"
	args = append(args, limit)
	q += fmt.Sprintf("\nLIMIT $%d", len(args))
	return q, args
}

// ListWraparoundRisk returns relations ranked by XID age (closest to a forced
// anti-wraparound vacuum first). schema is optional ("" = the whole cluster, including
// system and TOAST relations); a non-empty schema also keeps TOAST relations whose
// owning table lives in that schema.
func ListWraparoundRisk(ctx context.Context, q Querier, schema string, limit int) ([]WraparoundRisk, error) {
	sql, args := buildWraparoundQuery(schema, limit)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WraparoundRisk
	for rows.Next() {
		var w WraparoundRisk
		if err := rows.Scan(&w.Schema, &w.Name, &w.XIDAge, &w.MaxAge, &w.Size, &w.LastVacuum, &w.Owner); err != nil {
			return nil, err
		}
		if w.MaxAge > 0 {
			w.PctToFreeze = 100 * float64(w.XIDAge) / float64(w.MaxAge)
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// ---- Table usage (get tables --usage) ----

// TableUsage is one row of `pgdx get tables --usage`: how a table is read (sequential
// vs index scans) and written (inserts/updates/deletes), from pg_stat_all_tables. A
// large table with mostly sequential scans is a candidate for a better index.
type TableUsage struct {
	Schema  string  `json:"schema"`
	Name    string  `json:"name"`
	SeqScan int64   `json:"seq_scan"`
	IdxScan int64   `json:"idx_scan"`
	IdxPct  float64 `json:"idx_scan_pct"` // idx_scan / (seq_scan+idx_scan) * 100; -1 if never scanned
	Inserts int64   `json:"inserts"`
	Updates int64   `json:"updates"`
	Deletes int64   `json:"deletes"`
}

// buildTableUsageQuery lists tables with read/write counters, most sequentially-scanned
// first (the actionable end: heavy seq scans usually mean a missing index).
func buildTableUsageQuery(schema string) (string, []any) {
	q := `SELECT n.nspname, c.relname,
       COALESCE(s.seq_scan, 0),
       COALESCE(s.idx_scan, 0),
       COALESCE(s.n_tup_ins, 0),
       COALESCE(s.n_tup_upd, 0),
       COALESCE(s.n_tup_del, 0)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_stat_all_tables s ON s.relid = c.oid
WHERE c.relkind IN ('r', 'p')
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'`
	var args []any
	if schema != "" {
		args = append(args, schema)
		q += fmt.Sprintf("\n  AND n.nspname = $%d", len(args))
	}
	q += "\nORDER BY COALESCE(s.seq_scan, 0) DESC, n.nspname, c.relname"
	return q, args
}

// ListTableUsage returns per-table read/write usage, most sequentially-scanned first.
func ListTableUsage(ctx context.Context, q Querier, schema string) ([]TableUsage, error) {
	sql, args := buildTableUsageQuery(schema)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TableUsage
	for rows.Next() {
		var u TableUsage
		if err := rows.Scan(&u.Schema, &u.Name, &u.SeqScan, &u.IdxScan,
			&u.Inserts, &u.Updates, &u.Deletes); err != nil {
			return nil, err
		}
		if total := u.SeqScan + u.IdxScan; total > 0 {
			u.IdxPct = 100 * float64(u.IdxScan) / float64(total)
		} else {
			u.IdxPct = -1 // never scanned at all
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ---- Database summary (pgdx summarize) ----

// TableSize is one entry of the summary's largest-tables list.
type TableSize struct {
	Schema    string `json:"schema"`
	Name      string `json:"name"`
	SizeBytes int64  `json:"size_bytes"` // pg_total_relation_size (heap+indexes+toast)
}

// DatabaseSummary is the at-a-glance inventory of one database: object counts, a size
// breakdown, index-health and bloat rollups, and the largest tables. Counts and the size
// breakdown cover user schemas only (system catalogs are excluded), so the per-object
// byte figures intentionally don't sum to SizeBytes (the authoritative whole-database
// total, which includes catalogs).
type DatabaseSummary struct {
	Database          string      `json:"database"`
	Encoding          string      `json:"encoding"`
	SizeBytes         int64       `json:"size_bytes"`
	TableBytes        int64       `json:"user_table_bytes"` // heap + TOAST of user tables/matviews
	IndexBytes        int64       `json:"user_index_bytes"`
	Schemas           int64       `json:"schemas"`
	Tables            int64       `json:"tables"`
	Partitioned       int64       `json:"partitioned_tables"` // subset of Tables that are partitioned parents
	Views             int64       `json:"views"`
	MaterializedViews int64       `json:"materialized_views"`
	Indexes           int64       `json:"indexes"`
	Sequences         int64       `json:"sequences"`
	Functions         int64       `json:"functions"`
	Extensions        int64       `json:"extensions"`
	UnusedIndexes     int64       `json:"unused_indexes"` // non-unique, 0 scans
	UnusedIndexBytes  int64       `json:"unused_index_bytes"`
	RedundantIndexes  int64       `json:"redundant_indexes"`
	EstBloatBytes     int64       `json:"est_bloat_bytes"` // estimated reclaimable, summed
	TopTables         []TableSize `json:"top_tables"`
}

// SummarizeDatabase gathers the one-screen database inventory. It composes a handful of
// cheap aggregate catalog queries (and reuses ListRedundantIndexes); topN bounds the
// largest-tables list. Read-only.
func SummarizeDatabase(ctx context.Context, q Querier, topN int) (*DatabaseSummary, error) {
	s := &DatabaseSummary{}

	const scalarQ = `SELECT current_database(),
       (SELECT pg_catalog.pg_encoding_to_char(encoding) FROM pg_catalog.pg_database WHERE datname = current_database()),
       pg_catalog.pg_database_size(current_database())::bigint,
       (SELECT count(*) FROM pg_catalog.pg_namespace n WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'),
       (SELECT count(*) FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
              WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'),
       (SELECT count(*) FROM pg_catalog.pg_extension)`
	if err := scanRow(ctx, q, scalarQ, &s.Database, &s.Encoding, &s.SizeBytes, &s.Schemas, &s.Functions, &s.Extensions); err != nil {
		return nil, err
	}

	// Relkind counts + the user-object size breakdown, in one pass over pg_class. Storage
	// functions are summed only over relations that have storage (ordinary/partitioned
	// tables and matviews); a partitioned parent contributes 0, which is correct.
	const classQ = `SELECT
       count(*) FILTER (WHERE c.relkind IN ('r','p')),
       count(*) FILTER (WHERE c.relkind = 'p'),
       count(*) FILTER (WHERE c.relkind = 'v'),
       count(*) FILTER (WHERE c.relkind = 'm'),
       count(*) FILTER (WHERE c.relkind IN ('i','I')),
       count(*) FILTER (WHERE c.relkind = 'S'),
       COALESCE(sum(pg_catalog.pg_table_size(c.oid)) FILTER (WHERE c.relkind IN ('r','p','m')), 0)::bigint,
       COALESCE(sum(pg_catalog.pg_indexes_size(c.oid)) FILTER (WHERE c.relkind IN ('r','p','m')), 0)::bigint
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`
	if err := scanRow(ctx, q, classQ,
		&s.Tables, &s.Partitioned, &s.Views, &s.MaterializedViews, &s.Indexes, &s.Sequences,
		&s.TableBytes, &s.IndexBytes); err != nil {
		return nil, err
	}

	// Unused indexes (non-unique, never scanned) and the space they occupy.
	const unusedQ = `SELECT count(*), COALESCE(sum(pg_catalog.pg_relation_size(i.oid)), 0)::bigint
FROM pg_catalog.pg_index ix
JOIN pg_catalog.pg_class i ON i.oid = ix.indexrelid
JOIN pg_catalog.pg_namespace n ON n.oid = i.relnamespace
LEFT JOIN pg_catalog.pg_stat_user_indexes su ON su.indexrelid = ix.indexrelid
WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
  AND NOT ix.indisunique AND COALESCE(su.idx_scan, 0) = 0`
	if err := scanRow(ctx, q, unusedQ, &s.UnusedIndexes, &s.UnusedIndexBytes); err != nil {
		return nil, err
	}

	// Estimated reclaimable space, summed with the same dead-ratio × heap estimate as
	// `get bloat`.
	const bloatQ = `SELECT COALESCE(sum(
         CASE WHEN (COALESCE(st.n_live_tup,0) + COALESCE(st.n_dead_tup,0)) > 0
              THEN COALESCE(st.n_dead_tup,0)::float8 / (COALESCE(st.n_live_tup,0) + COALESCE(st.n_dead_tup,0))
                   * pg_catalog.pg_table_size(c.oid)
              ELSE 0 END), 0)::bigint
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_stat_all_tables st ON st.relid = c.oid
WHERE c.relkind IN ('r','m') AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'`
	if err := scanRow(ctx, q, bloatQ, &s.EstBloatBytes); err != nil {
		return nil, err
	}

	// Largest tables by total size.
	const topQ = `SELECT n.nspname, c.relname, pg_catalog.pg_total_relation_size(c.oid)::bigint
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p','m') AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
ORDER BY pg_catalog.pg_total_relation_size(c.oid) DESC
LIMIT $1`
	rows, err := q.Query(ctx, topQ, topN)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var t TableSize
		if err := rows.Scan(&t.Schema, &t.Name, &t.SizeBytes); err != nil {
			rows.Close()
			return nil, err
		}
		s.TopTables = append(s.TopTables, t)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Redundant (duplicate/prefix) indexes — reuse the structural analyzer.
	red, err := ListRedundantIndexes(ctx, q, "", "")
	if err != nil {
		return nil, err
	}
	s.RedundantIndexes = int64(len(red))

	return s, nil
}

// scanRow runs a query expected to return a single row and scans it into dest. No rows
// leaves dest at its zero values (not an error).
func scanRow(ctx context.Context, q Querier, sql string, dest ...any) error {
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(dest...); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ListTables returns tables visible in the database, optionally filtered to one
// schema. An empty schema means all non-system schemas.
func ListTables(ctx context.Context, q Querier, schema string) ([]Table, error) {
	sql, args := buildTablesQuery(schema)
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Table
	for rows.Next() {
		var t Table
		if err := rows.Scan(&t.Schema, &t.Name, &t.Owner, &t.EstRows, &t.Size, &t.LiveTup, &t.DeadTup); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RelationName is a schema-qualified relation name. It is intentionally minimal (no
// stats or sizes) so the shell's tab-completion can list candidate names on a single
// keypress without paying the cost of the full get/describe queries.
type RelationName struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

// ListRelationNames returns the names of relations whose relkind is in kinds — e.g.
// 'r','p' for tables (incl. partitioned), 'v','m' for views/matviews, 'i','I' for
// indexes — excluding system schemas. The result is capped so completion stays snappy
// on databases with very many objects; a prefix the user is typing still narrows it.
// kinds holds fixed single-char constants from the caller (never user input).
func ListRelationNames(ctx context.Context, q Querier, kinds ...string) ([]RelationName, error) {
	if len(kinds) == 0 {
		return nil, nil
	}
	const sql = `SELECT n.nspname, c.relname
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind::text = ANY($1)
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'
ORDER BY n.nspname, c.relname
LIMIT 5000`
	rows, err := q.Query(ctx, sql, kinds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RelationName
	for rows.Next() {
		var r RelationName
		if err := rows.Scan(&r.Schema, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ListDatabaseNames returns the names of connectable, non-template databases, for the
// shell's `use <database>` tab-completion. It is intentionally minimal (names only),
// unlike ListDatabases which gathers sizes and stats for `pgdx get databases`.
func ListDatabaseNames(ctx context.Context, q Querier) ([]string, error) {
	const sql = `SELECT datname
FROM pg_catalog.pg_database
WHERE datallowconn AND NOT datistemplate
ORDER BY datname`
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// SchemaExists reports whether a schema of the given name exists. The shell's
// `use schema <name>` uses it to reject a typo before changing search_path (Postgres
// itself accepts a non-existent schema in search_path and silently ignores it).
func SchemaExists(ctx context.Context, q Querier, name string) (bool, error) {
	rows, err := q.Query(ctx, `SELECT EXISTS(SELECT 1 FROM pg_catalog.pg_namespace WHERE nspname = $1)`, name)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	var ok bool
	if rows.Next() {
		if err := rows.Scan(&ok); err != nil {
			return false, err
		}
	}
	return ok, rows.Err()
}

// SetSearchPath sets the session search_path to a single schema and returns the value
// actually applied. The name is passed through quote_ident so identifiers needing quotes
// are handled safely. It must run outside a transaction (autocommit) so the setting
// persists for the rest of the session; it's a session GUC change, not a data write, so
// it's allowed under pgdx's read-only posture.
func SetSearchPath(ctx context.Context, q Querier, schema string) (string, error) {
	rows, err := q.Query(ctx, `SELECT set_config('search_path', quote_ident($1), false)`, schema)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var applied string
	if rows.Next() {
		if err := rows.Scan(&applied); err != nil {
			return "", err
		}
	}
	return applied, rows.Err()
}

// ListSchemaNames returns user schema names (system schemas excluded), for the shell's
// `use schema <TAB>` completion.
func ListSchemaNames(ctx context.Context, q Querier) ([]string, error) {
	const sql = `SELECT nspname
FROM pg_catalog.pg_namespace
WHERE nspname !~ '^pg_' AND nspname <> 'information_schema'
ORDER BY nspname`
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// TableStats is a lightweight snapshot of a table's tuple counts and on-disk size, used
// by `pgdx vacuum` to report what a run reclaimed (sampled before and after).
type TableStats struct {
	LiveTup   int64 `json:"live_tuples"`
	DeadTup   int64 `json:"dead_tuples"`
	SizeBytes int64 `json:"size_bytes"` // pg_total_relation_size (heap + indexes + TOAST)
	EstRows   int64 `json:"est_rows"`   // pg_class.reltuples (planner estimate); -1 = never analyzed
}

// GetTableStats reads a table's planner row estimate (pg_class.reltuples), live/dead tuple
// counts (pg_stat_all_tables), and total relation size by OID. Used around a VACUUM to
// compute tuples reclaimed and space freed, and after an ANALYZE to report the refreshed
// row estimate.
func GetTableStats(ctx context.Context, q Querier, oid uint32) (TableStats, error) {
	var s TableStats
	rows, err := q.Query(ctx, `SELECT COALESCE(st.n_live_tup, 0), COALESCE(st.n_dead_tup, 0),
       pg_catalog.pg_total_relation_size(c.oid)::bigint,
       c.reltuples::bigint
FROM pg_catalog.pg_class c
LEFT JOIN pg_catalog.pg_stat_all_tables st ON st.relid = c.oid
WHERE c.oid = $1`, oid)
	if err != nil {
		return s, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&s.LiveTup, &s.DeadTup, &s.SizeBytes, &s.EstRows); err != nil {
			return s, err
		}
	}
	return s, rows.Err()
}

// CountTablesForAnalyze counts ordinary + partitioned tables in scope (a single schema, or
// every non-system schema when schema is empty) and, of those, how many have never been
// analyzed (reltuples < 0). It lets `pgdx analyze --all`/`--schema` report a concise
// summary — "N tables, M had no prior statistics" — without enumerating every name.
func CountTablesForAnalyze(ctx context.Context, q Querier, schema string) (total, missing int, err error) {
	sql := `SELECT count(*), count(*) FILTER (WHERE c.reltuples < 0)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r', 'p')
  AND n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'`
	var args []any
	if schema != "" {
		args = append(args, schema)
		sql += "\n  AND n.nspname = $1"
	}
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return 0, 0, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&total, &missing); err != nil {
			return 0, 0, err
		}
	}
	return total, missing, rows.Err()
}
