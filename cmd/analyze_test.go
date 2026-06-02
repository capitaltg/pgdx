package cmd

import (
	"io"
	"strings"
	"testing"
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
