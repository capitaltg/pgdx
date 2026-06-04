package cmd

import (
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/render"
)

// parseFormatAllowingDDL accepts the shared table/json formats plus -o ddl. The
// describe-object commands support DDL but not mermaid, so they use this rather than
// widening the shared render.ParseFormat (which all commands call).
func parseFormatAllowingDDL(s string) (render.Format, error) {
	if render.Format(s) == render.FormatDDL {
		return render.FormatDDL, nil
	}
	return render.ParseFormat(s)
}

// DDL output is a readable reference / scaffold, NOT a pg_dump replacement. It covers
// the core structure: columns (type, NOT NULL, DEFAULT), primary-key/unique/check/
// foreign-key constraints, indexes, and the partition-by clause. It intentionally does
// NOT reproduce identity/generated-column syntax (these surface only as a DEFAULT or are
// invisible to `describe`), storage parameters, tablespaces, comments, ownership, grants,
// RLS, or triggers. For backups and migrations, use pg_dump.
const ddlReferenceNote = "-- pgdx DDL: a readable reference, not a pg_dump replacement.\n" +
	"-- Omits identity/generated syntax, storage params, comments, ownership, grants, RLS, and triggers.\n"

// renderTableDDL emits CREATE TABLE for one table with its foreign keys inline (you're
// looking at a single table, so forward references aren't a concern). Data → stdout (D4).
func renderTableDDL(w io.Writer, d *catalog.TableDetail) error {
	var b strings.Builder
	b.WriteString(ddlReferenceNote)
	b.WriteByte('\n')
	tableDDL(&b, d, nil)
	_, err := io.WriteString(w, b.String())
	return err
}

