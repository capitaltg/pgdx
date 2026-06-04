package catalog

import (
	"strings"
	"testing"
)

func TestBuildTablesQuery_AllSchemas(t *testing.T) {
	sql, args := buildTablesQuery("")
	if len(args) != 0 {
		t.Fatalf("no-schema query should have no args, got %v", args)
	}
	for _, want := range []string{
		"pg_catalog.pg_class",
		"relkind IN ('r', 'p')", // ordinary + partitioned tables
		"!~ '^pg_'",             // exclude system schemas
		"<> 'information_schema'",
		"ORDER BY n.nspname, c.relname",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("query missing %q:\n%s", want, sql)
		}
	}
	if strings.Contains(sql, "$1") {
		t.Fatalf("no-schema query must not reference $1:\n%s", sql)
	}
}

func TestBuildTablesQuery_OneSchema(t *testing.T) {
	sql, args := buildTablesQuery("public")
	if len(args) != 1 || args[0] != "public" {
		t.Fatalf("schema filter args = %v, want [public]", args)
	}
	if !strings.Contains(sql, "n.nspname = $1") {
		t.Fatalf("schema filter not parameterized (SQL-injection risk):\n%s", sql)
	}
}

func TestBuildIndexesQuery(t *testing.T) {
	t.Run("no filters", func(t *testing.T) {
		sql, args := buildIndexesQuery("", "", false, "name")
		if len(args) != 0 {
			t.Fatalf("want no args, got %v", args)
		}
		for _, want := range []string{"pg_catalog.pg_index", "pg_stat_user_indexes", "idx_scan"} {
			if !strings.Contains(sql, want) {
				t.Fatalf("query missing %q:\n%s", want, sql)
			}
		}
		if strings.Contains(sql, "$1") {
			t.Fatalf("unfiltered query must not reference $1:\n%s", sql)
		}
	})
	t.Run("schema only", func(t *testing.T) {
		sql, args := buildIndexesQuery("public", "", false, "name")
		if len(args) != 1 || args[0] != "public" {
			t.Fatalf("args = %v, want [public]", args)
		}
		if !strings.Contains(sql, "n.nspname = $1") {
			t.Fatalf("schema filter not parameterized:\n%s", sql)
		}
	})
	t.Run("table only", func(t *testing.T) {
		sql, args := buildIndexesQuery("", "orders", false, "name")
		if len(args) != 1 || args[0] != "orders" {
			t.Fatalf("args = %v, want [orders]", args)
		}
		if !strings.Contains(sql, "t.relname = $1") {
			t.Fatalf("table filter not parameterized:\n%s", sql)
		}
	})
	t.Run("both filters get distinct placeholders", func(t *testing.T) {
		sql, args := buildIndexesQuery("public", "orders", false, "name")
		if len(args) != 2 || args[0] != "public" || args[1] != "orders" {
			t.Fatalf("args = %v, want [public orders]", args)
		}
		if !strings.Contains(sql, "n.nspname = $1") || !strings.Contains(sql, "t.relname = $2") {
			t.Fatalf("placeholders not numbered correctly:\n%s", sql)
		}
	})
	t.Run("unused filters to 0-scan non-unique", func(t *testing.T) {
		sql, _ := buildIndexesQuery("", "", true, "name")
		if !strings.Contains(sql, "COALESCE(s.idx_scan, 0) = 0") || !strings.Contains(sql, "NOT ix.indisunique") {
			t.Fatalf("unused query should filter 0-scan non-unique:\n%s", sql)
		}
	})
	t.Run("sort keys drive ORDER BY", func(t *testing.T) {
		size, _ := buildIndexesQuery("", "", false, "size")
		if !strings.Contains(size, "ORDER BY pg_catalog.pg_relation_size(i.oid) DESC") {
			t.Fatalf("--sort size should order by size desc:\n%s", size)
		}
		scans, _ := buildIndexesQuery("", "", false, "scans")
		if !strings.Contains(scans, "ORDER BY COALESCE(s.idx_scan, 0) DESC") {
			t.Fatalf("--sort scans should order by scans desc:\n%s", scans)
		}
		name, _ := buildIndexesQuery("", "", false, "name")
		if !strings.Contains(name, "ORDER BY n.nspname, t.relname, i.relname") {
			t.Fatalf("--sort name should order by schema/table/name:\n%s", name)
		}
		// Numeric sorts keep a stable tiebreaker so equal rows don't reorder run-to-run.
		if !strings.Contains(size, "DESC, n.nspname, t.relname, i.relname") {
			t.Fatalf("--sort size should carry a name tiebreaker:\n%s", size)
		}
		// An unknown key falls back to name rather than producing invalid SQL.
		bad, _ := buildIndexesQuery("", "", false, "bogus")
		if !strings.Contains(bad, "ORDER BY n.nspname, t.relname, i.relname") {
			t.Fatalf("unknown sort key should fall back to name:\n%s", bad)
		}
	})
}

