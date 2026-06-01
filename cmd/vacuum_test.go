package cmd

import "testing"

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
