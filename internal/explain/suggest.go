package explain

import (
	"fmt"
	"regexp"
	"strings"
)

// AddIndexSuggestions fills in Finding.IndexSuggestion for every finding that
// carries enough structured evidence (a relation + a filter) to synthesize a
// candidate index. Findings where columns can't be extracted with confidence are
// left untouched — pgdx would rather say nothing than recommend a wrong index.
//
// This is opt-in (the `explain --suggest-index` flag) precisely because the result
// is a STARTING POINT: index choice depends on the whole workload, cardinality, and
// existing indexes, none of which a single plan reveals. The textual Suggestion
// already names the columns; this just saves the user from hand-writing the DDL.
func AddIndexSuggestions(d *Diagnosis) {
	for i := range d.Findings {
		f := &d.Findings[i]
		if f.rel == "" || f.filter == "" {
			continue
		}
		if stmt := indexCandidate(f.rel, f.filter); stmt != "" {
			f.IndexSuggestion = stmt
		}
	}
}

// reFilterPredicate matches one "<column> <op> ..." predicate in an EXPLAIN filter
// string. It captures the bare column name and the operator class so equality columns
// can be ordered ahead of range columns (B-tree best practice: equality first, then
// the leading range column).
//
// It understands the two ways Postgres renders a column on the left of a comparison:
//
//	status = 'active'::text          -- bare
//	(notes)::text = 'active'::text   -- implicit cast, parenthesized (very common)
//
// so it strips an optional surrounding paren, an optional table qualifier, and an
// optional `::type` cast (type names can contain spaces, e.g. "character varying").
var reFilterPredicate = regexp.MustCompile(`(?i)\(?\b(?:[a-z_][a-z0-9_]*\.)?([a-z_][a-z0-9_]*)\)?(?:::[a-z0-9_ ]+?)?\s*(=|<>|!=|<=|>=|<|>)\s`)

// reFuncOnColumn detects a function/expression applied to what would otherwise be a
// column reference (e.g. `lower(email) = ...`). When present, a plain column index
// won't help — it needs an expression index — so we decline rather than mislead.
// The paren must be directly attached to the name (a real call), so we don't trip on
// a keyword followed by a parenthesized group like `AND (...)`.
var reFuncOnColumn = regexp.MustCompile(`(?i)\b[a-z_][a-z0-9_]*\(`)

// indexCandidate synthesizes a `CREATE INDEX CONCURRENTLY` statement from a relation
// name and an EXPLAIN filter expression, or returns "" when it can't do so with
// confidence. Conservative by design: it bails on OR logic and on functions applied
// to columns, both of which a naive single-column-list B-tree index would not serve.
func indexCandidate(rel, filter string) string {
	// OR across predicates generally can't be served by one B-tree index. Decline.
	if regexp.MustCompile(`(?i)\bOR\b`).MatchString(filter) {
		return ""
	}
	// An expression like lower(col) needs an expression index, not a column index.
	if reFuncOnColumn.MatchString(filter) {
		return ""
	}

	var eqCols, rangeCols []string
	seen := map[string]bool{}
	for _, m := range reFilterPredicate.FindAllStringSubmatch(filter, -1) {
		col, op := m[1], m[2]
		if seen[col] {
			continue
		}
		seen[col] = true
		if op == "=" {
			eqCols = append(eqCols, col)
		} else {
			rangeCols = append(rangeCols, col)
		}
	}

	cols := append(eqCols, rangeCols...) // equality columns lead, then ranges
	if len(cols) == 0 {
		return ""
	}

	name := "idx_" + sanitizeIdent(rel) + "_" + strings.Join(mapSanitize(cols), "_")
	return fmt.Sprintf("CREATE INDEX CONCURRENTLY %s ON %s (%s);",
		name, quoteIdent(rel), strings.Join(quoteIdents(cols), ", "))
}

// sanitizeIdent reduces an identifier to a name-fragment safe form (for building the
// generated index name); it is NOT used for the quoted identifiers in the DDL itself.
func sanitizeIdent(s string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func mapSanitize(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = sanitizeIdent(s)
	}
	return out
}

// quoteIdent double-quotes an identifier so mixed-case or reserved names survive.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

func quoteIdents(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = quoteIdent(s)
	}
	return out
}