func TestValidIndexSort(t *testing.T) {
	for _, k := range IndexSortKeys {
		if !ValidIndexSort(k) {
			t.Errorf("IndexSortKeys lists %q but ValidIndexSort rejects it", k)
		}
	}
	if ValidIndexSort("bogus") {
		t.Error("ValidIndexSort should reject an unknown key")
	}
}

func TestBuildSlowQueriesQuery(t *testing.T) {
	t.Run("default and explicit total sort by total_exec_time", func(t *testing.T) {
		for _, key := range []string{"total", "bogus"} { // unknown key falls back to total
			sql := buildSlowQueriesQuery(key, false)
			for _, want := range []string{"pg_stat_statements", "stddev_exec_time", "wal_bytes", "ORDER BY total_exec_time DESC", "LIMIT $1"} {
				if !strings.Contains(sql, want) {
					t.Fatalf("slow-queries query (%s) missing %q:\n%s", key, want, sql)
				}
			}
		}
	})
	t.Run("each sort key maps to its expression", func(t *testing.T) {
		cases := map[string]string{
			"mean":   "ORDER BY mean_exec_time DESC",
			"max":    "ORDER BY max_exec_time DESC",
			"stddev": "ORDER BY stddev_exec_time DESC",
			"calls":  "ORDER BY calls DESC",
			"rows":   "ORDER BY rows DESC",
			"io":     "ORDER BY shared_blks_read DESC",
			"temp":   "ORDER BY (temp_blks_read + temp_blks_written) DESC",
		}
		for key, want := range cases {
			if sql := buildSlowQueriesQuery(key, false); !strings.Contains(sql, want) {
				t.Fatalf("sort %q should order by %q:\n%s", key, want, sql)
			}
		}
	})
	t.Run("currentDBOnly scopes by dbid, cluster-wide does not", func(t *testing.T) {
		scoped := buildSlowQueriesQuery("total", true)
		if !strings.Contains(scoped, "WHERE dbid = (SELECT oid FROM pg_catalog.pg_database WHERE datname = current_database())") {
			t.Fatalf("scoped query should filter by dbid:\n%s", scoped)
		}
		if strings.Contains(buildSlowQueriesQuery("total", false), "dbid") {
			t.Fatalf("cluster-wide query must not filter by dbid")
		}
	})
	t.Run("ValidSlowQuerySort gates input", func(t *testing.T) {
		if !ValidSlowQuerySort("io") || ValidSlowQuerySort("drop") {
			t.Fatal("ValidSlowQuerySort should accept io and reject drop")
		}
	})
}

func TestBuildRedundantIndexesQuery(t *testing.T) {
	t.Run("no filters", func(t *testing.T) {
		sql, args := buildRedundantIndexesQuery("", "")
		for _, want := range []string{"pg_catalog.pg_index", "ix.indkey::text", "indisvalid AND ix.indisready"} {
			if !strings.Contains(sql, want) {
				t.Fatalf("redundant query missing %q:\n%s", want, sql)
			}
		}
		if len(args) != 0 {
			t.Fatalf("want no args, got %v", args)
		}
	})
	t.Run("schema and table parameterized", func(t *testing.T) {
		sql, args := buildRedundantIndexesQuery("app", "orders")
		if len(args) != 2 || args[0] != "app" || args[1] != "orders" {
			t.Fatalf("args = %v, want [app orders]", args)
		}
		if !strings.Contains(sql, "n.nspname = $1") || !strings.Contains(sql, "t.relname = $2") {
			t.Fatalf("filters not parameterized:\n%s", sql)
		}
	})
}

