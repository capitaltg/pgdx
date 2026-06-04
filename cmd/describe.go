package cmd

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/render"
)

// newDescribeCmd is the kubectl-style detail verb. v0.1 ships `describe table`.
func newDescribeCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "describe <resource>",
		Short: "Show the details of one object (read-only)",
	}
	c.AddCommand(newDescribeTableCmd())
	c.AddCommand(newDescribeIndexCmd())
	c.AddCommand(newDescribeViewCmd())
	c.AddCommand(newDescribeFunctionCmd())
	c.AddCommand(newDescribeSequenceCmd())
	return c
}

func newDescribeViewCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "view <name>",
		Short: "Show a view's columns and definition",
		Long: "view shows a view or materialized view: its columns, the SELECT definition, and\n" +
			"(for materialized views) size and whether it's been populated. Accepts a bare name\n" +
			"or schema.name. Read-only.\n\n" +
			"-o ddl wraps the stored query in a runnable CREATE [OR REPLACE] VIEW (or CREATE\n" +
			"MATERIALIZED VIEW ... WITH [NO] DATA). It's a reference, not a pg_dump replacement\n" +
			"(omits ownership, grants, comments).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseFormatAllowingDDL(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, release, err := dial(ctx, cmd, flagDatabase)
			if err != nil {
				return err
			}
			defer release()

			d, err := catalog.DescribeView(ctx, conn, args[0])
			if err != nil {
				return err
			}
			switch format {
			case render.FormatJSON:
				return render.Render(cmd.OutOrStdout(), format, d)
			case render.FormatDDL:
				return renderViewDDL(cmd.OutOrStdout(), d)
			}
			return renderViewDetail(cmd, d)
		},
	}
	return c
}

func renderViewDetail(cmd *cobra.Command, d *catalog.ViewDetail) error {
	out := cmd.OutOrStdout()
	label := "View"
	if d.Type == "materialized" {
		label = "Materialized view"
	}
	fmt.Fprintf(out, "%s \"%s.%s\"\n", label, d.Schema, d.Name)
	fmt.Fprintf(out, "Owner: %s\n", d.Owner)
	if d.Type == "materialized" {
		pop := "yes"
		if !d.Populated {
			pop = "no — not yet REFRESHed (queries see no rows)"
		}
		fmt.Fprintf(out, "Size: %s   Populated: %s\n", d.Size, pop)
	}
	fmt.Fprintln(out, "\nColumns:")
	if err := render.Render(out, render.FormatTable, columnsView(d.Columns)); err != nil {
		return err
	}
	fmt.Fprintln(out, "\nDefinition:")
	fmt.Fprintf(out, "%s\n", d.Definition)
	return nil
}

func newDescribeTableCmd() *cobra.Command {
	var stats bool
	c := &cobra.Command{
		Use:   "table <name>",
		Short: "Show a table's columns, indexes, constraints, and incoming FKs",
		Long: "table shows one table in detail: columns (type/nullable/default), its indexes\n" +
			"(with scan counts), constraints, and which other tables reference it (incoming\n" +
			"foreign keys — the 'is it safe to drop?' answer). Accepts a bare name or\n" +
			"schema.name; a bare name that exists in multiple schemas must be qualified.\n" +
			"Read-only.\n\n" +
			"--stats adds per-column planner statistics from pg_stats (n_distinct, null\n" +
			"fraction, correlation, average width). A wrong n_distinct or stale stats are a\n" +
			"leading cause of the row-estimate blowups `pgdx explain` flags — this is where you\n" +
			"confirm them. (pg_stats only exposes columns your role may see; non-privileged\n" +
			"users may get fewer rows.)\n\n" +
			"-o mermaid emits an entity-relationship diagram; -o ddl emits a CREATE TABLE\n" +
			"reference (columns, constraints, indexes, partition-by). The DDL is for reading\n" +
			"and scaffolding — it omits identity/generated syntax, storage params, comments,\n" +
			"ownership, and grants, so use pg_dump for backups and migrations.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// describe table additionally supports -o mermaid (an entity-relationship
			// diagram) and -o ddl (a CREATE TABLE reference); other commands only do
			// table/json, so we accept these here rather than widening render.ParseFormat.
			format := render.Format(flagOutput)
			if format != render.FormatMermaid && format != render.FormatDDL {
				var err error
				format, err = render.ParseFormat(flagOutput)
				if err != nil {
					return usageError{err.Error()}
				}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, release, err := dial(ctx, cmd, flagDatabase)
			if err != nil {
				return err
			}
			defer release()

			detail, err := catalog.DescribeTable(ctx, conn, args[0])
			if err != nil {
				return err
			}
			if stats {
				cs, err := catalog.GatherColumnStats(ctx, conn, detail.Schema, detail.Name)
				if err != nil {
					return err
				}
				detail.ColumnStats = cs
			}

			switch format {
			case render.FormatJSON:
				return render.Render(cmd.OutOrStdout(), format, detail)
			case render.FormatMermaid:
				return renderTableMermaid(cmd.OutOrStdout(), detail)
			case render.FormatDDL:
				return renderTableDDL(cmd.OutOrStdout(), detail)
			default:
				return renderTableDetail(cmd, detail, stats)
			}
		},
	}
	c.Flags().BoolVar(&stats, "stats", false, "include per-column planner statistics from pg_stats (n_distinct, null frac, correlation)")
	return c
}

