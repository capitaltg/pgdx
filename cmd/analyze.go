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
	c := &cobra.Command{
		Use:   "analyze [table]",
		Short: "Refresh planner statistics (ANALYZE) for a table, a schema, or the whole database",
		Long: "analyze runs ANALYZE to refresh the planner's statistics — the row estimates and\n" +
			"column distributions it relies on, and what populates ROWS/DEAD% in `get tables`.\n" +
			"Run it after a restore or a major data load, or when `explain` flags a row-estimate\n" +
			"that looks stale. ANALYZE only samples rows (it does not scan the whole table) and\n" +
			"takes a SHARE UPDATE EXCLUSIVE lock, so it runs online without blocking reads or\n" +
			"writes.\n\n" +
			"Pick exactly one target:\n" +
			"  pgdx analyze public.orders     one table (bare name or schema.name)\n" +
			"  pgdx analyze --schema app      every table in a schema\n" +
			"  pgdx analyze --all             every table in the current database\n\n" +
			"Whole-database analyze is deliberate (--all), never the default, so it can't be\n" +
			"triggered by an accidental bare command.",
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			errOut := cmd.ErrOrStderr()

			// Exactly one of {table, --schema, --all} must be chosen.
			targets := 0
			if len(args) == 1 {
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

			noteContext(cmd)
			ctx := context.Background()
			conn, release, err := writeConn(ctx, cmd)
			if err != nil {
				return err
			}
			defer release()

			var sql, target string
			var oneTable *catalog.ResolvedTable // set only for a single-table analyze
			var total, missing int              // multi-table summary (schema / --all)
			switch {
			case len(args) == 1:
				rt, err := catalog.ResolveTable(ctx, conn, args[0])
				if err != nil {
					return err
				}
				oneTable = rt
				sql = "ANALYZE " + quoteIdent(rt.Schema) + "." + quoteIdent(rt.Name)
				target = fmt.Sprintf("%s.%s", rt.Schema, rt.Name)
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
					if t.EstRows < 0 {
						missing++
					}
				}
				total = len(tables)
				sql = "ANALYZE " + strings.Join(idents, ", ")
				target = fmt.Sprintf("%d table%s in schema %q", total, plural(total), schema)
			default: // --all
				t, m, err := catalog.CountTablesForAnalyze(ctx, conn, "")
				if err != nil {
					return err
				}
				if t == 0 {
					fmt.Fprintf(errOut, "no tables in database %q — nothing to analyze.\n", conn.Database())
					return nil
				}
				total, missing = t, m
				sql = "ANALYZE"
				target = fmt.Sprintf("all %d table%s in database %q", total, plural(total), conn.Database())
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
			case missing > 0:
				// Many tables: the useful detail is how many had no statistics before — the
				// "did this fix my blank `get tables`" answer after a restore.
				fmt.Fprintf(errOut, "%d of them had no statistics before — now populated.\n", missing)
			}
			return nil
		},
	}
	c.Flags().StringVar(&schema, "schema", "", "analyze every table in this schema")
	c.Flags().BoolVar(&all, "all", false, "analyze every table in the current database")
	return c
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