func TestRedundancyOf(t *testing.T) {
	base := func(name string, cols []int16, unique bool) idxKeyRow {
		return idxKeyRow{schema: "app", table: "orders", name: name, am: "btree", cols: cols, unique: unique}
	}
	t.Run("prefix of a wider index is redundant", func(t *testing.T) {
		a := base("idx_a", []int16{1}, false)
		b := base("idx_ab", []int16{1, 2}, false)
		reason, ok := redundancyOf(a, b)
		if !ok || !strings.Contains(reason, "prefix of idx_ab") {
			t.Fatalf("want prefix reason, got ok=%v reason=%q", ok, reason)
		}
	})
	t.Run("wider index is NOT redundant against its prefix", func(t *testing.T) {
		a := base("idx_ab", []int16{1, 2}, false)
		b := base("idx_a", []int16{1}, false)
		if _, ok := redundancyOf(a, b); ok {
			t.Fatal("a superset must not be reported redundant")
		}
	})
	t.Run("unique index is never flagged", func(t *testing.T) {
		a := base("uq_a", []int16{1}, true)
		b := base("idx_ab", []int16{1, 2}, false)
		if _, ok := redundancyOf(a, b); ok {
			t.Fatal("a unique index must never be a drop candidate")
		}
	})
	t.Run("different column order is not a prefix", func(t *testing.T) {
		a := base("idx_ba", []int16{2, 1}, false)
		b := base("idx_ab", []int16{1, 2}, false)
		if _, ok := redundancyOf(a, b); ok {
			t.Fatal("(2,1) is not a prefix of (1,2)")
		}
	})
	t.Run("special (partial/expression) indexes are not compared", func(t *testing.T) {
		a := base("idx_a", []int16{1}, false)
		a.special = true
		b := base("idx_ab", []int16{1, 2}, false)
		if _, ok := redundancyOf(a, b); ok {
			t.Fatal("partial/expression indexes must be skipped")
		}
	})
	t.Run("exact duplicate flags only the loser once", func(t *testing.T) {
		a := base("idx_a1", []int16{1}, false)
		a.scans = 0
		b := base("idx_a2", []int16{1}, false)
		b.scans = 100
		_, aLoses := redundancyOf(a, b) // a has fewer scans → a is the loser
		_, bLoses := redundancyOf(b, a)
		if !aLoses || bLoses {
			t.Fatalf("exactly the lower-scan duplicate should be flagged: aLoses=%v bLoses=%v", aLoses, bLoses)
		}
	})
}

func TestParseIndkey(t *testing.T) {
	if cols, ok := parseIndkey("1 2 3"); !ok || len(cols) != 3 || cols[2] != 3 {
		t.Fatalf("parseIndkey(\"1 2 3\") = %v, %v", cols, ok)
	}
	if _, ok := parseIndkey("1 0 2"); ok {
		t.Fatal("a 0 entry (expression column) should make the key non-comparable")
	}
	if _, ok := parseIndkey(""); ok {
		t.Fatal("empty indkey should be non-comparable")
	}
}

func TestBuildLockWaitsQuery(t *testing.T) {
	sql := buildLockWaitsQuery()
	for _, want := range []string{"pg_locks", "NOT l.granted", "pg_blocking_pids"} {
		if !strings.Contains(sql, want) {
			t.Fatalf("lock-waits query missing %q:\n%s", want, sql)
		}
	}
}

