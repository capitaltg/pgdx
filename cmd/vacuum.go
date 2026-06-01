package cmd

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

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
			// Disable statement_timeout (D6's default would cancel a long VACUUM).
			conn, err := db.Connect(ctx, flagDSN, "0", flagDatabase, sqlLog(cmd))
			if err != nil {
				return err
			}
			defer conn.Close(ctx)

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
			if err := conn.Exec(ctx, sql); err != nil {
				return err
			}
			fmt.Fprintf(errOut, "done. Re-check with: pgdx describe table %s.%s\n", rt.Schema, rt.Name)
			return nil
		},
	}
	c.Flags().BoolVar(&analyze, "analyze", false, "also ANALYZE (refresh planner statistics)")
	c.Flags().BoolVar(&full, "full", false, "VACUUM FULL — rewrites the table, ACCESS EXCLUSIVE lock (asks to confirm)")
	c.Flags().BoolVar(&force, "force", false, "skip the --full confirmation prompt")
	return c
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
