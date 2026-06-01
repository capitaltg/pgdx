package explain

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	cases := []struct {
		name string
		sql  string
		want Category
	}{
		{"select", "SELECT 1", CategoryReadOnly},
		{"select lower", "select * from orders", CategoryReadOnly},
		{"values", "VALUES (1), (2)", CategoryReadOnly},
		{"table", "TABLE orders", CategoryReadOnly},
		{"show", "SHOW work_mem", CategoryReadOnly},
		{"with select", "WITH t AS (SELECT 1) SELECT * FROM t", CategoryReadOnly},
		{"with delete cte", "WITH d AS (DELETE FROM o RETURNING *) SELECT * FROM d", CategoryWriting},
		{"with update cte", "WITH u AS (UPDATE o SET x=1 RETURNING *) SELECT * FROM u", CategoryWriting},
		{"with insert cte", "WITH i AS (INSERT INTO o VALUES (1) RETURNING *) SELECT * FROM i", CategoryWriting},
		{"insert", "INSERT INTO orders VALUES (1)", CategoryWriting},
		{"update", "UPDATE orders SET total = 0", CategoryWriting},
		{"delete", "DELETE FROM orders WHERE id = 1", CategoryWriting},
		{"merge", "MERGE INTO t USING s ON t.id=s.id WHEN MATCHED THEN DELETE", CategoryWriting},
		{"create", "CREATE TABLE t (id int)", CategoryDDL},
		{"drop", "DROP TABLE orders", CategoryDDL},
		{"truncate", "TRUNCATE orders", CategoryDDL},
		{"vacuum is other", "VACUUM orders", CategoryOther},
		{"leading whitespace", "   \n\t SELECT 1", CategoryReadOnly},
		{"leading line comment", "-- a comment\nSELECT 1", CategoryReadOnly},
		{"leading block comment", "/* hi */ SELECT 1", CategoryReadOnly},
		{"stacked comments", "-- one\n /* two */\n  -- three\n DELETE FROM o", CategoryWriting},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := Classify(c.sql); got != c.want {
				t.Fatalf("Classify(%q) = %v, want %v", c.sql, got, c.want)
			}
		})
	}
}

func TestDecide_DefaultNeverExecutes(t *testing.T) {
	// CRITICAL: without --analyze, nothing runs, no matter how dangerous.
	dangerous := []string{
		"SELECT 1",
		"DELETE FROM orders",
		"DROP TABLE orders",
		"UPDATE orders SET total = 0",
		"TRUNCATE orders",
		"WITH d AS (DELETE FROM o RETURNING *) SELECT * FROM d",
		"SELECT nextval('s')",
	}
	for _, sql := range dangerous {
		t.Run(sql, func(t *testing.T) {
			d := Decide(sql, false)
			if d.Action != ActionExplainPlain {
				t.Fatalf("Decide(%q, analyze=false).Action = %v, want ActionExplainPlain", sql, d.Action)
			}
			stmts := BuildStatements(d, sql)
			if len(stmts) != 1 || !strings.HasPrefix(stmts[0], "EXPLAIN (FORMAT JSON) ") {
				t.Fatalf("plain build = %v, want single EXPLAIN (FORMAT JSON)", stmts)
			}
			if strings.Contains(stmts[0], "ANALYZE") {
				t.Fatalf("plain build must never contain ANALYZE: %v", stmts)
			}
		})
	}
}

func TestDecide_AnalyzeReadOnlyRunsDirect(t *testing.T) {
	d := Decide("SELECT * FROM orders WHERE id = 1", true)
	if d.Action != ActionAnalyzeDirect {
		t.Fatalf("Action = %v, want ActionAnalyzeDirect", d.Action)
	}
	stmts := BuildStatements(d, "SELECT 1")
	if len(stmts) != 1 || !strings.Contains(stmts[0], "ANALYZE") {
		t.Fatalf("build = %v, want single EXPLAIN ANALYZE", stmts)
	}
}

func TestDecide_AnalyzeWritesGetRollbackWrapped(t *testing.T) {
	// CRITICAL: writes under --analyze run inside BEGIN..ROLLBACK and warn.
	writes := []string{
		"INSERT INTO orders VALUES (1)",
		"UPDATE orders SET total = 0",
		"DELETE FROM orders WHERE id = 1",
		"WITH d AS (DELETE FROM o RETURNING *) SELECT * FROM d",
		"CREATE TABLE t (id int)",
		"TRUNCATE orders",
	}
	for _, sql := range writes {
		t.Run(sql, func(t *testing.T) {
			d := Decide(sql, true)
			if d.Action != ActionAnalyzeRollback {
				t.Fatalf("Decide(%q, analyze=true).Action = %v, want ActionAnalyzeRollback", sql, d.Action)
			}
			if d.Warning == "" {
				t.Fatalf("expected a warning for a write under --analyze, got none")
			}
			stmts := BuildStatements(d, sql)
			if len(stmts) != 3 || stmts[0] != "BEGIN" || stmts[2] != "ROLLBACK" {
				t.Fatalf("build = %v, want [BEGIN, EXPLAIN ANALYZE..., ROLLBACK]", stmts)
			}
		})
	}
}

