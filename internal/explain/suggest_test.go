package explain

import (
	"strings"
	"testing"
)

func TestIndexCandidate(t *testing.T) {
	t.Run("single equality column", func(t *testing.T) {
		got := indexCandidate("orders", "(status = 'active'::text)")
		want := `CREATE INDEX CONCURRENTLY idx_orders_status ON "orders" ("status");`
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("equality columns ordered before range columns", func(t *testing.T) {
		// created_at is a range predicate; customer_id is equality. Equality must lead.
		got := indexCandidate("orders", "((created_at > '2020-01-01'::date) AND (customer_id = 42))")
		if !strings.Contains(got, `("customer_id", "created_at")`) {
			t.Fatalf("equality column should lead the index: %q", got)
		}
	})

	t.Run("unwraps an implicit cast on the column", func(t *testing.T) {
		// Postgres renders varchar/text comparisons as `(col)::text = ...`. The column
		// is `notes`, NOT the cast type `text`.
		got := indexCandidate("timesheetentry", "((notes)::text = 'zzz'::text)")
		want := `CREATE INDEX CONCURRENTLY idx_timesheetentry_notes ON "timesheetentry" ("notes");`
		if got != want {
			t.Fatalf("got %q, want %q", got, want)
		}
	})

	t.Run("strips table qualifier from column", func(t *testing.T) {
		got := indexCandidate("orders", "(orders.status = 'x'::text)")
		if !strings.Contains(got, `("status")`) || strings.Contains(got, "orders.status") {
			t.Fatalf("qualifier should be stripped: %q", got)
		}
	})

	t.Run("declines on OR (no single B-tree serves it)", func(t *testing.T) {
		if got := indexCandidate("orders", "((status = 'a'::text) OR (status = 'b'::text))"); got != "" {
			t.Fatalf("OR predicate should yield no suggestion, got %q", got)
		}
	})

	t.Run("declines on a function applied to a column", func(t *testing.T) {
		if got := indexCandidate("users", "(lower(email) = 'x'::text)"); got != "" {
			t.Fatalf("expression predicate should yield no suggestion, got %q", got)
		}
	})

	t.Run("no extractable column yields nothing", func(t *testing.T) {
		if got := indexCandidate("t", "(true)"); got != "" {
			t.Fatalf("want empty, got %q", got)
		}
	})

	t.Run("dedupes a column repeated across predicates", func(t *testing.T) {
		got := indexCandidate("t", "((id >= 1) AND (id <= 9))")
		if strings.Count(got, `"id"`) != 1 {
			t.Fatalf("column should appear once: %q", got)
		}
	})
}

func TestAddIndexSuggestions(t *testing.T) {
	d := Diagnosis{Findings: []Finding{
		{Title: "scan", rel: "orders", filter: "(status = 'x'::text)"},
		{Title: "other"}, // no rel/filter — must stay untouched
	}}
	AddIndexSuggestions(&d)
	if d.Findings[0].IndexSuggestion == "" {
		t.Fatal("finding with rel+filter should get a suggestion")
	}
	if d.Findings[1].IndexSuggestion != "" {
		t.Fatal("finding without evidence must not get a suggestion")
	}
}

// A missing-index finding from the diagnoser should carry the structured evidence
// needed for AddIndexSuggestions to produce a candidate end-to-end.
func TestDiagnoseToSuggestion_EndToEnd(t *testing.T) {
	d := diagnoseFixture(t, "seqscan_missing_index.json")
	AddIndexSuggestions(&d)
	found := false
	for _, f := range d.Findings {
		if f.IndexSuggestion != "" {
			found = true
			if !strings.HasPrefix(f.IndexSuggestion, "CREATE INDEX CONCURRENTLY") {
				t.Fatalf("unexpected suggestion shape: %q", f.IndexSuggestion)
			}
		}
	}
	if !found {
		t.Fatal("expected an index suggestion for the missing-index fixture")
	}
}
