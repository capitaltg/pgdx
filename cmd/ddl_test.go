package cmd

import (
	"strings"
	"testing"

	"github.com/capitaltg/pgdx/internal/catalog"
)

func TestRenderTableDDL(t *testing.T) {
	d := &catalog.TableDetail{
		Schema: "public",
		Name:   "orders",
		Columns: []catalog.Column{
			{Name: "id", Type: "bigint", Nullable: false, Default: "nextval('orders_id_seq'::regclass)"},
			{Name: "customer_id", Type: "bigint", Nullable: false},
			{Name: "email", Type: "character varying(255)", Nullable: true},
			{Name: "total", Type: "numeric(10,2)", Nullable: true},
		},
		Constraints: []catalog.Constraint{
			// Deliberately out of conventional order to exercise the sort.
			{Name: "orders_customer_id_fkey", Type: "foreign key", Definition: "FOREIGN KEY (customer_id) REFERENCES customers(id)"},
			{Name: "orders_total_check", Type: "check", Definition: "CHECK (total >= 0::numeric)"},
			{Name: "orders_pkey", Type: "primary key", Definition: "PRIMARY KEY (id)"},
			{Name: "orders_email_key", Type: "unique", Definition: "UNIQUE (email)"},
		},
		Indexes: []catalog.Index{
			// Backs the PK constraint — must be skipped (the constraint creates it).
			{Name: "orders_pkey", Definition: "CREATE UNIQUE INDEX orders_pkey ON public.orders USING btree (id)"},
			// Backs the unique constraint — must be skipped.
			{Name: "orders_email_key", Definition: "CREATE UNIQUE INDEX orders_email_key ON public.orders USING btree (email)"},
			// A standalone index — must be emitted.
			{Name: "orders_customer_id_idx", Definition: "CREATE INDEX orders_customer_id_idx ON public.orders USING btree (customer_id)"},
		},
	}

	var b strings.Builder
	if err := renderTableDDL(&b, d); err != nil {
		t.Fatalf("renderTableDDL: %v", err)
	}
	out := b.String()

	for _, w := range []string{
		"CREATE TABLE public.orders (",
		"    id bigint NOT NULL DEFAULT nextval('orders_id_seq'::regclass)",
		"    customer_id bigint NOT NULL",
		"    email character varying(255)",
		"    CONSTRAINT orders_pkey PRIMARY KEY (id)",
		"    CONSTRAINT orders_email_key UNIQUE (email)",
		"    CONSTRAINT orders_total_check CHECK (total >= 0::numeric)",
		"    CONSTRAINT orders_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES customers(id)",
		"CREATE INDEX orders_customer_id_idx ON public.orders USING btree (customer_id);",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("DDL output missing line:\n  %q\nfull output:\n%s", w, out)
		}
	}

	// Constraints come in conventional order: PK, unique, check, FK.
	if !ordered(out, "PRIMARY KEY", "UNIQUE", "CHECK", "FOREIGN KEY") {
		t.Errorf("constraints out of conventional order:\n%s", out)
	}

	// Backing indexes for PK/unique must not be re-emitted as CREATE [UNIQUE] INDEX.
	if strings.Contains(out, "CREATE UNIQUE INDEX orders_pkey") ||
		strings.Contains(out, "CREATE UNIQUE INDEX orders_email_key") {
		t.Errorf("constraint-backing indexes should be skipped:\n%s", out)
	}

	// For a single table, FKs are inline (no trailing ALTER TABLE).
	if strings.Contains(out, "ALTER TABLE") {
		t.Errorf("single-table DDL should inline FKs, not defer to ALTER TABLE:\n%s", out)
	}
}

func TestRenderTableDDLPartitionedParent(t *testing.T) {
	d := &catalog.TableDetail{
		Schema: "public",
		Name:   "events",
		Columns: []catalog.Column{
			{Name: "id", Type: "bigint", Nullable: false},
			{Name: "created_at", Type: "timestamp with time zone", Nullable: false},
		},
		Partition: &catalog.PartitionInfo{IsPartitioned: true, Key: "RANGE (created_at)"},
	}
	var b strings.Builder
	if err := renderTableDDL(&b, d); err != nil {
		t.Fatalf("renderTableDDL: %v", err)
	}
	if got := b.String(); !strings.Contains(got, ") PARTITION BY RANGE (created_at);") {
		t.Errorf("partitioned parent should carry PARTITION BY clause, got:\n%s", got)
	}
}

