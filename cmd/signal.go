package cmd

import (
	"context"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
)

// cancel and kill act on a backend PID. PIDs come from get activity (PID column),
// get locks (PID + BLOCKED-BY), or get connections --detail. Both are guarded writes;
// PIDs are cluster-global so they work regardless of which database pgdx connected to.

func newCancelCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "cancel <pid>",
		Short: "Cancel the running query on a backend (gentle; session survives)",
		Long: "cancel runs pg_cancel_backend — it interrupts the CURRENT query on a backend,\n" +
			"like Ctrl-C. The session stays connected and can retry; recoverable. For a backend\n" +
			"that's idle (e.g. idle in transaction, holding locks) use `kill` instead — there's\n" +
			"no running query to cancel.\n\n" +
			"Find PIDs with: pgdx get activity, pgdx get locks (incl BLOCKED-BY), or\n" +
			"pgdx get connections --detail. Needs superuser or pg_signal_backend for other\n" +
			"users' backends.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSignal(cmd, args[0], false, false)
		},
	}
}

func newKillCmd() *cobra.Command {
	var force bool
	c := &cobra.Command{
		Use:     "kill <pid>",
		Aliases: []string{"terminate"},
		Short:   "Terminate a backend (drops the connection, rolls back its transaction)",
		Long: "kill runs pg_terminate_backend — it ends the whole session, dropping the\n" +
			"connection and rolling back its transaction. Use it for stuck/idle-in-transaction\n" +
			"backends that `cancel` can't help. Asks for confirmation (--force to skip).\n\n" +
			"Find PIDs with: pgdx get activity, pgdx get locks (incl BLOCKED-BY), or\n" +
			"pgdx get connections --detail. Needs superuser or pg_signal_backend for other\n" +
			"users' backends.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSignal(cmd, args[0], true, force)
		},
	}
	c.Flags().BoolVar(&force, "force", false, "skip the confirmation prompt")
	return c
}

// backendIsRunning reports whether a pg_stat_activity state means a query is executing
// (so there's something to cancel). Idle states have no running query.
func backendIsRunning(state string) bool {
	return state == "active" || state == "fastpath function call"
}

// runSignal looks up the target backend, shows who it is, then cancels (terminate=false)
// or terminates (terminate=true) it. Terminate confirms unless force.
func runSignal(cmd *cobra.Command, pidArg string, terminate, force bool) error {
	pid64, err := strconv.ParseInt(pidArg, 10, 32)
	if err != nil || pid64 <= 0 {
		return usageError{fmt.Sprintf("PID must be a positive integer, got %q", pidArg)}
	}
	pid := int32(pid64)
	e := cmd.ErrOrStderr()

	noteContext(cmd)
	ctx := context.Background()
	// Route through dial so a shell session reuses its connection (cancel/kill act on
	// cluster-global PIDs, so any database works, but reusing the session's is consistent
	// with every other command).
	conn, release, err := dial(ctx, cmd, flagDatabase)
	if err != nil {
		return err
	}
	defer release()

	// Refuse to signal pgdx's own backend.
	if self, err := catalog.CurrentBackendPID(ctx, conn); err == nil && pid == self {
		return fmt.Errorf("PID %d is pgdx's own backend — refusing to signal it", pid)
	}

	// Show the target so the user can confirm they've got the right PID.
	info, found, err := catalog.BackendInfo(ctx, conn, pid)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("no backend with PID %d (it may have already ended)", pid)
	}
	fmt.Fprintf(e, "Target PID %d:\n  user=%s  db=%s  state=%s\n  query: %s\n",
		pid, dashIfEmpty(info.User), dashIfEmpty(info.Database), dashIfEmpty(info.State),
		compactQuery(info.Query, 80))

	// cancel only interrupts a RUNNING query. If the backend is idle there's nothing to
	// cancel — say so rather than reporting a signal that did nothing. (kill/terminate
	// still applies to idle backends — e.g. idle-in-transaction holding locks.)
	if !terminate && !backendIsRunning(info.State) {
		fmt.Fprintf(e, "backend %d is %s — no running query to cancel; nothing to do.\n",
			pid, dashIfEmpty(info.State))
		fmt.Fprintln(e, "(the query shown above is its LAST statement, not a running one. Use `kill` to terminate the session itself.)")
		return nil
	}

	if terminate && !force {
		ok, err := promptConfirm(cmd, fmt.Sprintf(
			"Terminate backend %d? This drops its connection and rolls back its transaction. [y/N]: ", pid))
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(e, "aborted.")
			return nil
		}
	}

	var delivered bool
	if terminate {
		delivered, err = catalog.TerminateBackend(ctx, conn, pid)
	} else {
		delivered, err = catalog.CancelBackend(ctx, conn, pid)
	}
	if err != nil {
		return err // permission errors surface here
	}
	switch {
	case !delivered:
		fmt.Fprintf(e, "signal not delivered to PID %d (it may have just ended)\n", pid)
	case terminate:
		fmt.Fprintf(e, "terminated backend %d\n", pid)
	default:
		fmt.Fprintf(e, "cancel signal sent to backend %d\n", pid)
	}
	return nil
}