// renderTableDetail prints the human (table-format) detail view: a header line plus
// Columns / Indexes / Constraints / Referenced-by sections (and Column statistics when
// withStats). All of it is data → stdout (D4).
func renderTableDetail(cmd *cobra.Command, d *catalog.TableDetail, withStats bool) error {
	out := cmd.OutOrStdout()

	rows := "—"
	if d.EstRows >= 0 {
		rows = withThousands(d.EstRows)
	}
	fmt.Fprintf(out, "Table \"%s.%s\"\n", d.Schema, d.Name)
	fmt.Fprintf(out, "Owner: %s   Size: %s   Rows (est): %s\n", d.Owner, d.Size, rows)

	fmt.Fprintln(out, "\nColumns:")
	if err := render.Render(out, render.FormatTable, columnsView(d.Columns)); err != nil {
		return err
	}

	fmt.Fprintln(out, "\nIndexes:")
	if len(d.Indexes) == 0 {
		fmt.Fprintln(out, "  (none)")
	} else if err := render.Render(out, render.FormatTable, describeIndexesView(d.Indexes)); err != nil {
		return err
	}

	fmt.Fprintln(out, "\nConstraints:")
	if len(d.Constraints) == 0 {
		fmt.Fprintln(out, "  (none)")
	} else if err := render.Render(out, render.FormatTable, constraintsView(d.Constraints)); err != nil {
		return err
	}

	fmt.Fprintln(out, "\nReferenced by:")
	if len(d.ReferencedBy) == 0 {
		fmt.Fprintln(out, "  (nothing references this table)")
	} else if err := render.Render(out, render.FormatTable, referencedByView(d.ReferencedBy)); err != nil {
		return err
	}

	if withStats {
		fmt.Fprintln(out, "\nColumn statistics (pg_stats):")
		if len(d.ColumnStats) == 0 {
			fmt.Fprintln(out, "  (none — table not yet ANALYZEd, or no visible stats for your role)")
		} else if err := render.Render(out, render.FormatTable, columnStatsView(d.ColumnStats)); err != nil {
			return err
		}
	}

	if h := d.Health; h != nil {
		fmt.Fprintln(out, "\nMaintenance:")
		fmt.Fprintf(out, "  Live tuples: %s   Dead tuples: %s (%.0f%%)\n",
			withThousands(h.LiveTuples), withThousands(h.DeadTuples), 100*h.DeadRatio)
		if h.DeadRatio >= 0.20 && h.DeadTuples >= 1000 {
			fmt.Fprintln(out, "  ⚠ high dead-tuple ratio — this table may be bloated; consider VACUUM (and check autovacuum).")
		}
		fmt.Fprintf(out, "  Last vacuum:  %s   (auto: %s)\n", orNever(h.LastVacuum), orNever(h.LastAutovacuum))
		fmt.Fprintf(out, "  Last analyze: %s   (auto: %s)\n", orNever(h.LastAnalyze), orNever(h.LastAutoanalyze))
	}

	if p := d.Partition; p != nil {
		fmt.Fprintln(out)
		if p.ParentTable != "" {
			fmt.Fprintf(out, "Partition of: %s  %s\n", p.ParentTable, p.Bound)
		}
		if p.IsPartitioned {
			fmt.Fprintf(out, "Partitioned by: %s\n", p.Key) // pg_get_partkeydef already includes the strategy word
			fmt.Fprintf(out, "Partitions (%d):\n", len(p.Partitions))
			if len(p.Partitions) == 0 {
				fmt.Fprintln(out, "  (none yet)")
			} else if err := render.Render(out, render.FormatTable, partitionsView(p.Partitions)); err != nil {
				return err
			}
		}
	}
	return nil
}

