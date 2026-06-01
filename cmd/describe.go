package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/db"
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
	return c
}

func newDescribeViewCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "view <name>",
		Short: "Show a view's columns and definition",
		Long: "view shows a view or materialized view: its columns, the SELECT definition, and\n" +
			"(for materialized views) size and whether it's been populated. Accepts a bare name\n" +
			"or schema.name. Read-only.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, err := db.Connect(ctx, flagDSN, "", flagDatabase, sqlLog(cmd))
			if err != nil {
				return err
			}
			defer conn.Close(ctx)

			d, err := catalog.DescribeView(ctx, conn, args[0])
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				return render.Render(cmd.OutOrStdout(), format, d)
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
			"users may get fewer rows.)",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, err := db.Connect(ctx, flagDSN, "", flagDatabase, sqlLog(cmd))
			if err != nil {
				return err
			}
			defer conn.Close(ctx)

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

			if format == render.FormatJSON {
				return render.Render(cmd.OutOrStdout(), format, detail)
			}
			return renderTableDetail(cmd, detail, stats)
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
			"Accepts a bare name or schema.name. Read-only.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, err := db.Connect(ctx, flagDSN, "", flagDatabase, sqlLog(cmd))
			if err != nil {
				return err
			}
			defer conn.Close(ctx)

			d, err := catalog.DescribeIndex(ctx, conn, args[0])
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				return render.Render(cmd.OutOrStdout(), format, d)
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
