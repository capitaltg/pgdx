package cmd

import (
	"context"
	"fmt"
	"time"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/db"
	"github.com/capitaltg/pgdx/internal/render"
	"github.com/capitaltg/pgdx/internal/snapshot"
)

// snapshot captures Postgres's cumulative stat counters to a local file so `pgdx diff`
// can later subtract two captures. Read-only: it reads catalogs and writes a JSON file
// on the local machine, never to the database.
func newSnapshotCmd() *cobra.Command {
	var label string
	var list bool
	c := &cobra.Command{
		Use:   "snapshot",
		Short: "Capture pg_stat_statements + table stats for later `pgdx diff`",
		Long: "snapshot saves a point-in-time copy of the cumulative statistics counters\n" +
			"(pg_stat_statements and pg_stat_user_tables) to a local file. Those counters are\n" +
			"only meaningful as a DELTA between two readings — `pgdx diff` subtracts two\n" +
			"snapshots to show what actually changed (which queries got slower, which tables\n" +
			"absorbed the writes) since a baseline. Take one now, take another later, then diff.\n\n" +
			"--label tags the capture (e.g. --label before-deploy) so you can reference it later.\n" +
			"--list shows stored snapshots. Snapshots live under $PGDX_STATE_DIR (default\n" +
			"~/.local/state/pgdx/snapshots). Read-only against the database.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			if list {
				return listSnapshots(cmd, format)
			}
			return captureSnapshot(cmd, format, label)
		},
	}
	c.Flags().StringVar(&label, "label", "", "tag for this snapshot (e.g. before-deploy)")
	c.Flags().BoolVar(&list, "list", false, "list stored snapshots instead of capturing one")
	return c
}

// collectSnapshot reads the live cumulative counters into a Snapshot (without saving).
// Shared by `pgdx snapshot` and `pgdx diff <baseline>` (which captures a live reading to
// compare against). Statement capture is best-effort so it still works without the
// pg_stat_statements extension.
func collectSnapshot(ctx context.Context, cmd *cobra.Command, conn *db.Conn, label string) (*snapshot.Snapshot, error) {
	snap := &snapshot.Snapshot{
		Label:    label,
		Database: conn.Database(),
		Host:     conn.Host(),
		Port:     conn.Port(),
		TakenAt:  time.Now(),
	}
	if avail, _ := catalog.PgStatStatementsAvailable(ctx, conn); avail {
		stmts, err := catalog.SnapshotStatements(ctx, conn)
		if err != nil {
			return nil, err
		}
		snap.Statements = stmts
	} else {
		fmt.Fprintln(cmd.ErrOrStderr(), "note: pg_stat_statements not available — capturing table stats only (no per-query diff).")
	}
	tables, err := catalog.SnapshotTables(ctx, conn)
	if err != nil {
		return nil, err
	}
	snap.Tables = tables
	return snap, nil
}

func captureSnapshot(cmd *cobra.Command, format render.Format, label string) error {
	noteContext(cmd)
	ctx := context.Background()
	conn, release, err := dial(ctx, cmd, flagDatabase)
	if err != nil {
		return err
	}
	defer release()

	snap, err := collectSnapshot(ctx, cmd, conn, label)
	if err != nil {
		return err
	}

	path, err := snapshot.Save(snap)
	if err != nil {
		return err
	}
	if format == render.FormatJSON {
		return render.Render(cmd.OutOrStdout(), format, map[string]any{
			"path": path, "statements": len(snap.Statements), "tables": len(snap.Tables),
			"database": snap.Database, "taken_at": snap.TakenAt,
		})
	}
	fmt.Fprintf(cmd.OutOrStdout(), "captured %d statements and %d tables from %q → %s\n",
		len(snap.Statements), len(snap.Tables), snap.Database, path)
	fmt.Fprintln(cmd.ErrOrStderr(), "take another later, then compare with: pgdx diff")
	return nil
}

func listSnapshots(cmd *cobra.Command, format render.Format) error {
	ents, err := snapshot.List()
	if err != nil {
		return err
	}
	if format == render.FormatJSON {
		names := make([]string, 0, len(ents))
		for _, e := range ents {
			names = append(names, e.Name)
		}
		return render.Render(cmd.OutOrStdout(), format, names)
	}
	if len(ents) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "no snapshots yet — capture one with `pgdx snapshot`")
		return nil
	}
	for _, e := range ents {
		fmt.Fprintln(cmd.OutOrStdout(), e.Name)
	}
	return nil
}
