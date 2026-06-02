package cmd

import (
	"strings"
	"testing"

	"github.com/capitaltg/pgdx/internal/catalog"
)

func TestVacuumSQL(t *testing.T) {
	const id = `"public"."orders"`
	cases := []struct {
		full, analyze bool
		want          string
	}{
		{false, false, `VACUUM "public"."orders"`},
		{false, true, `VACUUM (ANALYZE) "public"."orders"`},
		{true, false, `VACUUM (FULL) "public"."orders"`},
		{true, true, `VACUUM (FULL, ANALYZE) "public"."orders"`},
	}
	for _, c := range cases {
		if got := vacuumSQL(id, c.full, c.analyze); got != c.want {
			t.Fatalf("vacuumSQL(full=%v,analyze=%v) = %q, want %q", c.full, c.analyze, got, c.want)
		}
	}
}

func TestQuoteIdent(t *testing.T) {
	cases := map[string]string{
		"orders":     `"orders"`,
		"MyTable":    `"MyTable"`,
		`we"ird`:     `"we""ird"`,
		`a"; DROP--`: `"a""; DROP--"`, // injection attempt is neutralized by doubling quotes
	}
	for in, want := range cases {
		if got := quoteIdent(in); got != want {
			t.Fatalf("quoteIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestReportVacuumOutcome(t *testing.T) {
	mb := int64(1) << 20
	cases := []struct {
		name          string
		before, after catalog.TableStats
		full          bool
		wantContains  []string
		wantOmits     []string
	}{
		{
			name:         "plain: reclaimed some",
			before:       catalog.TableStats{DeadTup: 20000, SizeBytes: 3 * mb},
			after:        catalog.TableStats{DeadTup: 0, SizeBytes: 3 * mb},
			wantContains: []string{"reclaimed 20,000 dead tuples", "0 remaining"},
			wantOmits:    []string{"none were removable", "freed"},
		},
		{
			name:         "plain: nothing removable -> hint",
			before:       catalog.TableStats{DeadTup: 5000, SizeBytes: mb},
			after:        catalog.TableStats{DeadTup: 5000, SizeBytes: mb},
			wantContains: []string{"none were removable", "get transaction-age"},
			wantOmits:    []string{"reclaimed"},
		},
		{
			name:         "full: trust size, never the pinned hint",
			before:       catalog.TableStats{DeadTup: 19886, SizeBytes: 3 * mb},
			after:        catalog.TableStats{DeadTup: 19886, SizeBytes: mb},
			full:         true,
			wantContains: []string{"rewrote the table", "freed"},
			wantOmits:    []string{"none were removable", "transaction-age"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var b strings.Builder
			reportVacuumOutcome(&b, tc.before, tc.after, tc.full)
			out := b.String()
			for _, s := range tc.wantContains {
				if !strings.Contains(out, s) {
					t.Errorf("output %q missing %q", out, s)
				}
			}
			for _, s := range tc.wantOmits {
				if strings.Contains(out, s) {
					t.Errorf("output %q should not contain %q", out, s)
				}
			}
		})
	}
}
