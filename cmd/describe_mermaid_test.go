package cmd

import (
	"strings"
	"testing"

	"github.com/capitaltg/pgdx/internal/catalog"
)

func TestRenderTableMermaid(t *testing.T) {
	d := &catalog.TableDetail{
		Schema: "public",
		Name:   "orders",
		Columns: []catalog.Column{
			{Name: "id", Type: "bigint", Nullable: false},
			{Name: "customer_id", Type: "bigint", Nullable: false},
			{Name: "email", Type: "character varying(255)", Nullable: true},
			{Name: "total", Type: "numeric(10,2)", Nullable: true},
		},
		Constraints: []catalog.Constraint{
			{Name: "orders_pkey", Type: "primary key", Definition: "PRIMARY KEY (id)"},
			{Name: "orders_email_key", Type: "unique", Definition: "UNIQUE (email)"},
			{Name: "orders_customer_id_fkey", Type: "foreign key", Definition: "FOREIGN KEY (customer_id) REFERENCES customers(id)"},
		},
		ReferencedBy: []catalog.Reference{
			{Schema: "public", Table: "order_items", Constraint: "order_items_order_id_fkey", Definition: "FOREIGN KEY (order_id) REFERENCES orders(id)"},
		},
	}

	var b strings.Builder
	if err := renderTableMermaid(&b, d); err != nil {
		t.Fatalf("renderTableMermaid: %v", err)
	}
	out := b.String()

	want := []string{
		"erDiagram",
		"    orders {",
		"        bigint id PK \"not null\"",
		"        bigint customer_id FK \"not null\"",
		"        character_varying_255 email UK \"null\"", // type folded to one token
		"        numeric_10_2 total \"null\"",
		"    customers ||--o{ orders : \"orders_customer_id_fkey\"", // outgoing FK
		"    orders ||--o{ order_items : \"order_items_order_id_fkey\"", // incoming FK
	}
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("mermaid output missing line:\n  %q\nfull output:\n%s", w, out)
		}
	}
}

func TestRenderSchemaMermaid(t *testing.T) {
	g := &catalog.SchemaGraph{
		Tables: []catalog.SchemaGraphTable{
			{Schema: "public", Name: "customers", Columns: []catalog.SchemaGraphColumn{
				{Name: "id", Type: "bigint", IsPK: true},
				{Name: "email", Type: "text", IsUnique: true, Nullable: true},
				{Name: "notes", Type: "text", Nullable: true}, // plain column
			}},
			{Schema: "public", Name: "orders", Columns: []catalog.SchemaGraphColumn{
				{Name: "id", Type: "bigint", IsPK: true},
				{Name: "customer_id", Type: "bigint", IsFK: true},
				{Name: "amount", Type: "numeric(10,2)", Nullable: true}, // plain column
			}},
		},
		Edges: []catalog.SchemaGraphEdge{
			{FromSchema: "public", FromTable: "orders", ToSchema: "public", ToTable: "customers", Constraint: "orders_customer_id_fkey"},
		},
	}

	// Default: key columns only — plain columns omitted, edge present.
	var b strings.Builder
	if err := renderSchemaMermaid(&b, g, false); err != nil {
		t.Fatalf("renderSchemaMermaid: %v", err)
	}
	out := b.String()
	for _, w := range []string{
		"erDiagram",
		"    customers {",
		"        bigint id PK \"not null\"",
		"        text email UK \"null\"",
		"        bigint customer_id FK \"not null\"",
		"    customers ||--o{ orders : \"orders_customer_id_fkey\"",
	} {
		if !strings.Contains(out, w) {
			t.Errorf("key-only output missing %q\nfull:\n%s", w, out)
		}
	}
	if strings.Contains(out, "notes") || strings.Contains(out, "amount") {
		t.Errorf("key-only output should omit plain columns, got:\n%s", out)
	}

	// --all-columns: plain columns included.
	var bAll strings.Builder
	if err := renderSchemaMermaid(&bAll, g, true); err != nil {
		t.Fatalf("renderSchemaMermaid(all): %v", err)
	}
	allOut := bAll.String()
	if !strings.Contains(allOut, "text notes") || !strings.Contains(allOut, "numeric_10_2 amount") {
		t.Errorf("--all-columns output should include plain columns, got:\n%s", allOut)
	}
}

func TestMermaidEntityDropsPublicSchema(t *testing.T) {
	if got := mermaidEntity("public", "orders"); got != "orders" {
		t.Errorf("public schema should be dropped: got %q", got)
	}
	if got := mermaidEntity("app", "users"); got != "app_users" {
		t.Errorf("non-public schema should fold in: got %q", got)
	}
}
