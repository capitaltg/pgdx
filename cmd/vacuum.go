package cmd

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/db"
)

// vacuum is one of pgdx's few WRITE commands. It's defensible against the read-only
// posture because plain VACUUM/ANALYZE are non-destructive (they reclaim dead-tuple
// space and refresh stats, never changing data). --full is the exception: it rewrites
// the table under an ACCESS EXCLUSIVE lock, so it requires explicit confirmation.
func newVacuumCmd() *cobra.Command {
	var analyze, full, force bool
	c := &cobra.Command{
		Use:   "vacuum <table>",
		Short: "Reclaim dead tuples on a table (VACUUM)",
		Long: "vacuum runs VACUUM on a table to reclaim dead-tuple space (see the DEAD% column\n" +
			"in `get tables`). Plain VACUUM and --analyze are online and non-destructive.\n\n" +
			"--full rewrites the table to return disk to the OS, but takes an ACCESS EXCLUSIVE\n" +
			"lock that blocks ALL reads and writes until it finishes — it asks for confirmation\n" +
			"(skip with --force). Accepts a bare name or schema.name.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			errOut := cmd.ErrOrStderr()
			noteContext(cmd)
			ctx := context.Background()
			conn, release, err := writeConn(ctx, cmd)
			if err != nil {
				return err
			}
			defer release()

			rt, err := catalog.ResolveTable(ctx, conn, args[0])
			if err != nil {
				return err
			}

			if full && !force {
				ok, err := promptConfirm(cmd, fmt.Sprintf(
					"VACUUM FULL takes an ACCESS EXCLUSIVE lock and blocks ALL access to %s.%s while it runs. Proceed? [y/N]: ",
					rt.Schema, rt.Name))
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(errOut, "aborted.")
					return nil
				}
			}

			sql := vacuumSQL(quoteIdent(rt.Schema)+"."+quoteIdent(rt.Name), full, analyze)
			fmt.Fprintf(errOut, "running: %s\n", sql)

			before, _ := catalog.GetTableStats(ctx, conn, rt.OID) // best-effort baseline
			start := time.Now()
			if err := conn.Exec(ctx, sql); err != nil {
				return err
			}
			elapsed := time.Since(start)

			fmt.Fprintf(errOut, "done in %s.\n", fmtDuration(elapsed))
			if after, err := catalog.GetTableStats(ctx, conn, rt.OID); err == nil {
				reportVacuumOutcome(errOut, before, after, full)
			}
			fmt.Fprintf(errOut, "Re-check with: pgdx describe table %s.%s\n", rt.Schema, rt.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&analyze, "analyze", false, "also ANALYZE (refresh planner statistics)")
	c.Flags().BoolVar(&full, "full", false, "VACUUM FULL — rewrites the table, ACCESS EXCLUSIVE lock (asks to confirm)")
	c.Flags().BoolVar(&force, "force", false, "skip the --full confirmation prompt")
	return c
}

