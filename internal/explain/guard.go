// Package explain implements pgdx's flagship command: it turns a query into a
// plain-language diagnosis. This file is the SAFETY CORE.
//
// Decision D1 (eng review): pgdx executes NOTHING by default. `EXPLAIN` only
// describes a plan; `EXPLAIN ANALYZE` actually RUNS the statement. So:
//
//	          ┌─────────────── Decide(sql, analyze) ───────────────┐
//	          │                                                     │
//	analyze=false ──────────────────────────────► ExplainPlain     │  (never executes)
//	          │                                                     │
//	analyze=true                                                    │
//	   ├─ input already starts with EXPLAIN ──────► Refuse          │  (usability)
//	   ├─ uncontainable side effect (NOTIFY,                        │
//	   │   nextval, COPY..PROGRAM, dblink) ───────► Refuse          │  (safety)
//	   ├─ read-only (SELECT/VALUES/TABLE/                           │
//	   │   WITH..SELECT with no DML CTE) ─────────► AnalyzeDirect   │
//	   └─ writes (INSERT/UPDATE/DELETE/MERGE,                       │
//	       DDL, data-modifying CTE) ─────────────► AnalyzeRollback  │  (BEGIN..ROLLBACK + warn)
//	          └─────────────────────────────────────────────────────┘
//
// The classification is intentionally CONSERVATIVE: when in doubt, treat a
// statement as writing. Over-classifying a read as a write only costs a wrapped
// transaction (safe); under-classifying a write as a read could mutate prod data.
package explain

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Category is the coarse kind of a SQL statement, as far as execution safety goes.
type Category int

const (
	CategoryReadOnly Category = iota // SELECT, VALUES, TABLE, SHOW, WITH..SELECT (no DML CTE)
	CategoryWriting                  // INSERT, UPDATE, DELETE, MERGE, or a data-modifying CTE
	CategoryDDL                      // CREATE, ALTER, DROP, TRUNCATE, ...
	CategoryOther                    // anything else (VACUUM, CALL, SET, ...) — treated as unsafe to execute
)

func (c Category) String() string {
	switch c {
	case CategoryReadOnly:
		return "read-only"
	case CategoryWriting:
		return "writing"
	case CategoryDDL:
		return "ddl"
	default:
		return "other"
	}
}

// Action is what pgdx will do with the statement.
type Action int

const (
	ActionExplainPlain    Action = iota // EXPLAIN (FORMAT JSON) <stmt> — no execution
	ActionAnalyzeDirect                 // EXPLAIN (ANALYZE, ...) <stmt> — executes, read-only
	ActionAnalyzeRollback               // BEGIN; EXPLAIN (ANALYZE, ...) <stmt>; ROLLBACK
	ActionRefuse                        // cannot run safely (or usability refusal)
)

func (a Action) String() string {
	switch a {
	case ActionExplainPlain:
		return "explain-plain"
	case ActionAnalyzeDirect:
		return "analyze-direct"
	case ActionAnalyzeRollback:
		return "analyze-rollback"
	default:
		return "refuse"
	}
}

// Decision is the full result of evaluating a statement under the D1 policy.
type Decision struct {
	Category Category
	Action   Action
	// Warning is shown to the user (stderr) when non-empty — e.g. "this will run
	// inside a rolled-back transaction".
	Warning string
	// Reason explains a Refuse. Empty otherwise.
	Reason string
	// GenericPlan asks for EXPLAIN (GENERIC_PLAN) on a plain explain — used for a
	// parameterized query ($1, …) pulled from a running backend, where no values are
	// bound to substitute. GENERIC_PLAN never executes, so it's always plan-only-safe.
	// Requires PostgreSQL 16+. Only meaningful with ActionExplainPlain.
	GenericPlan bool
}

