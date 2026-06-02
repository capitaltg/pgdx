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
