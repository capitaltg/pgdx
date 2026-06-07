package cmd

import (
	"io"
	"strings"
	"testing"

	"github.com/capitaltg/pgdx/internal/catalog"
)

func TestReportAnalyzeRows(t *testing.T) {
	cases := []struct {
		name          string
		before, after int64
		want          string
	}{
		{"first analyze of a never-analyzed table", -1, 1000, "now 1,000 (was unknown"},
		{"estimate changed", 800, 1000, "800 → 1,000"},
		{"estimate unchanged", 1000, 1000, "1,000 (unchanged)"},
		{"no estimate after (skip)", 1000, -1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			reportAnalyzeRows(&b, tc.before, tc.after)
			got := b.String()
			if tc.want == "" {
				if got != "" {
					t.Fatalf("reportAnalyzeRows(%d,%d) = %q, want empty", tc.before, tc.after, got)
				}
				return
			}
			if !strings.Contains(got, tc.want) {
				t.Fatalf("reportAnalyzeRows(%d,%d) = %q, want substring %q", tc.before, tc.after, got, tc.want)
			}
		})
	}
}

// TestAnalyzeTargetValidation checks the "exactly one target" guard, which runs before any
// connection — so a bare `analyze` (or conflicting targets) is rejected without touching a
// database. This is the safety rail that keeps whole-database analyze deliberate (--all).
func TestAnalyzeTargetValidation(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no target errors (no accidental whole-db)", []string{}, "specify a table"},
		{"table plus --all conflicts", []string{"orders", "--all"}, "only one"},
		{"--schema plus --all conflicts", []string{"--schema", "app", "--all"}, "only one"},
		{"columns with --schema rejected", []string{"--schema", "app", "orders", "id"}, "only one"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := newAnalyzeCmd()
			c.SetArgs(tc.args)
			c.SetOut(io.Discard)
			c.SetErr(io.Discard)
			err := c.Execute()
			if err == nil {
				t.Fatalf("Execute(%v) = nil error, want one containing %q", tc.args, tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("Execute(%v) err = %q, want substring %q", tc.args, err.Error(), tc.wantErr)
			}
		})
	}
}

func TestReportAnalyzeMulti(t *testing.T) {
	mk := func(s, n string, est int64) catalog.Table {
		return catalog.Table{Schema: s, Name: n, EstRows: est}
	}
	before := []catalog.Table{
		mk("public", "orders", -1),    // newly populated
		mk("public", "events", 1000),  // big move
		mk("public", "users", 1000),   // tiny move (noise)
		mk("public", "settings", 500), // unchanged
	}
	after := []catalog.Table{
		mk("public", "orders", 50000),
		mk("public", "events", 12500),
		mk("public", "users", 1010),
		mk("public", "settings", 500),
	}

	var b strings.Builder
	reportAnalyzeMulti(&b, before, after, false)
	got := b.String()
	for _, want := range []string{"notable changes:", "public.orders", "public.events", "2 other tables unchanged"} {
		if !strings.Contains(got, want) {
			t.Fatalf("default output missing %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "public.users") || strings.Contains(got, "public.settings") {
		t.Fatalf("default output should not list noise/unchanged tables:\n%s", got)
	}

	b.Reset()
	reportAnalyzeMulti(&b, before, after, true)
	got = b.String()
	for _, want := range []string{"row estimates:", "public.orders", "public.events", "public.users", "public.settings", "(unchanged)"} {
		if !strings.Contains(got, want) {
			t.Fatalf("verbose output missing %q:\n%s", want, got)
		}
	}
}
