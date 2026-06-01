package cmd

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/db"
	"github.com/capitaltg/pgdx/internal/render"
)

// query is the read-only escape hatch: run arbitrary SQL with pgdx's table/JSON
// rendering and safety rails, without dropping to psql. It runs inside a READ ONLY
// transaction (so any write — or a data-modifying CTE — is rejected by the server) and
// under the connect-time statement_timeout, keeping it true to pgdx's read-only posture.
func newQueryCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "query <sql>",
		Short: "Run a read-only SQL query with pgdx rendering (READ ONLY transaction)",
		Long: "query runs one SQL statement and renders the result as a table (or -o json), so you\n" +
			"get pgdx's formatting and safety without switching to psql. It executes inside a\n" +
			"READ ONLY transaction that is always rolled back: any attempt to write (INSERT/\n" +
			"UPDATE/DELETE/DDL, or a data-modifying CTE) is refused by Postgres, so this stays\n" +
			"read-only even though the SQL is yours. The standard statement_timeout still applies.\n\n" +
			"Use the global -d/--database to target another database, --sql to echo what runs,\n" +
			"and -o json for scriptable output (values keep their native JSON types).",
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

			res, err := conn.RunReadOnlyQuery(ctx, args[0])
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				return render.Render(cmd.OutOrStdout(), format, res)
			}
			if len(res.Rows) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "(0 rows)")
				return nil
			}
			if err := render.Render(cmd.OutOrStdout(), render.FormatTable, queryResultView{res: res}); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "(%d row%s)\n", len(res.Rows), plural(len(res.Rows)))
			return nil
		},
	}
	return c
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// queryResultView adapts a generic db.QueryResult to the table renderer, stringifying
// each native value (NULL → "—").
type queryResultView struct{ res *db.QueryResult }

func (v queryResultView) Headers() []string { return v.res.Columns }
func (v queryResultView) Rows() [][]string {
	out := make([][]string, 0, len(v.res.Rows))
	for _, row := range v.res.Rows {
		cells := make([]string, len(row))
		for i, val := range row {
			cells[i] = renderValue(val)
		}
		out = append(out, cells)
	}
	return out
}

// renderValue stringifies an arbitrary scanned SQL value for the table view. []byte
// (bytea / unmapped types) becomes its string form; nil becomes the table's null marker.
func renderValue(v any) string {
	switch x := v.(type) {
	case nil:
		return "—"
	case []byte:
		return string(x)
	case string:
		return x
	default:
		return fmt.Sprint(x)
	}
}
