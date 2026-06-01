package cmd

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/db"
	"github.com/capitaltg/pgdx/internal/render"
)

// summarize is the at-a-glance inventory of one database: object counts, a size
// breakdown, index-health and bloat rollups, and the largest tables — what `status`
// (health) and `get databases` (cluster-wide) don't give. Read-only; defaults to the
// connected database, and the global -d/--database targets another one.
func newSummarizeCmd() *cobra.Command {
	var top int
	c := &cobra.Command{
		Use:   "summarize",
		Short: "One-screen inventory of a database: object counts, sizes, index health",
		Long: "summarize gives a single-screen overview of the connected database: how many\n" +
			"tables, views, indexes, sequences, functions, and extensions it has; a size\n" +
			"breakdown (total, plus user table vs index bytes); an index-health rollup (unused\n" +
			"and redundant counts); an estimate of reclaimable bloat; and the largest tables.\n" +
			"It composes existing read-only catalog queries — no new load beyond a few cheap\n" +
			"aggregates.\n\n" +
			"It summarizes the database pgdx connects to; use the global -d/--database to point\n" +
			"it at another one (needs CONNECT). Use -o json for the full structured figures.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			ctx := context.Background()
			defer conn.Close(ctx)

			sum, err := catalog.SummarizeDatabase(ctx, conn, top)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				return render.Render(cmd.OutOrStdout(), format, sum)
			}
			printSummary(cmd, conn, sum)
			return nil
		},
	}
	c.Flags().IntVar(&top, "top", 5, "how many of the largest tables to list")
	return c
}

// printSummary renders the human (stacked) overview. The data goes to stdout (D4); the
// closing drill-down pointers go to stderr.
func printSummary(cmd *cobra.Command, conn *db.Conn, s *catalog.DatabaseSummary) {
	out := cmd.OutOrStdout()
	host := conn.Host()
	if host == "" {
		host = "local"
	}
	fmt.Fprintf(out, "Database %q @ %s:%d", s.Database, host, conn.Port())
	if s.Encoding != "" {
		fmt.Fprintf(out, " — encoding %s", s.Encoding)
	}
	fmt.Fprintln(out)

	printSummaryBody(out, s)

	// One-line pointers to the drill-down commands, on stderr so stdout stays clean.
	e := cmd.ErrOrStderr()
	if s.UnusedIndexes > 0 || s.RedundantIndexes > 0 {
		fmt.Fprintln(e, "→ drop candidates: `pgdx get indexes --unused` and `pgdx get indexes --redundant`")
	}
	if s.EstBloatBytes > 0 {
		fmt.Fprintln(e, "→ where vacuuming pays off: `pgdx get bloat`")
	}
}

// printSummaryBody writes the size breakdown, object counts, health rollups, and largest
// tables to w. Split out from printSummary (which adds the connection-specific header and
// stderr pointers) so it's testable without a database connection.
func printSummaryBody(w io.Writer, s *catalog.DatabaseSummary) {
	out := w
	fmt.Fprintf(out, "Size: %s   (user objects: %s tables + %s indexes)\n",
		humanBytes(s.SizeBytes), humanBytes(s.TableBytes), humanBytes(s.IndexBytes))

	// Object counts as an aligned two-column list, with parenthetical detail where useful.
	fmt.Fprintln(out, "\nObjects:")
	type row struct{ label, val, note string }
	rows := []row{
		{"Schemas", withThousands(s.Schemas), ""},
		{"Tables", withThousands(s.Tables), partitionedNote(s.Partitioned)},
		{"Views", withThousands(s.Views), matviewNote(s.MaterializedViews)},
		{"Indexes", withThousands(s.Indexes), indexHealthNote(s)},
		{"Sequences", withThousands(s.Sequences), ""},
		{"Functions", withThousands(s.Functions), ""},
		{"Extensions", withThousands(s.Extensions), ""},
	}
	labelW, valW := 0, 0
	for _, r := range rows {
		if len(r.label) > labelW {
			labelW = len(r.label)
		}
		if len(r.val) > valW {
			valW = len(r.val)
		}
	}
	for _, r := range rows {
		fmt.Fprintf(out, "  %-*s  %*s", labelW, r.label, valW, r.val)
		if r.note != "" {
			fmt.Fprintf(out, "   %s", r.note)
		}
		fmt.Fprintln(out)
	}

	// Health rollups.
	fmt.Fprintf(out, "\nIndex health: %s unused (%s), %s redundant\n",
		withThousands(s.UnusedIndexes), humanBytes(s.UnusedIndexBytes), withThousands(s.RedundantIndexes))
	fmt.Fprintf(out, "Estimated bloat: ~%s reclaimable\n", humanBytes(s.EstBloatBytes))

	if len(s.TopTables) > 0 {
		fmt.Fprintln(out, "\nLargest tables:")
		render.Render(out, render.FormatTable, topTablesView(s.TopTables))
	}
}

func partitionedNote(n int64) string {
	if n > 0 {
		return fmt.Sprintf("(%s partitioned)", withThousands(n))
	}
	return ""
}

// matviewNote uses a leading "+" because materialized views are counted separately from
// the plain-view total (unlike partitioned tables, which ARE a subset of the table count).
func matviewNote(n int64) string {
	if n > 0 {
		return fmt.Sprintf("(+%s materialized)", withThousands(n))
	}
	return ""
}

func indexHealthNote(s *catalog.DatabaseSummary) string {
	if s.UnusedIndexes == 0 && s.RedundantIndexes == 0 {
		return ""
	}
	return fmt.Sprintf("(%s unused · %s redundant)", withThousands(s.UnusedIndexes), withThousands(s.RedundantIndexes))
}

type topTablesView []catalog.TableSize

func (v topTablesView) Headers() []string { return []string{"SCHEMA", "NAME", "SIZE"} }
func (v topTablesView) Aligns() []render.Align {
	return []render.Align{render.AlignLeft, render.AlignLeft, render.AlignRight}
}
func (v topTablesView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, t := range v {
		out = append(out, []string{t.Schema, t.Name, humanBytes(t.SizeBytes)})
	}
	return out
}