func TestBuildBrowseQueries(t *testing.T) {
	t.Run("views relkind + schema filter", func(t *testing.T) {
		sql, args := buildViewsQuery("app")
		if !strings.Contains(sql, "c.relkind IN ('v', 'm')") || len(args) != 1 || !strings.Contains(sql, "$1") {
			t.Fatalf("views query wrong: args=%v\n%s", args, sql)
		}
	})
	t.Run("sequences use pg_sequences", func(t *testing.T) {
		sql, args := buildSequencesQuery("")
		if !strings.Contains(sql, "pg_catalog.pg_sequences") || len(args) != 0 {
			t.Fatalf("sequences query wrong: args=%v\n%s", args, sql)
		}
	})
	t.Run("functions use prokind", func(t *testing.T) {
		sql, _ := buildFunctionsQuery("")
		if !strings.Contains(sql, "p.prokind") || !strings.Contains(sql, "pg_get_function_arguments") {
			t.Fatalf("functions query wrong:\n%s", sql)
		}
	})
	t.Run("schemas exclude system", func(t *testing.T) {
		sql := buildSchemasQuery()
		if !strings.Contains(sql, "!~ '^pg_'") || !strings.Contains(sql, "<> 'information_schema'") {
			t.Fatalf("schemas query should exclude system schemas:\n%s", sql)
		}
	})
	t.Run("databases guard size by CONNECT, exclude templates", func(t *testing.T) {
		sql := buildDatabasesQuery("name")
		if !strings.Contains(sql, "has_database_privilege(current_user") {
			t.Fatalf("databases query should guard size by CONNECT privilege:\n%s", sql)
		}
		if !strings.Contains(sql, "NOT d.datistemplate") {
			t.Fatalf("databases query should exclude templates:\n%s", sql)
		}
		if !strings.Contains(sql, "ORDER BY d.datname") {
			t.Fatalf("name sort should order by datname:\n%s", sql)
		}
	})
	t.Run("databases sort=size orders by guarded size desc", func(t *testing.T) {
		sql := buildDatabasesQuery("size")
		if !strings.Contains(sql, ") DESC, d.datname") {
			t.Fatalf("size sort should order by size desc:\n%s", sql)
		}
		// The size expression in ORDER BY must keep the privilege guard (no error on no-CONNECT dbs).
		if strings.Count(sql, "has_database_privilege(current_user") < 2 {
			t.Fatalf("size-sort ORDER BY must also guard pg_database_size:\n%s", sql)
		}
	})
}

func TestBuildAvailableExtensionsQuery(t *testing.T) {
	sql := buildAvailableExtensionsQuery()
	for _, want := range []string{
		"pg_available_extensions",         // the installable set (what's on disk)
		"pg_available_extension_versions", // for the trusted flag
		"installed_version IS NOT NULL",   // installed status
		"aev.trusted",
		"ORDER BY ae.name",
	} {
		if !strings.Contains(sql, want) {
			t.Fatalf("available-extensions query missing %q:\n%s", want, sql)
		}
	}
}

func TestSplitQualified(t *testing.T) {
	cases := []struct{ in, schema, name string }{
		{"orders", "", "orders"},
		{"public.orders", "public", "orders"},
		{"reporting.daily_totals", "reporting", "daily_totals"},
	}
	for _, c := range cases {
		s, n := SplitQualified(c.in)
		if s != c.schema || n != c.name {
			t.Fatalf("SplitQualified(%q) = (%q,%q), want (%q,%q)", c.in, s, n, c.schema, c.name)
		}
	}
}

func TestBuildResolveQuery(t *testing.T) {
	t.Run("bare name", func(t *testing.T) {
		sql, args := buildResolveQuery("", "orders")
		if len(args) != 1 || args[0] != "orders" {
			t.Fatalf("args = %v, want [orders]", args)
		}
		if !strings.Contains(sql, "c.relname = $1") || strings.Contains(sql, "$2") {
			t.Fatalf("bare-name resolve query malformed:\n%s", sql)
		}
	})
	t.Run("qualified", func(t *testing.T) {
		sql, args := buildResolveQuery("public", "orders")
		if len(args) != 2 || args[1] != "public" {
			t.Fatalf("args = %v, want [orders public]", args)
		}
		if !strings.Contains(sql, "n.nspname = $2") {
			t.Fatalf("schema placeholder missing:\n%s", sql)
		}
	})
}