func orNever(s string) string {
	if s == "" {
		return "never"
	}
	return s
}

// fkDefRe pulls the pieces out of a pg_get_constraintdef foreign-key definition:
//
//	FOREIGN KEY (a, b) REFERENCES schema.other(x, y)
//
// group 1 = local columns, group 2 = referenced table, group 3 = referenced columns.
var fkDefRe = regexp.MustCompile(`(?is)FOREIGN KEY\s*\(([^)]*)\)\s*REFERENCES\s+([^\s(]+)\s*\(([^)]*)\)`)

// parenColsRe grabs the first parenthesized column list, e.g. PRIMARY KEY (id).
var parenColsRe = regexp.MustCompile(`\(([^)]*)\)`)

// renderTableMermaid emits a Mermaid erDiagram for one table: its columns (with
// PK/FK/UK tags and nullability) plus its outgoing and incoming foreign-key
// relationships. Stats, health, and partitioning are intentionally omitted — this is
// a relationship diagram, not a full dump. Data → stdout (D4).
func renderTableMermaid(w io.Writer, d *catalog.TableDetail) error {
	self := mermaidEntity(d.Schema, d.Name)

	// Build per-column key tags from the constraints.
	tags := map[string][]string{}
	addTag := func(cols []string, tag string) {
		for _, c := range cols {
			tags[c] = append(tags[c], tag)
		}
	}
	for _, c := range d.Constraints {
		switch c.Type {
		case "primary key":
			addTag(parseColList(c.Definition), "PK")
		case "unique":
			addTag(parseColList(c.Definition), "UK")
		case "foreign key":
			if m := fkDefRe.FindStringSubmatch(c.Definition); m != nil {
				addTag(splitCols(m[1]), "FK")
			}
		}
	}

	var b strings.Builder
	b.WriteString("erDiagram\n")

	// Focal entity with full column detail.
	fmt.Fprintf(&b, "    %s {\n", self)
	for _, col := range d.Columns {
		line := fmt.Sprintf("        %s %s", mermaidType(col.Type), mermaidIdent(col.Name))
		if t := tags[col.Name]; len(t) > 0 {
			line += " " + strings.Join(t, ",")
		}
		nullable := "not null"
		if col.Nullable {
			nullable = "null"
		}
		line += fmt.Sprintf(" %q", nullable)
		b.WriteString(line + "\n")
	}
	b.WriteString("    }\n")

	// Outgoing FKs: this table references a parent. parent ||--o{ self.
	for _, c := range d.Constraints {
		if c.Type != "foreign key" {
			continue
		}
		m := fkDefRe.FindStringSubmatch(c.Definition)
		if m == nil {
			continue
		}
		parent := mermaidEntity(catalog.SplitQualified(m[2]))
		fmt.Fprintf(&b, "    %s ||--o{ %s : %q\n", parent, self, c.Name)
	}

	// Incoming FKs: another table references this one. self ||--o{ child.
	for _, r := range d.ReferencedBy {
		child := mermaidEntity(r.Schema, r.Table)
		fmt.Fprintf(&b, "    %s ||--o{ %s : %q\n", self, child, r.Constraint)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// renderSchemaMermaid emits a Mermaid erDiagram for a whole schema: every table as an
// entity and every foreign key as a relationship edge. With allColumns=false (the
// default) each entity shows only its key columns (PK/FK/unique) to stay readable on
// large schemas; allColumns=true shows every column. Data → stdout (D4).
func renderSchemaMermaid(w io.Writer, g *catalog.SchemaGraph, allColumns bool) error {
	var b strings.Builder
	b.WriteString("erDiagram\n")

	for _, t := range g.Tables {
		entity := mermaidEntity(t.Schema, t.Name)
		var lines []string
		for _, col := range t.Columns {
			if !allColumns && !col.IsPK && !col.IsFK && !col.IsUnique {
				continue // key-columns-only mode
			}
			var keys []string
			if col.IsPK {
				keys = append(keys, "PK")
			}
			if col.IsFK {
				keys = append(keys, "FK")
			}
			if col.IsUnique {
				keys = append(keys, "UK")
			}
			line := fmt.Sprintf("        %s %s", mermaidType(col.Type), mermaidIdent(col.Name))
			if len(keys) > 0 {
				line += " " + strings.Join(keys, ",")
			}
			nullable := "not null"
			if col.Nullable {
				nullable = "null"
			}
			line += fmt.Sprintf(" %q", nullable)
			lines = append(lines, line)
		}
		// A table with no displayed columns (key-only mode, no keys) is emitted as a bare
		// entity name — an empty "{ }" block trips some Mermaid renderers.
		if len(lines) == 0 {
			fmt.Fprintf(&b, "    %s\n", entity)
			continue
		}
		fmt.Fprintf(&b, "    %s {\n", entity)
		for _, l := range lines {
			b.WriteString(l + "\n")
		}
		b.WriteString("    }\n")
	}

	for _, e := range g.Edges {
		// parent (referenced, "one") ||--o{ child (FK holder, "many").
		parent := mermaidEntity(e.ToSchema, e.ToTable)
		child := mermaidEntity(e.FromSchema, e.FromTable)
		fmt.Fprintf(&b, "    %s ||--o{ %s : %q\n", parent, child, e.Constraint)
	}

	_, err := io.WriteString(w, b.String())
	return err
}

// parseColList returns the columns of the first parenthesized list in a constraint
// definition, e.g. "PRIMARY KEY (a, b)" -> ["a","b"].
func parseColList(def string) []string {
	m := parenColsRe.FindStringSubmatch(def)
	if m == nil {
		return nil
	}
	return splitCols(m[1])
}

func splitCols(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if c := strings.TrimSpace(p); c != "" {
			out = append(out, c)
		}
	}
	return out
}

// nonIdentRe matches any run of characters Mermaid won't accept in an identifier.
var nonIdentRe = regexp.MustCompile(`[^A-Za-z0-9_]+`)

// mermaidEntity is a diagram-safe entity name; the public schema is dropped, other
// schemas are folded in with an underscore (public.orders -> orders, app.users ->
// app_users).
func mermaidEntity(schema, name string) string {
	if schema != "" && schema != "public" {
		return mermaidIdent(schema + "_" + name)
	}
	return mermaidIdent(name)
}

func mermaidIdent(s string) string {
	s = nonIdentRe.ReplaceAllString(s, "_")
	return strings.Trim(s, "_")
}

// mermaidType folds a Postgres type into a single token Mermaid will accept as an
// attribute type ("character varying(255)" -> "character_varying_255").
func mermaidType(t string) string {
	if s := mermaidIdent(t); s != "" {
		return s
	}
	return "unknown"
}

type partitionsView []catalog.Partition

func (v partitionsView) Headers() []string { return []string{"NAME", "BOUND"} }
func (v partitionsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, p := range v {
		name := p.Name
		if p.Schema != "" && p.Schema != "public" {
			name = p.Schema + "." + p.Name
		}
		out = append(out, []string{name, p.Bound})
	}
	return out
}

func newDescribeIndexCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "index <name>",
		Short: "Show an index's definition, method, usage, and validity",
		Long: "index shows one index in detail: the table it's on, access method, uniqueness,\n" +
			"size, scan/tuple usage, the full definition, and whether it's VALID (a failed\n" +
			"CREATE INDEX CONCURRENTLY leaves an invalid index that queries silently ignore).\n" +
			"Accepts a bare name or schema.name. Read-only.\n\n" +
			"-o ddl emits the CREATE INDEX statement (exact, from pg_get_indexdef).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseFormatAllowingDDL(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, release, err := dial(ctx, cmd, flagDatabase)
			if err != nil {
				return err
			}
			defer release()

			d, err := catalog.DescribeIndex(ctx, conn, args[0])
			if err != nil {
				return err
			}
			switch format {
			case render.FormatJSON:
				return render.Render(cmd.OutOrStdout(), format, d)
			case render.FormatDDL:
				return renderIndexDDL(cmd.OutOrStdout(), d)
			}
			return renderIndexDetail(cmd, d)
		},
	}
	return c
}

