package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
)

// analyze refreshes planner statistics (ANALYZE). Like vacuum it's one of pgdx's few WRITE
// commands, and defensible against the read-only posture: ANALYZE only samples rows and
// updates pg_statistic / pg_class.reltuples — it never changes data — and takes a
// SHARE UPDATE EXCLUSIVE lock, so it doesn't block reads or writes.
//
// Whole-database analyze is NOT the default: it requires an explicit --all. A bare
// `pgdx analyze` errors with guidance, so a fat-fingered command can't kick off database-
// wide work on a large cluster by accident.
func newAnalyzeCmd() *cobra.Command {
	var schema string
	var all bool
	var verbose bool
	c := &cobra.Command{
		Use:   "analyze [table [columns...]]",
		Short: "Refresh planner statistics (ANALYZE) for a table, specific columns, a schema, or the whole database",
		Long: "analyze runs ANALYZE to refresh the planner's statistics — the row estimates and\n" +
			"column distributions it relies on, and what populates ROWS/DEAD% in `get tables`.\n" +
			"Run it after a restore or a major data load, or when `explain` flags a row-estimate\n" +
			"that looks stale. ANALYZE only samples rows (it does not scan the whole table) and\n" +
			"takes a SHARE UPDATE EXCLUSIVE lock, so it runs online without blocking reads or\n" +
			"writes.\n\n" +
			"Pick exactly one target:\n" +
			"  pgdx analyze public.orders                one table (bare name or schema.name)\n" +
			"  pgdx analyze public.orders created_at id  one table, only these columns\n" +
			"  pgdx analyze --schema app                 every table in a schema\n" +
			"  pgdx analyze --all                        every table in the current database\n\n" +
			"Column-scoped analyze is useful when you've added an index on a column or know one\n" +
			"column's distribution shifted — it refreshes pg_statistic for just those columns\n" +
			"and skips the sample work for the rest.\n\n" +
			"Whole-database analyze is deliberate (--all), never the default, so it can't be\n" +
			"triggered by an accidental bare command.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			errOut := cmd.ErrOrStderr()

			// Exactly one of {table(+optional columns), --schema, --all} must be chosen.
			targets := 0
			if len(args) >= 1 {
				targets++
			}
			if schema != "" {
				targets++
			}
			if all {
				targets++
			}
			switch {
			case targets == 0:
				return usageError{"specify a table, --schema NAME, or --all (analyzing every table is deliberate, so it isn't the default)"}
			case targets > 1:
				return usageError{"choose only one of: a table argument, --schema, or --all"}
			}
			if len(args) > 1 && (schema != "" || all) {
				// Defensive — targets>1 already catches this, but keep the column-specific message handy.
				return usageError{"columns can only be specified with a single table, not --schema or --all"}
			}

			noteContext(cmd)
			ctx := context.Background()
			conn, release, err := writeConn(ctx, cmd)
			if err != nil {
				return err
			}
			defer release()

			var sql, target string
			var oneTable *catalog.ResolvedTable // set only for a single-table analyze
			var before []catalog.Table          // before-snapshot for multi-table modes
			multiSchema := ""                   // schema scope for the multi-table after-snapshot ("" = all)
			multi := false
			switch {
			case len(args) >= 1:
				rt, err := catalog.ResolveTable(ctx, conn, args[0])
				if err != nil {
					return err
				}
				oneTable = rt
				tableIdent := quoteIdent(rt.Schema) + "." + quoteIdent(rt.Name)
				if len(args) > 1 {
					cols := make([]string, len(args)-1)
					for i, c := range args[1:] {
						cols[i] = quoteIdent(c)
					}
					sql = "ANALYZE " + tableIdent + " (" + strings.Join(cols, ", ") + ")"
					target = fmt.Sprintf("%s.%s (%s)", rt.Schema, rt.Name, strings.Join(args[1:], ", "))
				} else {
					sql = "ANALYZE " + tableIdent
					target = fmt.Sprintf("%s.%s", rt.Schema, rt.Name)
				}
			case schema != "":
				tables, err := catalog.ListTables(ctx, conn, schema)
				if err != nil {
					return err
				}
				if len(tables) == 0 {
					fmt.Fprintf(errOut, "no tables in schema %q — nothing to analyze.\n", schema)
					return nil
				}
				idents := make([]string, len(tables))
				for i, t := range tables {
					idents[i] = quoteIdent(t.Schema) + "." + quoteIdent(t.Name)
				}
				before = tables
				multi = true
				multiSchema = schema
				sql = "ANALYZE " + strings.Join(idents, ", ")
				target = fmt.Sprintf("%d table%s in schema %q", len(tables), plural(len(tables)), schema)
			default: // --all
				tables, err := catalog.ListTables(ctx, conn, "")
				if err != nil {
					return err
				}
				if len(tables) == 0 {
					fmt.Fprintf(errOut, "no tables in database %q — nothing to analyze.\n", conn.Database())
					return nil
				}
				before = tables
				multi = true
				sql = "ANALYZE"
				target = fmt.Sprintf("all %d table%s in database %q", len(tables), plural(len(tables)), conn.Database())
			}

			fmt.Fprintf(errOut, "analyzing %s…\n", target)
			start := time.Now()
			if err := conn.Exec(ctx, sql); err != nil {
				return err
			}
			fmt.Fprintf(errOut, "done in %s.\n", fmtDuration(time.Since(start)))

			switch {
			case oneTable != nil:
				// One table: show the refreshed planner row estimate — the thing ANALYZE
				// recomputes, and what feeds ROWS in `get tables`.
				if after, err := catalog.GetTableStats(ctx, conn, oneTable.OID); err == nil {
					reportAnalyzeRows(errOut, oneTable.EstRows, after.EstRows)
				}
			case multi:
				after, err := catalog.ListTables(ctx, conn, multiSchema)
				if err == nil {
					reportAnalyzeMulti(errOut, before, after, verbose)
				}
			}
			return nil
		},
	}
	c.Flags().StringVar(&schema, "schema", "", "analyze every table in this schema")
	c.Flags().BoolVar(&all, "all", false, "analyze every table in the current database")
	c.Flags().BoolVarP(&verbose, "verbose", "v", false, "show before→after row estimates for every table (default: only notable changes)")
	return c
}