func TestBuildActivityQuery(t *testing.T) {
	def, args := buildActivityQuery(false, "blocked", "", 0)
	if len(args) != 0 {
		t.Fatalf("unfiltered activity query should have no args, got %v", args)
	}
	if !strings.Contains(def, "pg_stat_activity") || !strings.Contains(def, "pg_blocking_pids") {
		t.Fatalf("activity query missing core pieces:\n%s", def)
	}
	if !strings.Contains(def, "pg_backend_pid()") {
		t.Fatal("default activity query must exclude pgdx's own session")
	}
	if !strings.Contains(def, "state IS DISTINCT FROM 'idle'") {
		t.Fatal("default activity query should hide idle sessions")
	}
	all, _ := buildActivityQuery(true, "blocked", "", 0)
	if strings.Contains(all, "state IS DISTINCT FROM 'idle'") {
		t.Fatal("--all activity query must NOT filter idle sessions")
	}
}

func TestBuildActivityQuery_Datname(t *testing.T) {
	// The datname filter is parameterized (no injection) and independent of --all.
	sql, args := buildActivityQuery(true, "blocked", "movement_db", 0)
	if len(args) != 1 || args[0] != "movement_db" {
		t.Fatalf("args = %v, want [movement_db]", args)
	}
	if !strings.Contains(sql, "datname = $1") {
		t.Fatalf("datname filter not parameterized:\n%s", sql)
	}
	// Filtering by database must NOT re-introduce the idle filter when --all is set.
	if strings.Contains(sql, "state IS DISTINCT FROM 'idle'") {
		t.Fatalf("--all + --datname must still include idle sessions:\n%s", sql)
	}
}

func TestBuildActivityQuery_Sort(t *testing.T) {
	blocked, _ := buildActivityQuery(false, "blocked", "", 0)
	if !strings.Contains(blocked, "cardinality(pg_catalog.pg_blocking_pids(pid)) > 0) DESC") {
		t.Fatalf("blocked sort should order blocked-first:\n%s", blocked)
	}
	dur, _ := buildActivityQuery(false, "duration", "", 0)
	if strings.Contains(dur, "cardinality(") {
		t.Fatalf("duration sort should NOT use the blocked-first ordering:\n%s", dur)
	}
	if !strings.Contains(dur, "DESC") {
		t.Fatalf("duration sort should order by duration desc:\n%s", dur)
	}
}

func TestBuildActivityQuery_MinDuration(t *testing.T) {
	// The threshold is parameterized and gates on time-in-current-state (the same expr the
	// DURATION column and the duration sort use), so it means "running longer than N".
	sql, args := buildActivityQuery(false, "duration", "", 30)
	if len(args) != 1 || args[0] != float64(30) {
		t.Fatalf("args = %v, want [30]", args)
	}
	if !strings.Contains(sql, "CASE WHEN state = 'active' THEN query_start ELSE state_change END)), 0) >= $1") {
		t.Fatalf("min-duration filter not on the state-aware duration / not parameterized:\n%s", sql)
	}
	// Zero (the default) adds no filter and no arg.
	none, noneArgs := buildActivityQuery(false, "duration", "", 0)
	if len(noneArgs) != 0 || strings.Contains(none, ">= $") {
		t.Fatalf("min-duration 0 must add no filter:\n%s", none)
	}
	// It composes with --datname, keeping placeholder numbering correct ($1 datname, $2 dur).
	both, bothArgs := buildActivityQuery(false, "duration", "movement_db", 30)
	if len(bothArgs) != 2 || bothArgs[0] != "movement_db" || bothArgs[1] != float64(30) {
		t.Fatalf("args = %v, want [movement_db 30]", bothArgs)
	}
	if !strings.Contains(both, "datname = $1") || !strings.Contains(both, ">= $2") {
		t.Fatalf("datname/min-duration placeholders mis-numbered:\n%s", both)
	}
}