// reportVacuumOutcome summarizes what the VACUUM changed.
//
// For VACUUM FULL the table is rewritten, so disk freed is the meaningful (and reliable)
// signal — pg_stat's dead-tuple counter isn't updated promptly after a rewrite, so we do
// NOT infer "reclaimed" from it here (doing so produced a bogus "nothing removable" line
// even when the rewrite plainly shrank the table).
//
// For a plain VACUUM the dead-tuple delta is reliable: it reports how many were reclaimed,
// and when dead tuples were present but none were removable it names the usual cause —
// rows still visible to an open transaction or pinned by a replication slot — so a "done"
// that didn't actually help explains itself. Plain VACUUM can also truncate trailing empty
// pages, so any size freed is reported too.
func reportVacuumOutcome(w io.Writer, before, after catalog.TableStats, full bool) {
	if full {
		if freed := before.SizeBytes - after.SizeBytes; freed > 0 {
			fmt.Fprintf(w, "rewrote the table; size %s → %s (freed %s).\n",
				humanBytes(before.SizeBytes), humanBytes(after.SizeBytes), humanBytes(freed))
		} else {
			fmt.Fprintf(w, "rewrote the table; size %s (nothing to reclaim).\n", humanBytes(after.SizeBytes))
		}
		return
	}
	switch reclaimed := before.DeadTup - after.DeadTup; {
	case reclaimed > 0:
		fmt.Fprintf(w, "reclaimed %s dead tuple%s (%s remaining).\n",
			withThousands(reclaimed), plural(int(reclaimed)), withThousands(after.DeadTup))
	case before.DeadTup > 0:
		fmt.Fprintf(w, "%s dead tuple%s remain — none were removable. They're likely still visible to an\n"+
			"open transaction or pinned by a replication slot; check `pgdx get transaction-age` and `pgdx status`.\n",
			withThousands(after.DeadTup), plural(int(after.DeadTup)))
	}
	if freed := before.SizeBytes - after.SizeBytes; freed > 0 {
		fmt.Fprintf(w, "size %s → %s (freed %s).\n",
			humanBytes(before.SizeBytes), humanBytes(after.SizeBytes), humanBytes(freed))
	}
}

// fmtDuration renders a VACUUM's elapsed time at a sensible precision: milliseconds under
// a second, tenths under a minute, whole seconds beyond.
func fmtDuration(d time.Duration) string {
	switch {
	case d < time.Second:
		return d.Round(time.Millisecond).String()
	case d < time.Minute:
		return d.Round(100 * time.Millisecond).String()
	default:
		return d.Round(time.Second).String()
	}
}

// writeConn opens the connection for a write command (vacuum, analyze): the session's
// database with no statement_timeout — these can run far longer than D6's default cap —
// reusing the shell session's already-resolved target when inside `pgdx shell` (where the
// per-command flags have been reset, so re-resolving from them would land on the wrong
// database). Returns the connection and a release func.
func writeConn(ctx context.Context, cmd *cobra.Command) (*db.Conn, func(), error) {
	var conn *db.Conn
	var err error
	if sharedConn != nil {
		conn, err = sharedConn.ConnectWithoutTimeout(ctx)
	} else {
		conn, err = db.Connect(ctx, flagDSN, "0", flagDatabase, sqlLog(cmd))
	}
	if err != nil {
		return nil, nil, err
	}
	// Register this connection as the shell's Ctrl-C cancel target for its lifetime, so an
	// interactive Ctrl-C cancels a long VACUUM/ANALYZE running on it (no-op outside a shell).
	setActiveCancel(conn)
	return conn, func() {
		setActiveCancel(nil)
		_ = conn.Close(ctx)
	}, nil
}

// vacuumSQL builds the statement. Options use the parenthesized form (PG9+).
func vacuumSQL(ident string, full, analyze bool) string {
	var opts []string
	if full {
		opts = append(opts, "FULL")
	}
	if analyze {
		opts = append(opts, "ANALYZE")
	}
	if len(opts) > 0 {
		return fmt.Sprintf("VACUUM (%s) %s", strings.Join(opts, ", "), ident)
	}
	return "VACUUM " + ident
}

// quoteIdent double-quotes a SQL identifier, escaping embedded quotes — VACUUM takes a
// table name in the command text (not a bind parameter), so this prevents injection and
// handles mixed-case / special-character names.
func quoteIdent(s string) string {
	return `"` + strings.ReplaceAll(s, `"`, `""`) + `"`
}

// promptConfirm reads a yes/no answer from stdin. A non-terminal / empty answer is "no"
// (safe default), so piping without --force aborts rather than proceeding.
func promptConfirm(cmd *cobra.Command, prompt string) (bool, error) {
	fmt.Fprint(cmd.ErrOrStderr(), prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return false, nil
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	default:
		return false, nil
	}
}
