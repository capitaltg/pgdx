package cmd

import (
	"strings"
	"testing"

	"github.com/capitaltg/pgdx/internal/catalog"
)

func TestWithThousands(t *testing.T) {
	cases := map[int64]string{
		0:       "0",
		7:       "7",
		100:     "100",
		1000:    "1,000",
		12345:   "12,345",
		500000:  "500,000",
		1234567: "1,234,567",
		1000000: "1,000,000",
	}
	for in, want := range cases {
		if got := withThousands(in); got != want {
			t.Fatalf("withThousands(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestTableRows(t *testing.T) {
	cases := []struct {
		name string
		row  catalog.Table
		want string
	}{
		{"analyzed: planner estimate", catalog.Table{EstRows: 1000, LiveTup: 1200}, "1,000"},
		{"analyzed zero rows", catalog.Table{EstRows: 0, LiveTup: 0}, "0"},
		{"never analyzed: fall back to live tuples (~)", catalog.Table{EstRows: -1, LiveTup: 1000}, "~1,000"},
		{"no stats at all", catalog.Table{EstRows: -1, LiveTup: 0}, "—"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tableRows(tc.row); got != tc.want {
				t.Fatalf("tableRows(%+v) = %q, want %q", tc.row, got, tc.want)
			}
		})
	}
}

func TestAgoHuman(t *testing.T) {
	cases := []struct {
		name string
		secs float64
		want string
	}{
		{"never happened", -1, "never"},
		{"under a minute", 12, "just now"},
		{"minutes", 5 * 60, "5m ago"},
		{"hours", 3 * 3600, "3h ago"},
		{"days", 2 * 86400, "2d ago"},
		{"weeks", 21 * 86400, "3w ago"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agoHuman(tc.secs); got != tc.want {
				t.Fatalf("agoHuman(%v) = %q, want %q", tc.secs, got, tc.want)
			}
		})
	}
}

func TestTableMaintenanceView(t *testing.T) {
	rows := tableMaintenanceView{
		{Schema: "public", Name: "orders", LiveTup: 900, DeadTup: 100,
			VacuumAgeSec: 3 * 86400, AnalyzeAgeSec: -1, ModsSinceAnalyze: 12345, AutovacuumCount: 47},
	}.Rows()
	got := rows[0]
	// SCHEMA NAME DEAD% LAST-VACUUM LAST-ANALYZE MODS-SINCE-ANALYZE AUTOVAC
	want := []string{"public", "orders", "10%", "3d ago", "never", "12,345", "47"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("column %d = %q, want %q (row %v)", i, got[i], want[i], got)
		}
	}
}

func TestTablespacesView(t *testing.T) {
	rows := tablespacesView{
		{Name: "pg_default", Owner: "postgres", SizeBytes: 136 << 20, Location: ""},
		{Name: "fast_ssd", Owner: "app", SizeBytes: -1, Location: "/mnt/ssd/pg"},
	}.Rows()
	// Built-in: humanized size, blank location → "(data directory)".
	if got := rows[0][3]; got != "(data directory)" {
		t.Fatalf("built-in LOCATION = %q, want \"(data directory)\"", got)
	}
	// Unprivileged size → "—"; real location passes through.
	if got := rows[1][2]; got != "—" {
		t.Fatalf("unprivileged SIZE = %q, want \"—\"", got)
	}
	if got := rows[1][3]; got != "/mnt/ssd/pg" {
		t.Fatalf("LOCATION = %q, want \"/mnt/ssd/pg\"", got)
	}
}

func TestActivityView_FullQuery(t *testing.T) {
	// A query longer than the 60-rune table cap, with newlines to flatten.
	long := "SELECT a, b, c\nFROM some_table\nWHERE col = 'a-fairly-long-literal-value' AND other_col > 100 ORDER BY a"
	rows := []catalog.Activity{{PID: 1, Query: long}}
	const queryCol = 8 // QUERY is the last column

	t.Run("default truncates with an ellipsis", func(t *testing.T) {
		got := activityView{rows: rows}.Rows()[0][queryCol]
		if !strings.HasSuffix(got, "…") {
			t.Fatalf("default QUERY should be truncated with …, got %q", got)
		}
		if len([]rune(got)) > 60 {
			t.Fatalf("default QUERY should be capped at 60 runes, got %d", len([]rune(got)))
		}
	})

	t.Run("--full shows the whole query, flattened to one line", func(t *testing.T) {
		got := activityView{rows: rows, full: true}.Rows()[0][queryCol]
		if strings.Contains(got, "…") {
			t.Fatalf("--full QUERY must not be truncated, got %q", got)
		}
		if strings.ContainsAny(got, "\n\r") {
			t.Fatalf("--full QUERY should be one line (no newlines), got %q", got)
		}
		if !strings.Contains(got, "ORDER BY a") {
			t.Fatalf("--full QUERY should contain the tail of the statement, got %q", got)
		}
	})
}