var (
	// Leading SQL comment / whitespace stripping.
	reLineComment  = regexp.MustCompile(`^--[^\n]*`)
	reBlockComment = regexp.MustCompile(`(?s)^/\*.*?\*/`)

	// A leading EXPLAIN with an optional ( ... ) options group.
	reLeadingExplain = regexp.MustCompile(`(?is)^EXPLAIN\b`)

	// Data-modifying keywords anywhere in the statement (used for WITH detection).
	reDML = regexp.MustCompile(`(?i)\b(INSERT|UPDATE|DELETE|MERGE)\b`)

	// A positional parameter placeholder ($1, $2, …) as Postgres records prepared /
	// parameterized statements in pg_stat_activity. Such a query can't be planned with
	// a plain EXPLAIN (no values to bind) — it needs EXPLAIN (GENERIC_PLAN), PG16+.
	reParams = regexp.MustCompile(`\$\d+`)

	// Uncontainable side effects: a ROLLBACK does NOT undo these, so refuse to
	// execute them even wrapped in a transaction.
	reSideEffects = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\bNOTIFY\b`),       // NOTIFY chan
		regexp.MustCompile(`(?i)\bpg_notify\s*\(`), // pg_notify('chan', 'msg') — '_' blocks the \bNOTIFY\b boundary
		regexp.MustCompile(`(?i)\bnextval\s*\(`),
		regexp.MustCompile(`(?i)\bsetval\s*\(`),
		regexp.MustCompile(`(?i)\bdblink(_exec)?\s*\(`),
		regexp.MustCompile(`(?is)\bCOPY\b.*\bPROGRAM\b`),
	}
)

// stripLeading removes leading whitespace and SQL comments, repeatedly, until
// the first real token is exposed.
func stripLeading(sql string) string {
	for {
		trimmed := strings.TrimLeftFunc(sql, isSpace)
		if loc := reLineComment.FindString(trimmed); loc != "" {
			sql = trimmed[len(loc):]
			continue
		}
		if loc := reBlockComment.FindString(trimmed); loc != "" {
			sql = trimmed[len(loc):]
			continue
		}
		return trimmed
	}
}

func isSpace(r rune) bool {
	return r == ' ' || r == '\t' || r == '\n' || r == '\r' || r == '\v' || r == '\f'
}

// firstKeyword returns the upper-cased leading word of a (pre-stripped) statement.
func firstKeyword(s string) string {
	end := strings.IndexFunc(s, func(r rune) bool {
		return !((r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || r == '_')
	})
	if end < 0 {
		end = len(s)
	}
	return strings.ToUpper(s[:end])
}

// Classify determines the safety category of a statement. It strips leading
// comments/whitespace and is case-insensitive. WITH statements are inspected for
// data-modifying CTEs and conservatively treated as writing if any are found.
func Classify(sql string) Category {
	s := stripLeading(sql)
	switch firstKeyword(s) {
	case "SELECT", "VALUES", "TABLE", "SHOW":
		return CategoryReadOnly
	case "WITH":
		// A WITH can hide a data-modifying CTE: WITH x AS (DELETE ...) SELECT ...
		// Conservative: any DML keyword present => treat the whole thing as writing.
		if reDML.MatchString(s) {
			return CategoryWriting
		}
		return CategoryReadOnly
	case "INSERT", "UPDATE", "DELETE", "MERGE":
		return CategoryWriting
	case "CREATE", "ALTER", "DROP", "TRUNCATE", "GRANT", "REVOKE",
		"COMMENT", "REINDEX", "CLUSTER", "REFRESH":
		return CategoryDDL
	default:
		return CategoryOther
	}
}

// hasLeadingExplain reports whether the input already begins with EXPLAIN.
func hasLeadingExplain(sql string) bool {
	return reLeadingExplain.MatchString(stripLeading(sql))
}

// HasParameters reports whether the statement contains positional placeholders
// ($1, $2, …) — true for most prepared/JDBC statements pulled from pg_stat_activity.
func HasParameters(sql string) bool {
	return reParams.MatchString(sql)
}

// reParamIdx captures the number of a positional placeholder ($1, $2, …).
var reParamIdx = regexp.MustCompile(`\$(\d+)`)

// MaxParamIndex returns the highest placeholder number used in the statement (0 if
// none) — i.e. how many values it expects, so callers can validate a supplied set.
func MaxParamIndex(sql string) int {
	max := 0
	for _, m := range reParamIdx.FindAllStringSubmatch(sql, -1) {
		if n, err := strconv.Atoi(m[1]); err == nil && n > max {
			max = n
		}
	}
	return max
}

// hasUncontainableSideEffect reports whether running the statement could cause
// effects a ROLLBACK won't undo (sequence advances, NOTIFY, external programs).
func hasUncontainableSideEffect(sql string) bool {
	for _, re := range reSideEffects {
		if re.MatchString(sql) {
			return true
		}
	}
	return false
}

// Decide applies the D1 policy to a raw statement.
func Decide(sql string, analyze bool) Decision {
	cat := Classify(sql)

	if !analyze {
		// Default path: describe the plan, never run the statement.
		if hasLeadingExplain(sql) {
			return Decision{
				Category: cat,
				Action:   ActionRefuse,
				Reason:   "input already begins with EXPLAIN — pass just the query and pgdx will add EXPLAIN itself",
			}
		}
		return Decision{Category: cat, Action: ActionExplainPlain}
	}

	// --analyze: the statement WILL run. Guard hard.
	if hasLeadingExplain(sql) {
		return Decision{
			Category: cat,
			Action:   ActionRefuse,
			Reason:   "input already begins with EXPLAIN — pass just the query and pgdx will add EXPLAIN itself",
		}
	}
	if hasUncontainableSideEffect(sql) {
		return Decision{
			Category: cat,
			Action:   ActionRefuse,
			Reason:   "statement has side effects a ROLLBACK cannot undo (e.g. NOTIFY, nextval/setval, COPY..PROGRAM, dblink); refusing to execute it. Run without --analyze for a plan-only EXPLAIN.",
		}
	}
	if cat == CategoryReadOnly {
		return Decision{Category: cat, Action: ActionAnalyzeDirect}
	}
	// Writing / DDL / Other: execute inside a transaction that is always rolled back.
	return Decision{
		Category: cat,
		Action:   ActionAnalyzeRollback,
		Warning:  fmt.Sprintf("statement is %s; --analyze will EXECUTE it inside BEGIN..ROLLBACK so changes are discarded, but verify this is what you want", cat),
	}
}

// BuildStatements returns the SQL statements pgdx should send to the server for a
// given decision, in order. The caller (db layer) is responsible for guaranteeing
// the trailing ROLLBACK runs even if the EXPLAIN errors.
//
// A Refuse decision yields no statements (callers must check d.Action first).
func BuildStatements(d Decision, sql string) []string {
	const (
		plain   = "EXPLAIN (FORMAT JSON) "
		analyze = "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "
	)
	switch d.Action {
	case ActionExplainPlain:
		if d.GenericPlan {
			// Plan a parameterized query without bound values (PG16+). Never executes.
			return []string{"EXPLAIN (GENERIC_PLAN, FORMAT JSON) " + sql}
		}
		return []string{plain + sql}
	case ActionAnalyzeDirect:
		return []string{analyze + sql}
	case ActionAnalyzeRollback:
		return []string{"BEGIN", analyze + sql, "ROLLBACK"}
	default: // ActionRefuse
		return nil
	}
}