// reportAnalyzeMulti prints per-table row-estimate changes after a schema/--all ANALYZE.
// By default it surfaces only notable changes (newly-populated tables, or ≥10% movement
// with at least 100 rows of delta) and rolls the rest into a tail summary. --verbose
// prints every table's before→after.
func reportAnalyzeMulti(w io.Writer, before, after []catalog.Table, verbose bool) {
	beforeBy := make(map[string]int64, len(before))
	for _, t := range before {
		beforeBy[t.Schema+"."+t.Name] = t.EstRows
	}
	type row struct {
		name          string
		before, after int64
		notable       bool
	}
	rows := make([]row, 0, len(after))
	var notable, unchanged int
	for _, t := range after {
		key := t.Schema + "." + t.Name
		b, ok := beforeBy[key]
		if !ok {
			b = -1 // not seen pre-ANALYZE; treat as never-analyzed
		}
		r := row{name: key, before: b, after: t.EstRows}
		switch {
		case r.after < 0:
			// no after estimate — skip
			continue
		case r.before < 0:
			r.notable = true
		default:
			delta := r.after - r.before
			if delta < 0 {
				delta = -delta
			}
			absBefore := r.before
			if absBefore < 0 {
				absBefore = -absBefore
			}
			if delta >= 100 && delta*10 >= absBefore {
				r.notable = true
			}
		}
		if r.notable {
			notable++
		} else {
			unchanged++
		}
		rows = append(rows, r)
	}

	if notable == 0 && !verbose {
		if unchanged > 0 {
			fmt.Fprintf(w, "row estimates: no notable changes across %d table%s.\n", unchanged, plural(unchanged))
		}
		return
	}

	if verbose {
		fmt.Fprintln(w, "row estimates:")
	} else {
		fmt.Fprintln(w, "notable changes:")
	}
	for _, r := range rows {
		if !verbose && !r.notable {
			continue
		}
		switch {
		case r.before < 0:
			fmt.Fprintf(w, "  %s: now %s rows (was unknown).\n", r.name, withThousands(r.after))
		case r.before == r.after:
			fmt.Fprintf(w, "  %s: %s rows (unchanged).\n", r.name, withThousands(r.after))
		default:
			fmt.Fprintf(w, "  %s: %s → %s rows.\n", r.name, withThousands(r.before), withThousands(r.after))
		}
	}
	if !verbose && unchanged > 0 {
		fmt.Fprintf(w, "  …and %d other table%s unchanged.\n", unchanged, plural(unchanged))
	}
}

// reportAnalyzeRows prints how ANALYZE changed the planner's row estimate (reltuples),
// before → after. reltuples is -1 on a never-analyzed table, so the first analyze reads as
// "now N (was unknown)".
func reportAnalyzeRows(w io.Writer, before, after int64) {
	switch {
	case after < 0:
		return // shouldn't happen right after ANALYZE; nothing meaningful to say
	case before < 0:
		fmt.Fprintf(w, "row estimate: now %s (was unknown — never analyzed).\n", withThousands(after))
	case before != after:
		fmt.Fprintf(w, "row estimate: %s → %s.\n", withThousands(before), withThousands(after))
	default:
		fmt.Fprintf(w, "row estimate: %s (unchanged).\n", withThousands(after))
	}
}
