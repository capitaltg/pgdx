// Package cmd wires the pgdx command tree.
//
// The root command owns the persistent flags, the output-format flag, and the
// stdout/stderr contract; each subcommand (explain, status, summarize, get,
// describe, query, snapshot, diff, vacuum, cancel, kill, config) is built by its
// own newXxxCmd constructor and registered in newRootCmd.
package cmd

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// version is overridden at build time via -ldflags.
var version = "0.1.0-dev"

// Persistent flags shared by all subcommands.
var (
	flagDSN      string
	flagOutput   string
	flagDatabase string
	flagSQL      bool
	flagTimeout  string
)

// sqlLog returns the writer that --sql echoes statements to (stderr, so it never mixes
// with -o json on stdout), or nil when the flag is off.
func sqlLog(cmd *cobra.Command) io.Writer {
	if flagSQL {
		return cmd.ErrOrStderr()
	}
	return nil
}

// noteContext applies pgdx's default context (when no explicit --dsn / $PGSERVICE is set)
// and reports to stderr what pgdx is connecting to. It always surfaces the global
// -d/--database when set, so `-d` is never invisible behind a context name (the original
// message named only the context, which made `-d other_db` look ignored).
func noteContext(cmd *cobra.Command) {
	applied := applyDefaultContext(flagDSN)
	e := cmd.ErrOrStderr()
	switch {
	case applied != "" && flagDatabase != "":
		fmt.Fprintf(e, "using default context %q (database %q via -d)\n", applied, flagDatabase)
	case applied != "":
		fmt.Fprintf(e, "using default context %q\n", applied)
	case flagDatabase != "":
		fmt.Fprintf(e, "connecting to database %q (via -d)\n", flagDatabase)
	}
}

func newRootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "pgdx",
		Short: "pgdx — a kubectl-style CLI for Postgres",
		Long: "pgdx brings a consistent, scriptable command grammar to Postgres: browse\n" +
			"your schema, see what the server is doing right now, and diagnose slow\n" +
			"queries. Read-only by default — `pgdx explain` reads a query plan and tells\n" +
			"you, in plain language, what's expensive without executing your query.",
		Version:       version,
		SilenceUsage:  true, // we control when usage prints (exit code 2 path)
		SilenceErrors: true, // main() prints errors to stderr itself
	}
	root.PersistentFlags().StringVar(&flagDSN, "dsn", "",
		"connection string or postgres:// URI (default: PG* env vars / .pgpass, like psql)")
	root.PersistentFlags().StringVarP(&flagOutput, "output", "o", "table",
		"output format: table | json")
	root.PersistentFlags().StringVarP(&flagDatabase, "database", "d", "",
		"connect to this database instead of the default (needs CONNECT on it)")
	root.PersistentFlags().BoolVar(&flagSQL, "sql", false,
		"print the SQL pgdx runs (to stderr) — handy for learning or copying the query")
	root.PersistentFlags().StringVar(&flagTimeout, "timeout", "",
		"per-query statement_timeout, e.g. 2m or 120s (default 15s); raise it for a slow explain --analyze")

	root.AddCommand(newExplainCmd())
	root.AddCommand(newStatusCmd())
	root.AddCommand(newGetCmd())
	root.AddCommand(newDescribeCmd())
	root.AddCommand(newSummarizeCmd())
	root.AddCommand(newQueryCmd())
	root.AddCommand(newSnapshotCmd())
	root.AddCommand(newDiffCmd())
	root.AddCommand(newVacuumCmd())
	root.AddCommand(newCancelCmd())
	root.AddCommand(newKillCmd())
	root.AddCommand(newConfigCmd())
	return root
}

// Execute runs the root command and returns a process exit code. Errors are
// printed to stderr here (cobra's own printing is silenced) so the stdout/stderr
// contract (D4) stays clean: only command data ever reaches stdout.
func Execute() int {
	root := newRootCmd()
	if err := root.Execute(); err != nil {
		fmt.Fprintln(root.ErrOrStderr(), "error:", err)
		if ec, ok := err.(exitCoder); ok {
			return ec.ExitCode()
		}
		return 1
	}
	return 0
}