func TestRenderSchemaDDLDefersForeignKeys(t *testing.T) {
	details := []*catalog.TableDetail{
		{
			Schema:  "public",
			Name:    "orders",
			Columns: []catalog.Column{{Name: "id", Type: "bigint", Nullable: false}, {Name: "customer_id", Type: "bigint", Nullable: false}},
			Constraints: []catalog.Constraint{
				{Name: "orders_pkey", Type: "primary key", Definition: "PRIMARY KEY (id)"},
				{Name: "orders_customer_id_fkey", Type: "foreign key", Definition: "FOREIGN KEY (customer_id) REFERENCES customers(id)"},
			},
		},
		{
			Schema:      "public",
			Name:        "customers",
			Columns:     []catalog.Column{{Name: "id", Type: "bigint", Nullable: false}},
			Constraints: []catalog.Constraint{{Name: "customers_pkey", Type: "primary key", Definition: "PRIMARY KEY (id)"}},
		},
	}
	var b strings.Builder
	if err := renderSchemaDDL(&b, details); err != nil {
		t.Fatalf("renderSchemaDDL: %v", err)
	}
	out := b.String()

	if !strings.Contains(out, "CREATE TABLE public.orders (") || !strings.Contains(out, "CREATE TABLE public.customers (") {
		t.Errorf("schema DDL missing a CREATE TABLE:\n%s", out)
	}
	// The FK must be deferred to a trailing ALTER TABLE, not inline in CREATE TABLE.
	wantFK := "ALTER TABLE public.orders ADD CONSTRAINT orders_customer_id_fkey FOREIGN KEY (customer_id) REFERENCES customers(id);"
	if !strings.Contains(out, wantFK) {
		t.Errorf("schema DDL should defer FK to ALTER TABLE, missing:\n  %q\nfull:\n%s", wantFK, out)
	}
	// The deferred FK must come after every CREATE TABLE so the script replays in order.
	if !ordered(out, "CREATE TABLE public.orders", "CREATE TABLE public.customers", "ALTER TABLE public.orders ADD CONSTRAINT") {
		t.Errorf("deferred FK should follow all CREATE TABLE statements:\n%s", out)
	}
}