// renderSchemaDDL emits CREATE TABLE for every table, then every foreign key as a
// trailing ALTER TABLE. Deferring FKs lets the script replay regardless of table order
// (the pg_dump approach — inline FKs would fail on forward references). Data → stdout.
func renderSchemaDDL(w io.Writer, details []*catalog.TableDetail) error {
	var b strings.Builder
	b.WriteString(ddlReferenceNote)

	var deferredFKs []string
	for _, d := range details {
		b.WriteByte('\n')
		tableDDL(&b, d, &deferredFKs)
	}
	if len(deferredFKs) > 0 {
		b.WriteString("\n-- Foreign keys (added after all tables so the script replays in any order).\n")
		for _, fk := range deferredFKs {
			b.WriteString(fk)
			b.WriteByte('\n')
		}
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// constraintDDLOrder is the conventional, readable order for table constraints (the
// catalog returns them ordered by contype, which interleaves them differently).
var constraintDDLOrder = map[string]int{
	"primary key": 0,
	"unique":      1,
	"check":       2,
	"foreign key": 3,
}

// tableDDL appends one CREATE TABLE statement (plus its standalone indexes) to b. When
// deferFKs is non-nil, foreign-key constraints are appended to it as ALTER TABLE
// statements instead of being written inline.
func tableDDL(b *strings.Builder, d *catalog.TableDetail, deferFKs *[]string) {
	qname := qualifiedIdent(d.Schema, d.Name)

	if d.Partition != nil && d.Partition.ParentTable != "" {
		fmt.Fprintf(b, "-- partition of %s %s\n", d.Partition.ParentTable, d.Partition.Bound)
	}
	fmt.Fprintf(b, "CREATE TABLE %s (\n", qname)

	// Column definitions.
	var lines []string
	for _, c := range d.Columns {
		line := "    " + ddlIdent(c.Name) + " " + c.Type
		if !c.Nullable {
			line += " NOT NULL"
		}
		if c.Default != "" {
			line += " DEFAULT " + c.Default
		}
		lines = append(lines, line)
	}

	// Constraints, in readable order. FKs may be deferred to a trailing ALTER TABLE.
	cons := append([]catalog.Constraint(nil), d.Constraints...)
	sort.SliceStable(cons, func(i, j int) bool {
		return constraintDDLOrder[cons[i].Type] < constraintDDLOrder[cons[j].Type]
	})
	for _, c := range cons {
		if c.Type == "foreign key" && deferFKs != nil {
			*deferFKs = append(*deferFKs, fmt.Sprintf("ALTER TABLE %s ADD CONSTRAINT %s %s;",
				qname, ddlIdent(c.Name), c.Definition))
			continue
		}
		lines = append(lines, fmt.Sprintf("    CONSTRAINT %s %s", ddlIdent(c.Name), c.Definition))
	}

	b.WriteString(strings.Join(lines, ",\n"))
	b.WriteString("\n)")
	if d.Partition != nil && d.Partition.IsPartitioned {
		// Partition.Key is pg_get_partkeydef, which already includes the strategy word
		// (e.g. "RANGE (created_at)").
		fmt.Fprintf(b, " PARTITION BY %s", d.Partition.Key)
	}
	b.WriteString(";\n")

	// Standalone indexes. Skip those backing a PK/unique constraint: Postgres names the
	// backing index after the constraint, and the constraint above already creates it.
	backed := map[string]bool{}
	for _, c := range d.Constraints {
		if c.Type == "primary key" || c.Type == "unique" {
			backed[c.Name] = true
		}
	}
	for _, ix := range d.Indexes {
		if backed[ix.Name] || ix.Definition == "" {
			continue
		}
		b.WriteString(ix.Definition)
		b.WriteString(";\n")
	}
}

// renderViewDDL wraps a view's stored SELECT (pg_get_viewdef returns only the body) in a
// runnable CREATE statement. Matviews carry WITH [NO] DATA reflecting whether they've been
// REFRESHed. Data → stdout (D4).
func renderViewDDL(w io.Writer, d *catalog.ViewDetail) error {
	body := strings.TrimRight(strings.TrimSpace(d.Definition), ";")
	qname := qualifiedIdent(d.Schema, d.Name)
	var b strings.Builder
	b.WriteString("-- pgdx DDL: a readable reference, not a pg_dump replacement (omits ownership, grants, comments).\n\n")
	if d.Type == "materialized" {
		data := "DATA"
		if !d.Populated {
			data = "NO DATA"
		}
		fmt.Fprintf(&b, "CREATE MATERIALIZED VIEW %s AS\n%s\nWITH %s;\n", qname, body, data)
	} else {
		fmt.Fprintf(&b, "CREATE OR REPLACE VIEW %s AS\n%s;\n", qname, body)
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// renderIndexDDL emits the index's pg_get_indexdef, which is already exact CREATE INDEX
// DDL — no reference caveat needed. Data → stdout (D4).
func renderIndexDDL(w io.Writer, d *catalog.IndexDetail) error {
	def := strings.TrimRight(strings.TrimSpace(d.Definition), ";")
	_, err := io.WriteString(w, def+";\n")
	return err
}

// renderSequenceDDL assembles CREATE SEQUENCE from the sequence's properties (Postgres has
// no pg_get_sequencedef). Values are emitted explicitly rather than relying on type
// defaults, matching pg_dump. OWNED BY is omitted — see the note. Data → stdout (D4).
func renderSequenceDDL(w io.Writer, s *catalog.SequenceDetail) error {
	var b strings.Builder
	b.WriteString("-- pgdx DDL: a readable reference, not a pg_dump replacement (omits OWNED BY, ownership, grants).\n\n")
	fmt.Fprintf(&b, "CREATE SEQUENCE %s\n", qualifiedIdent(s.Schema, s.Name))
	fmt.Fprintf(&b, "    AS %s\n", s.DataType)
	fmt.Fprintf(&b, "    INCREMENT BY %d\n", s.Increment)
	fmt.Fprintf(&b, "    MINVALUE %d\n", s.Min)
	fmt.Fprintf(&b, "    MAXVALUE %d\n", s.Max)
	fmt.Fprintf(&b, "    START WITH %d\n", s.Start)
	fmt.Fprintf(&b, "    CACHE %d", s.Cache)
	if s.Cycle {
		b.WriteString("\n    CYCLE")
	}
	b.WriteString(";\n")
	_, err := io.WriteString(w, b.String())
	return err
}

// renderFunctionDDL emits each overload's pg_get_functiondef (a complete CREATE OR REPLACE
// FUNCTION). Aggregate/window kinds have no definition (the builtin doesn't apply) and are
// noted as a comment. Data → stdout (D4).
func renderFunctionDDL(w io.Writer, d *catalog.FunctionDetail) error {
	var b strings.Builder
	for i, f := range d.Overloads {
		if i > 0 {
			b.WriteByte('\n')
		}
		sig := fmt.Sprintf("%s.%s(%s)", f.Schema, f.Name, f.Args)
		if strings.TrimSpace(f.Definition) == "" {
			fmt.Fprintf(&b, "-- %s %s: DDL not available (pg_get_functiondef doesn't support aggregate/window functions)\n", f.Kind, sig)
			continue
		}
		def := strings.TrimRight(strings.TrimSpace(f.Definition), ";")
		b.WriteString(def)
		b.WriteString(";\n")
	}
	_, err := io.WriteString(w, b.String())
	return err
}

// qualifiedIdent renders schema.name with each part quoted only as needed.
func qualifiedIdent(schema, name string) string {
	if schema == "" {
		return ddlIdent(name)
	}
	return ddlIdent(schema) + "." + ddlIdent(name)
}

// simpleIdent matches an identifier that needs no double-quoting: lowercase start,
// then lowercase/digit/underscore.
var simpleIdent = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

// reservedIdents are SQL keywords common enough to appear as column names; they must be
// quoted even though they're lexically simple. Not exhaustive — review unusual names.
var reservedIdents = map[string]bool{
	"user": true, "order": true, "group": true, "table": true, "column": true,
	"select": true, "where": true, "from": true, "default": true, "check": true,
	"primary": true, "references": true, "constraint": true, "unique": true,
	"limit": true, "offset": true, "desc": true, "asc": true, "all": true,
	"and": true, "or": true, "not": true, "null": true, "true": true, "false": true,
	"end": true, "case": true, "when": true, "then": true, "else": true,
}

// ddlIdent double-quotes a Postgres identifier when it isn't a plain lowercase token or
// is a reserved word, doubling any embedded quotes. Plain names are left bare for
// readability (matching how psql/DBeaver present DDL). Distinct from vacuum's quoteIdent,
// which always quotes for injection safety in command text.
func ddlIdent(s string) string {
	if simpleIdent.MatchString(s) && !reservedIdents[s] {
		return s
	}
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}
