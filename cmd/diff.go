package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/render"
	"github.com/capitaltg/pgdx/internal/snapshot"
)

// diff subtracts two stat snapshots and shows what changed in between — the per-query
// and per-table movers. Cumulative counters are only meaningful as a delta, so this is
// where snapshots pay off. Read-only.
func newDiffCmd() *cobra.Command {
	var limit int
	c := &cobra.Command{
		Use:   "diff [baseline] [later]",
		Short: "Show what changed between two stat snapshots (top movers)",
		Long: "diff subtracts two `pgdx snapshot` captures and reports the biggest movers: which\n" +
			"queries added the most execution time, and which tables took the most writes, in\n" +
			"the interval between them. This is the answer to 'what got slow since this morning'\n" +
			"that raw pg_stat_statements totals can't give.\n\n" +
			"With no arguments it diffs the two most recent snapshots. With one argument it diffs\n" +
			"that snapshot against a fresh live reading (baseline → now). With two it diffs the\n" +
			"named snapshots (baseline → later). Reference a snapshot by full name (see\n" +
			"`pgdx snapshot --list`) or a unique substring of its label. Read-only.",
		Args: cobra.MaximumNArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			older, newer, err := resolveDiffPair(cmd, args)
			if err != nil {
				return err
			}
			if older.Database != newer.Database {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"warning: comparing snapshots of different databases (%q vs %q) — the diff may be meaningless.\n",
					older.Database, newer.Database)
			}
			return renderDiff(cmd, format, older, newer, limit)
		},
	}
	c.Flags().IntVar(&limit, "limit", 20, "max rows to show per section")
	return c
}

// resolveDiffPair turns 0/1/2 positional args into the (older, newer) snapshot pair.
func resolveDiffPair(cmd *cobra.Command, args []string) (older, newer *snapshot.Snapshot, err error) {
	switch len(args) {
	case 0:
		return snapshot.LatestTwo()
	case 1:
		older, err = snapshot.Load(args[0])
		if err != nil {
			return nil, nil, err
		}
		newer, err = captureLiveForDiff(cmd)
		return older, newer, err
	default: // 2
		older, err = snapshot.Load(args[0])
		if err != nil {
			return nil, nil, err
		}
		newer, err = snapshot.Load(args[1])
		return older, newer, err
	}
}

// captureLiveForDiff takes a fresh, unsaved live reading to compare a baseline against.
func captureLiveForDiff(cmd *cobra.Command) (*snapshot.Snapshot, error) {
	noteContext(cmd)
	ctx := context.Background()
	conn, release, err := dial(ctx, cmd, flagDatabase)
	if err != nil {
		return nil, err
	}
	defer release()
	fmt.Fprintln(cmd.ErrOrStderr(), "comparing baseline against a fresh live reading...")
	return collectSnapshot(ctx, cmd, conn, "live")
}

func renderDiff(cmd *cobra.Command, format render.Format, older, newer *snapshot.Snapshot, limit int) error {
	stmts := snapshot.DiffStatements(older, newer)
	tables := snapshot.DiffTables(older, newer)

	if format == render.FormatJSON {
		return render.Render(cmd.OutOrStdout(), format, map[string]any{
			"from":       older.TakenAt,
			"to":         newer.TakenAt,
			"statements": capStmts(stmts, limit),
			"tables":     capTables(tables, limit),
		})
	}

	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "Diff %s → %s (%s elapsed)\n",
		older.TakenAt.Format("2006-01-02 15:04:05"), newer.TakenAt.Format("2006-01-02 15:04:05"),
		newer.TakenAt.Sub(older.TakenAt).Round(1e9))

	fmt.Fprintln(out, "\nQuery changes (by added execution time):")
	if len(stmts) == 0 {
		fmt.Fprintln(out, "  (no query activity in the interval, or no pg_stat_statements data)")
	} else if err := render.Render(out, render.FormatTable, stmtDiffView(capStmts(stmts, limit))); err != nil {
		return err
	}

	fmt.Fprintln(out, "\nTable changes (by writes):")
	if len(tables) == 0 {
		fmt.Fprintln(out, "  (no table write/scan activity in the interval)")
	} else if err := render.Render(out, render.FormatTable, tableDiffView(capTables(tables, limit))); err != nil {
		return err
	}
	return nil
}

func capStmts(s []snapshot.StmtDelta, n int) []snapshot.StmtDelta {
	if n > 0 && len(s) > n {
		return s[:n]
	}
	return s
}
func capTables(t []snapshot.TableDelta, n int) []snapshot.TableDelta {
	if n > 0 && len(t) > n {
		return t[:n]
	}
	return t
}

type stmtDiffView []snapshot.StmtDelta

func (v stmtDiffView) Headers() []string {
	return []string{"ΔCALLS", "ΔTOTAL", "MEAN", "ΔROWS", "ΔREADS", "NEW", "QUERY"}
}
func (v stmtDiffView) Aligns() []render.Align {
	return []render.Align{
		render.AlignRight, render.AlignRight, render.AlignRight, render.AlignRight,
		render.AlignRight, render.AlignLeft, render.AlignLeft,
	}
}
func (v stmtDiffView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, d := range v {
		out = append(out, []string{
			withThousands(d.Calls), msHuman(d.TotalMs), msHuman(d.MeanMs),
			withThousands(d.Rows), withThousands(d.SharedRead),
			yesNo(d.IsNew), compactQuery(d.Query, 50),
		})
	}
	return out
}

type tableDiffView []snapshot.TableDelta

func (v tableDiffView) Headers() []string {
	return []string{"SCHEMA", "NAME", "ΔWRITES", "ΔINS", "ΔUPD", "ΔDEL", "ΔSEQ-SCAN", "ΔIDX-SCAN"}
}
func (v tableDiffView) Aligns() []render.Align {
	return []render.Align{
		render.AlignLeft, render.AlignLeft, render.AlignRight, render.AlignRight,
		render.AlignRight, render.AlignRight, render.AlignRight, render.AlignRight,
	}
}
func (v tableDiffView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, d := range v {
		out = append(out, []string{
			d.Schema, d.Name, withThousands(d.Writes), withThousands(d.Ins),
			withThousands(d.Upd), withThousands(d.Del), withThousands(d.SeqScan), withThousands(d.IdxScan),
		})
	}
	return out
}