func renderIndexDetail(cmd *cobra.Command, d *catalog.IndexDetail) error {
	out := cmd.OutOrStdout()
	yn := func(b bool) string {
		if b {
			return "yes"
		}
		return "no"
	}
	fmt.Fprintf(out, "Index \"%s.%s\"\n", d.Schema, d.Name)
	if !d.Valid {
		fmt.Fprintln(out, "⚠ INVALID — this index is incomplete (failed or in-progress build) and is NOT used by queries.")
		fmt.Fprintln(out, "  Fix with: REINDEX INDEX, or DROP and recreate it.")
	}
	fmt.Fprintf(out, "Table:   %s.%s\n", d.Schema, d.Table)
	fmt.Fprintf(out, "Method:  %s\n", d.Method)
	fmt.Fprintf(out, "Unique:  %s   Primary: %s   Valid: %s\n", yn(d.Unique), yn(d.Primary), yn(d.Valid))
	fmt.Fprintf(out, "Size:    %s\n", d.Size)
	fmt.Fprintf(out, "Usage:   %s scans, %s tuples read, %s fetched\n",
		withThousands(d.Scans), withThousands(d.TuplesRead), withThousands(d.TuplesFetched))
	if d.Scans == 0 {
		fmt.Fprintln(out, "         (0 scans — this index may be unused dead weight)")
	}
	fmt.Fprintln(out, "\nDefinition:")
	fmt.Fprintf(out, "  %s\n", d.Definition)
	return nil
}

func newDescribeFunctionCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "function <name>",
		Short: "Show a function/procedure's overloads and source",
		Long: "function shows every overload a name resolves to: its arguments, return type,\n" +
			"language, and owner. Accepts a bare name or schema.name; a bare name is matched\n" +
			"across all non-system schemas. Read-only.\n\n" +
			"-o ddl emits the full CREATE OR REPLACE FUNCTION for each overload (from\n" +
			"pg_get_functiondef). Aggregate and window functions have no such definition and are\n" +
			"noted instead.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseFormatAllowingDDL(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, release, err := dial(ctx, cmd, flagDatabase)
			if err != nil {
				return err
			}
			defer release()

			d, err := catalog.DescribeFunction(ctx, conn, args[0])
			if err != nil {
				return err
			}
			switch format {
			case render.FormatJSON:
				return render.Render(cmd.OutOrStdout(), format, d)
			case render.FormatDDL:
				return renderFunctionDDL(cmd.OutOrStdout(), d)
			}
			out := cmd.OutOrStdout()
			noun := "overload"
			if len(d.Overloads) != 1 {
				noun = "overloads"
			}
			fmt.Fprintf(out, "Function %q — %d %s\n\n", d.Name, len(d.Overloads), noun)
			return render.Render(out, render.FormatTable, functionOverloadsView(d.Overloads))
		},
	}
	return c
}

func newDescribeSequenceCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "sequence <name>",
		Short: "Show a sequence's type, bounds, increment, and current value",
		Long: "sequence shows one sequence: data type, start, min/max, increment, cache, whether\n" +
			"it cycles, and its last value. Accepts a bare name or schema.name; a bare name that\n" +
			"exists in multiple schemas must be qualified. Read-only.\n\n" +
			"-o ddl emits a CREATE SEQUENCE reference (omits OWNED BY, ownership, grants).",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := parseFormatAllowingDDL(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, release, err := dial(ctx, cmd, flagDatabase)
			if err != nil {
				return err
			}
			defer release()

			d, err := catalog.DescribeSequence(ctx, conn, args[0])
			if err != nil {
				return err
			}
			switch format {
			case render.FormatJSON:
				return render.Render(cmd.OutOrStdout(), format, d)
			case render.FormatDDL:
				return renderSequenceDDL(cmd.OutOrStdout(), d)
			}
			return renderSequenceDetail(cmd, d)
		},
	}
	return c
}