func TestBuildActivityQuery_StateAwareDuration(t *testing.T) {
	// DURATION must be time-in-current-state, not always since query_start: query_start
	// when active, state_change otherwise. This stops an idle pool connection from
	// looking like a long-running query.
	sql, _ := buildActivityQuery(true, "blocked", "", 0)
	if !strings.Contains(sql, "CASE WHEN state = 'active' THEN query_start ELSE state_change END") {
		t.Fatalf("DURATION should be state-aware (query_start if active, else state_change):\n%s", sql)
	}
}

func TestBuildIndexResolveQuery(t *testing.T) {
	sql, args := buildIndexResolveQuery("", "orders_pkey")
	if len(args) != 1 || args[0] != "orders_pkey" {
		t.Fatalf("args = %v, want [orders_pkey]", args)
	}
	if !strings.Contains(sql, "ix.indisvalid") || !strings.Contains(sql, "i.relkind IN ('i', 'I')") {
		t.Fatalf("index resolve query missing validity/relkind:\n%s", sql)
	}
	sql2, args2 := buildIndexResolveQuery("public", "orders_pkey")
	if len(args2) != 2 || args2[1] != "public" || !strings.Contains(sql2, "n.nspname = $2") {
		t.Fatalf("qualified index resolve malformed: args=%v\n%s", args2, sql2)
	}
}

func TestBuildTableBloatQuery(t *testing.T) {
	t.Run("no schema: ranks dead tables by estimated waste, parameterized limit", func(t *testing.T) {
		sql, args := buildTableBloatQuery("", 20)
		if len(args) != 1 || args[0] != 20 {
			t.Fatalf("args = %v, want [20] (limit only)", args)
		}
		for _, want := range []string{
			"pg_table_size",                 // the waste base
			"COALESCE(s.n_dead_tup, 0) > 0", // only tables with dead tuples
			"relkind IN ('r', 'm')",
			"ORDER BY", "DESC",
			"LIMIT $1",
		} {
			if !strings.Contains(sql, want) {
				t.Fatalf("bloat query missing %q:\n%s", want, sql)
			}
		}
		if strings.Contains(sql, "$2") {
			t.Fatalf("no-schema bloat query must only use $1:\n%s", sql)
		}
	})
	t.Run("schema filter takes $1, limit shifts to $2", func(t *testing.T) {
		sql, args := buildTableBloatQuery("public", 5)
		if len(args) != 2 || args[0] != "public" || args[1] != 5 {
			t.Fatalf("args = %v, want [public 5]", args)
		}
		if !strings.Contains(sql, "n.nspname = $1") || !strings.Contains(sql, "LIMIT $2") {
			t.Fatalf("placeholders not numbered correctly:\n%s", sql)
		}
	})
}

func TestBuildLongTxnQuery(t *testing.T) {
	t.Run("no min: oldest-first, excludes own backend, only open xacts", func(t *testing.T) {
		sql, args := buildLongTxnQuery(0)
		if len(args) != 0 {
			t.Fatalf("want no args, got %v", args)
		}
		for _, want := range []string{
			"xact_start IS NOT NULL",
			"pg_backend_pid()",
			"ORDER BY xact_start ASC",
		} {
			if !strings.Contains(sql, want) {
				t.Fatalf("long-txn query missing %q:\n%s", want, sql)
			}
		}
		if strings.Contains(sql, "make_interval") {
			t.Fatalf("no-min query should not add an interval filter:\n%s", sql)
		}
	})
	t.Run("min adds a parameterized interval filter", func(t *testing.T) {
		sql, args := buildLongTxnQuery(30)
		if len(args) != 1 || args[0] != float64(30) {
			t.Fatalf("args = %v, want [30]", args)
		}
		if !strings.Contains(sql, "make_interval(secs => $1)") {
			t.Fatalf("min filter not parameterized:\n%s", sql)
		}
	})
}