func TestDecide_AnalyzeRefusesUncontainableSideEffects(t *testing.T) {
	// CRITICAL: ROLLBACK can't undo these, so refuse to execute them.
	refusals := []string{
		"SELECT pg_notify('chan', 'msg')",
		"NOTIFY my_channel",
		"SELECT nextval('order_id_seq')",
		"SELECT setval('s', 1)",
		"INSERT INTO t SELECT dblink('conn', 'SELECT 1')",
		"COPY t TO PROGRAM 'cat > /tmp/x'",
	}
	for _, sql := range refusals {
		t.Run(sql, func(t *testing.T) {
			d := Decide(sql, true)
			if d.Action != ActionRefuse {
				t.Fatalf("Decide(%q, analyze=true).Action = %v, want ActionRefuse", sql, d.Action)
			}
			if d.Reason == "" {
				t.Fatalf("refusal must carry a reason")
			}
			if got := BuildStatements(d, sql); got != nil {
				t.Fatalf("refused decision must build no statements, got %v", got)
			}
		})
	}
}

func TestDecide_RefusesAlreadyExplain(t *testing.T) {
	for _, analyze := range []bool{false, true} {
		for _, sql := range []string{
			"EXPLAIN SELECT 1",
			"explain analyze select 1",
			"EXPLAIN (FORMAT JSON) SELECT 1",
			"  /* c */ EXPLAIN SELECT 1",
		} {
			d := Decide(sql, analyze)
			if d.Action != ActionRefuse {
				t.Fatalf("Decide(%q, %v).Action = %v, want ActionRefuse", sql, analyze, d.Action)
			}
			if !strings.Contains(d.Reason, "already begins with EXPLAIN") {
				t.Fatalf("unexpected reason: %q", d.Reason)
			}
		}
	}
}

func TestDecide_ReadOnlyWithComments(t *testing.T) {
	// Comments and case must not fool the classifier into running a write directly.
	d := Decide("/* nightly */ -- cleanup\n DELETE FROM stale", true)
	if d.Action != ActionAnalyzeRollback {
		t.Fatalf("commented DELETE under --analyze = %v, want ActionAnalyzeRollback", d.Action)
	}
}

func TestHasParameters(t *testing.T) {
	cases := map[string]bool{
		"SELECT * FROM t WHERE id = $1":            true,
		"SELECT * FROM t WHERE a = $1 AND b = $12": true,
		"SELECT * FROM t WHERE name = 'literal'":   false,
		"SELECT pg_advisory_unlock($1)":            true,
		"UPDATE t SET x = 1 WHERE id = 5":          false,
	}
	for sql, want := range cases {
		if got := HasParameters(sql); got != want {
			t.Fatalf("HasParameters(%q) = %v, want %v", sql, got, want)
		}
	}
}

func TestBuildStatements_GenericPlan(t *testing.T) {
	d := Decision{Action: ActionExplainPlain, GenericPlan: true}
	stmts := BuildStatements(d, "SELECT * FROM t WHERE id = $1")
	if len(stmts) != 1 || !strings.HasPrefix(stmts[0], "EXPLAIN (GENERIC_PLAN, FORMAT JSON) ") {
		t.Fatalf("generic-plan explain wrong: %#v", stmts)
	}
	// Without the flag, a plain explain must NOT use GENERIC_PLAN.
	plain := BuildStatements(Decision{Action: ActionExplainPlain}, "SELECT 1")
	if strings.Contains(plain[0], "GENERIC_PLAN") {
		t.Fatalf("plain explain should not use GENERIC_PLAN: %q", plain[0])
	}
}

func TestMaxParamIndex(t *testing.T) {
	cases := map[string]int{
		"SELECT 1":                                 0,
		"SELECT * FROM t WHERE id = $1":            1,
		"... WHERE a = $1 AND b ILIKE $3 LIMIT $2": 3, // highest index, not the count
		"$1 $1 $1":                                 1, // repeated placeholder
	}
	for sql, want := range cases {
		if got := MaxParamIndex(sql); got != want {
			t.Fatalf("MaxParamIndex(%q) = %d, want %d", sql, got, want)
		}
	}
}