func renderSequenceDetail(cmd *cobra.Command, d *catalog.SequenceDetail) error {
	out := cmd.OutOrStdout()
	last := "—  (never used)"
	if d.LastValue != nil {
		last = withThousands(*d.LastValue)
	}
	cycle := "no"
	if d.Cycle {
		cycle = "yes"
	}
	fmt.Fprintf(out, "Sequence \"%s.%s\"\n", d.Schema, d.Name)
	fmt.Fprintf(out, "Type:       %s\n", d.DataType)
	fmt.Fprintf(out, "Increment:  %s\n", withThousands(d.Increment))
	fmt.Fprintf(out, "Min / Max:  %s / %s\n", withThousands(d.Min), withThousands(d.Max))
	fmt.Fprintf(out, "Start:      %s\n", withThousands(d.Start))
	fmt.Fprintf(out, "Cache:      %s   Cycle: %s\n", withThousands(d.Cache), cycle)
	fmt.Fprintf(out, "Last value: %s\n", last)
	return nil
}

// functionOverloadsView lists the overloads a function name resolves to.
type functionOverloadsView []catalog.FunctionOverload

func (v functionOverloadsView) Headers() []string {
	return []string{"SCHEMA", "KIND", "ARGUMENTS", "RETURNS", "LANGUAGE", "OWNER"}
}
func (v functionOverloadsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, f := range v {
		out = append(out, []string{f.Schema, f.Kind, dashIfEmpty(f.Args), f.Result, f.Language, f.Owner})
	}
	return out
}

type columnsView []catalog.Column

func (v columnsView) Headers() []string { return []string{"NAME", "TYPE", "NULLABLE", "DEFAULT"} }
func (v columnsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, c := range v {
		nullable := "not null"
		if c.Nullable {
			nullable = "null"
		}
		def := c.Default
		if def == "" {
			def = "—"
		}
		out = append(out, []string{c.Name, c.Type, nullable, def})
	}
	return out
}

// describeIndexesView is the index section inside describe (no SCHEMA/TABLE columns,
// since they're fixed by the table being described).
type describeIndexesView []catalog.Index

func (v describeIndexesView) Headers() []string { return []string{"NAME", "UNIQUE", "SIZE", "SCANS"} }
func (v describeIndexesView) Aligns() []render.Align {
	return []render.Align{render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignRight}
}
func (v describeIndexesView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, ix := range v {
		unique := "no"
		if ix.Unique {
			unique = "yes"
		}
		out = append(out, []string{ix.Name, unique, ix.Size, withThousands(ix.Scans)})
	}
	return out
}

type constraintsView []catalog.Constraint

func (v constraintsView) Headers() []string { return []string{"NAME", "TYPE", "DEFINITION"} }
func (v constraintsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, c := range v {
		out = append(out, []string{c.Name, c.Type, c.Definition})
	}
	return out
}

// referencedByView lists incoming foreign keys (tables that point at this one).
type referencedByView []catalog.Reference

func (v referencedByView) Headers() []string { return []string{"TABLE", "CONSTRAINT", "DEFINITION"} }
func (v referencedByView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, r := range v {
		table := r.Table
		if r.Schema != "" && r.Schema != "public" {
			table = r.Schema + "." + r.Table
		}
		out = append(out, []string{table, r.Constraint, r.Definition})
	}
	return out
}

// columnStatsView renders per-column planner statistics. n_distinct is shown as Postgres
// stores it: a negative value is a multiple of the row count (-1 = unique), a positive
// value is an absolute distinct-value estimate.
type columnStatsView []catalog.ColumnStat

func (v columnStatsView) Headers() []string {
	return []string{"COLUMN", "NULL%", "N-DISTINCT", "AVG-WIDTH", "CORRELATION", "MOST-COMMON"}
}
func (v columnStatsView) Aligns() []render.Align {
	return []render.Align{
		render.AlignLeft, render.AlignRight, render.AlignRight, render.AlignRight, render.AlignRight, render.AlignLeft,
	}
}
func (v columnStatsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, c := range v {
		ndistinct := fmt.Sprintf("%.0f", c.NDistinct)
		if c.NDistinct < 0 { // a fraction of the row count, e.g. -1 = all distinct
			ndistinct = fmt.Sprintf("%.2f×rows", -c.NDistinct)
		}
		out = append(out, []string{
			c.Column,
			fmt.Sprintf("%.0f%%", 100*c.NullFrac),
			ndistinct,
			fmt.Sprintf("%d", c.AvgWidth),
			fmt.Sprintf("%.2f", c.Correlation),
			dashIfEmpty(c.MCV),
		})
	}
	return out
}