func TestBuildWraparoundQuery(t *testing.T) {
	t.Run("no schema: whole cluster, limit only", func(t *testing.T) {
		sql, args := buildWraparoundQuery("", 15)
		if len(args) != 1 || args[0] != 15 {
			t.Fatalf("args = %v, want [15]", args)
		}
		for _, want := range []string{
			"age(c.relfrozenxid)",
			"autovacuum_freeze_max_age",
			"relkind IN ('r', 'm', 't')",  // includes TOAST — wraparound is cluster-wide
			"owner.reltoastrelid = c.oid", // resolves each TOAST relation to its owning table
			"c.relfrozenxid <> 0",
			"DESC",
			"LIMIT $1",
		} {
			if !strings.Contains(sql, want) {
				t.Fatalf("wraparound query missing %q:\n%s", want, sql)
			}
		}
		// Must NOT filter out system schemas — a pg_catalog/pg_toast table can be the culprit.
		if strings.Contains(sql, "!~ '^pg_'") {
			t.Fatalf("wraparound query must not exclude system schemas:\n%s", sql)
		}
		// With no schema there is no namespace-filter predicate (the owner namespace is
		// still SELECTed to label TOAST rows; only the WHERE predicate must be absent).
		if strings.Contains(sql, "nspname = $1") {
			t.Fatalf("unfiltered query should have no schema predicate:\n%s", sql)
		}
	})

	t.Run("schema filter matches own schema OR the owning table's schema", func(t *testing.T) {
		sql, args := buildWraparoundQuery("public", 15)
		if len(args) != 2 || args[0] != "public" || args[1] != 15 {
			t.Fatalf("args = %v, want [public 15]", args)
		}
		// $1 is the schema, used for both the relation's own namespace and the TOAST
		// owner's namespace; $2 is the limit.
		for _, want := range []string{
			"n.nspname = $1 OR ons.nspname = $1",
			"LIMIT $2",
		} {
			if !strings.Contains(sql, want) {
				t.Fatalf("schema-filtered query missing %q:\n%s", want, sql)
			}
		}
	})
}

func TestBuildTableUsageQuery(t *testing.T) {
	t.Run("orders by seq_scan desc, no schema", func(t *testing.T) {
		sql, args := buildTableUsageQuery("")
		if len(args) != 0 {
			t.Fatalf("want no args, got %v", args)
		}
		for _, want := range []string{"seq_scan", "idx_scan", "n_tup_ins", "n_tup_upd", "n_tup_del",
			"ORDER BY COALESCE(s.seq_scan, 0) DESC"} {
			if !strings.Contains(sql, want) {
				t.Fatalf("usage query missing %q:\n%s", want, sql)
			}
		}
	})
	t.Run("schema filter parameterized", func(t *testing.T) {
		sql, args := buildTableUsageQuery("app")
		if len(args) != 1 || args[0] != "app" || !strings.Contains(sql, "n.nspname = $1") {
			t.Fatalf("schema filter wrong: args=%v\n%s", args, sql)
		}
	})
}

func TestBuildTableMaintenanceQuery(t *testing.T) {
	t.Run("most-stale first, no schema", func(t *testing.T) {
		sql, args := buildTableMaintenanceQuery("")
		if len(args) != 0 {
			t.Fatalf("want no args, got %v", args)
		}
		for _, want := range []string{
			"GREATEST(s.last_vacuum, s.last_autovacuum)",
			"GREATEST(s.last_analyze, s.last_autoanalyze)",
			"n_mod_since_analyze", "autovacuum_count",
			"ORDER BY COALESCE(s.n_mod_since_analyze, 0) DESC",
		} {
			if !strings.Contains(sql, want) {
				t.Fatalf("maintenance query missing %q:\n%s", want, sql)
			}
		}
	})
	t.Run("schema filter parameterized", func(t *testing.T) {
		sql, args := buildTableMaintenanceQuery("app")
		if len(args) != 1 || args[0] != "app" || !strings.Contains(sql, "n.nspname = $1") {
			t.Fatalf("schema filter wrong: args=%v\n%s", args, sql)
		}
	})
}

func TestConstraintType(t *testing.T) {
	cases := map[string]string{
		"p": "primary key", "f": "foreign key", "u": "unique",
		"c": "check", "x": "exclusion", "z": "z",
	}
	for in, want := range cases {
		if got := constraintType(in); got != want {
			t.Fatalf("constraintType(%q) = %q, want %q", in, got, want)
		}
	}
}