func TestDDLIdentQuoting(t *testing.T) {
	cases := map[string]string{
		"orders":      "orders",       // plain — bare
		"customer_id": "customer_id",  // plain — bare
		"user":        `"user"`,       // reserved — quoted
		"order":       `"order"`,      // reserved — quoted
		"MixedCase":   `"MixedCase"`,  // mixed case — quoted
		"weird name":  `"weird name"`, // space — quoted
		`has"quote`:   `"has""quote"`, // embedded quote doubled
		"_underscore": "_underscore",  // leading underscore is a legal plain start
	}
	for in, want := range cases {
		if got := ddlIdent(in); got != want {
			t.Errorf("ddlIdent(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRenderViewDDL(t *testing.T) {
	view := &catalog.ViewDetail{
		Schema: "public", Name: "active_users", Type: "view",
		Definition: " SELECT id, name\n   FROM users\n  WHERE active;",
	}
	var b strings.Builder
	if err := renderViewDDL(&b, view); err != nil {
		t.Fatalf("renderViewDDL: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "CREATE OR REPLACE VIEW public.active_users AS") {
		t.Errorf("view DDL missing CREATE VIEW header:\n%s", out)
	}
	if !strings.Contains(out, "WHERE active;") || strings.Contains(out, "active;;") {
		t.Errorf("view body should be wrapped with exactly one trailing semicolon:\n%s", out)
	}

	// Materialized, not yet populated -> WITH NO DATA.
	mat := &catalog.ViewDetail{
		Schema: "public", Name: "daily_totals", Type: "materialized", Populated: false,
		Definition: "SELECT count(*) FROM orders",
	}
	var bm strings.Builder
	if err := renderViewDDL(&bm, mat); err != nil {
		t.Fatalf("renderViewDDL(mat): %v", err)
	}
	mout := bm.String()
	if !strings.Contains(mout, "CREATE MATERIALIZED VIEW public.daily_totals AS") || !strings.Contains(mout, "WITH NO DATA;") {
		t.Errorf("unpopulated matview should emit WITH NO DATA:\n%s", mout)
	}
}

func TestRenderIndexDDL(t *testing.T) {
	d := &catalog.IndexDetail{
		Definition: "CREATE INDEX orders_customer_id_idx ON public.orders USING btree (customer_id)",
	}
	var b strings.Builder
	if err := renderIndexDDL(&b, d); err != nil {
		t.Fatalf("renderIndexDDL: %v", err)
	}
	if got := b.String(); got != "CREATE INDEX orders_customer_id_idx ON public.orders USING btree (customer_id);\n" {
		t.Errorf("index DDL = %q", got)
	}
}

func TestRenderSequenceDDL(t *testing.T) {
	d := &catalog.SequenceDetail{
		Schema: "public", Name: "orders_id_seq", DataType: "bigint",
		Start: 1, Min: 1, Max: 9223372036854775807, Increment: 1, Cache: 1, Cycle: false,
	}
	var b strings.Builder
	if err := renderSequenceDDL(&b, d); err != nil {
		t.Fatalf("renderSequenceDDL: %v", err)
	}
	out := b.String()
	for _, w := range []string{
		"CREATE SEQUENCE public.orders_id_seq",
		"    AS bigint",
		"    INCREMENT BY 1",
		"    START WITH 1",
		"    CACHE 1;",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("sequence DDL missing %q:\n%s", w, out)
		}
	}
	if strings.Contains(out, "CYCLE") {
		t.Errorf("non-cycling sequence should not emit CYCLE:\n%s", out)
	}

	// Cycling sequence emits CYCLE before the terminating semicolon.
	dc := *d
	dc.Cycle = true
	var bc strings.Builder
	if err := renderSequenceDDL(&bc, &dc); err != nil {
		t.Fatalf("renderSequenceDDL(cycle): %v", err)
	}
	if !strings.Contains(bc.String(), "CYCLE;") {
		t.Errorf("cycling sequence should emit CYCLE:\n%s", bc.String())
	}
}

func TestRenderFunctionDDL(t *testing.T) {
	d := &catalog.FunctionDetail{
		Name: "add",
		Overloads: []catalog.FunctionOverload{
			{Schema: "public", Name: "add", Kind: "func", Args: "a integer, b integer",
				Definition: "CREATE OR REPLACE FUNCTION public.add(a integer, b integer)\n RETURNS integer\n LANGUAGE sql\nAS $function$ SELECT a + b $function$\n"},
			// An aggregate: no pg_get_functiondef -> commented note, not SQL.
			{Schema: "public", Name: "add", Kind: "agg", Args: "integer", Definition: ""},
		},
	}
	var b strings.Builder
	if err := renderFunctionDDL(&b, d); err != nil {
		t.Fatalf("renderFunctionDDL: %v", err)
	}
	out := b.String()
	if !strings.Contains(out, "CREATE OR REPLACE FUNCTION public.add(a integer, b integer)") {
		t.Errorf("function DDL missing CREATE FUNCTION:\n%s", out)
	}
	if !strings.Contains(out, "$function$;") {
		t.Errorf("function DDL should terminate with a semicolon:\n%s", out)
	}
	if !strings.Contains(out, "-- agg public.add(integer): DDL not available") {
		t.Errorf("aggregate overload should be noted, not emitted as SQL:\n%s", out)
	}
}

// ordered reports whether each substring appears after the previous one in s.
func ordered(s string, subs ...string) bool {
	idx := 0
	for _, sub := range subs {
		i := strings.Index(s[idx:], sub)
		if i < 0 {
			return false
		}
		idx += i + len(sub)
	}
	return true
}
