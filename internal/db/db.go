// Package db owns the Postgres connection for pgdx.
//
// Connection (decided direction): pgx/v5, pure Go, honoring the standard
// libpq-compatible inputs — PG* env vars, a postgres:// URI, and .pgpass. No
// custom connection contexts in v0.1.
//
// Self-limiting (D6): pgdx points at databases that are already under stress, so
// it sets a conservative statement_timeout on its OWN session. The diagnostic tool
// must never become the query that hangs.
package db

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/jackc/pgx/v5"
)

// DefaultStatementTimeout caps how long any pgdx query may run (D6).
const DefaultStatementTimeout = "15s"

// Conn wraps a pgx connection with pgdx's policies applied.
type Conn struct {
	conn *pgx.Conn
	// sqlLog, when non-nil, receives every statement pgdx runs (the --sql flag). Goes
	// to stderr so it never pollutes -o json on stdout.
	sqlLog io.Writer
}

// logSQL echoes a statement (and any bind args) to the --sql writer, if enabled.
func (c *Conn) logSQL(sql string, args ...any) {
	if c.sqlLog == nil {
		return
	}
	fmt.Fprintf(c.sqlLog, "\n--- pgdx SQL ---\n%s\n", strings.TrimSpace(sql))
	if len(args) > 0 {
		fmt.Fprintf(c.sqlLog, "-- args: %v\n", args)
	}
}

// Connect opens a connection. dsn may be empty, in which case pgx resolves the
// target from PG* env vars / .pgpass exactly like libpq. A non-empty dsn is a
// postgres:// URI or key=value connection string. A non-empty database overrides the
// target database (after env/service/dsn are resolved), so `--database` wins over a
// context's dbname — Postgres can't cross-query databases, so this connects there.
func Connect(ctx context.Context, dsn, statementTimeout, database string, sqlLog io.Writer) (*Conn, error) {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse connection config: %w", err)
	}
	if database != "" {
		cfg.Database = database
	}
	if statementTimeout == "" {
		statementTimeout = DefaultStatementTimeout
	}
	// Apply the self-limit at connection time so it covers every query (D6).
	if cfg.RuntimeParams == nil {
		cfg.RuntimeParams = map[string]string{}
	}
	cfg.RuntimeParams["statement_timeout"] = statementTimeout

	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Conn{conn: conn, sqlLog: sqlLog}, nil
}

// ConnectDatabase opens a new connection to a different database on the SAME server,
// reusing this connection's already-resolved settings — host, port, user, auth, and the
// statement_timeout (D6) — and only swapping the target database. The shell's `use`
// command uses it to repoint a session without re-resolving env/$PGSERVICE/.pgpass. The
// caller owns the returned connection (and still owns the receiver).
func (c *Conn) ConnectDatabase(ctx context.Context, database string) (*Conn, error) {
	cfg := c.conn.Config().Copy()
	cfg.Database = database
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Conn{conn: conn, sqlLog: c.sqlLog}, nil
}

// ConnectWithoutTimeout opens a new connection to the SAME target as c (host, port, user,
// auth, AND database), but with no statement_timeout — for VACUUM, which can legitimately
// run far longer than D6's default cap. Cloning c's resolved config is what lets the
// shell's `vacuum` reach the session's database even though the per-command flags have
// been reset. The caller owns the returned connection.
func (c *Conn) ConnectWithoutTimeout(ctx context.Context) (*Conn, error) {
	cfg := c.conn.Config().Copy()
	delete(cfg.RuntimeParams, "statement_timeout")
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("connect: %w", err)
	}
	return &Conn{conn: conn, sqlLog: c.sqlLog}, nil
}

// Cancel asks the server to cancel whatever query is currently running on this
// connection, using Postgres's out-of-band cancellation (a separate short-lived
// connection) — the same mechanism psql's Ctrl-C uses. It's safe to call from another
// goroutine while the connection is busy, and is a harmless no-op when nothing is running.
func (c *Conn) Cancel(ctx context.Context) error {
	return c.conn.PgConn().CancelRequest(ctx)
}

// Close releases the connection.
func (c *Conn) Close(ctx context.Context) error { return c.conn.Close(ctx) }

// Host, Port, and Database report the resolved connection target (after env/service/
// dsn/--database are applied) — used for the `pgdx status` header, like a psql prompt.
func (c *Conn) Host() string     { return c.conn.Config().Host }
func (c *Conn) Port() uint16     { return c.conn.Config().Port }
func (c *Conn) Database() string { return c.conn.Config().Database }

// Query runs a read query. Used by the catalog browse commands (get/describe).
// The statement_timeout set at connect time (D6) applies here too.
func (c *Conn) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	c.logSQL(sql, args...)
	return c.conn.Query(ctx, sql, args...)
}

// Exec runs a statement with no result rows (e.g. VACUUM). VACUUM cannot run inside a
// transaction block; pgx's Exec autocommits each call, so this is safe for it.
func (c *Conn) Exec(ctx context.Context, sql string) error {
	c.logSQL(sql)
	_, err := c.conn.Exec(ctx, sql)
	return err
}

// QueryResult is the generic shape returned by RunReadOnlyQuery: column names and the
// rows as native Go values (so -o json marshals them with their real types).
type QueryResult struct {
	Columns []string `json:"columns"`
	Rows    [][]any  `json:"rows"`
}

// RunReadOnlyQuery runs an arbitrary SELECT inside a READ ONLY transaction and returns
// its columns and rows. The transaction is opened READ ONLY so any statement that tries
// to write (or a data-modifying CTE) is rejected by the server — the read-only posture
// holds even though the SQL is user-supplied — and it's always rolled back. The
// connect-time statement_timeout (D6) still applies, so a runaway query can't hang.
func (c *Conn) RunReadOnlyQuery(ctx context.Context, sql string) (*QueryResult, error) {
	if _, err := c.conn.Exec(ctx, "BEGIN READ ONLY"); err != nil {
		return nil, fmt.Errorf("begin read-only: %w", err)
	}
	// Always end the transaction; nothing here is ever committed.
	defer func() { _, _ = c.conn.Exec(ctx, "ROLLBACK") }()

	c.logSQL(sql)
	rows, err := c.conn.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	res := &QueryResult{}
	for _, fd := range rows.FieldDescriptions() {
		res.Columns = append(res.Columns, string(fd.Name))
	}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		res.Rows = append(res.Rows, vals)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return res, nil
}

// RunExplain executes the statements produced by the explain guard and returns the
// raw EXPLAIN (FORMAT JSON) output.
//
// For the rollback-wrapped case the statements are [BEGIN, EXPLAIN ANALYZE..., ROLLBACK].
// We guarantee the transaction is closed even if the EXPLAIN fails: if anything goes
// wrong mid-sequence after a BEGIN, we issue a best-effort ROLLBACK.
//
//	stmts ── BEGIN ──► EXPLAIN ANALYZE (captures JSON) ──► ROLLBACK
//	            │                  │ error                      ▲
//	            └──────────────────┴──── deferred ROLLBACK ─────┘
func (c *Conn) RunExplain(ctx context.Context, stmts []string) (json []byte, err error) {
	inTx := false
	defer func() {
		if inTx {
			// Best-effort: ensure no transaction is left open on error paths.
			_, _ = c.conn.Exec(ctx, "ROLLBACK")
		}
	}()

	for _, stmt := range stmts {
		c.logSQL(stmt)
		switch stmt {
		case "BEGIN":
			if _, err = c.conn.Exec(ctx, stmt); err != nil {
				return nil, fmt.Errorf("begin: %w", err)
			}
			inTx = true
		case "ROLLBACK":
			if _, err = c.conn.Exec(ctx, stmt); err != nil {
				return nil, fmt.Errorf("rollback: %w", err)
			}
			inTx = false
		default:
			// The EXPLAIN statement: one row, one JSON column. Run it through the raw
			// simple-query path (PgConn().Exec) rather than QueryRow. An EXPLAIN
			// (GENERIC_PLAN) of a parameterized query carries a literal "$1" in its text;
			// pgx's normal paths treat "$N" as a client bind placeholder and demand a
			// value ("insufficient arguments"). PgConn().Exec sends the SQL verbatim, so
			// the placeholder reaches the server untouched and GENERIC_PLAN handles it.
			j, rerr := readExplainJSON(ctx, c.conn, stmt)
			if rerr != nil {
				return nil, fmt.Errorf("run EXPLAIN: %w", rerr)
			}
			json = j
		}
	}
	if json == nil {
		return nil, fmt.Errorf("no EXPLAIN output produced")
	}
	return json, nil
}

// RunExplainWithParams is RunExplain for a parameterized query: it binds the supplied
// values to the statement's $1…$N placeholders so the planner sees real values (real
// selectivity, and real timings under --analyze) — the alternative to EXPLAIN
// (GENERIC_PLAN), which can only estimate.
//
// The values are sent with UNKNOWN type OIDs in text format, so Postgres infers each
// placeholder's type from its context in the query (LIMIT $n → integer, col ILIKE $n →
// text) and parses the text exactly as it would a literal. The values are bound through
// the protocol, never spliced into the SQL, so this is injection-safe. The trailing
// ROLLBACK still runs on any error path, as in RunExplain.
func (c *Conn) RunExplainWithParams(ctx context.Context, stmts []string, params []string) (json []byte, err error) {
	paramValues := make([][]byte, len(params))
	for i, p := range params {
		paramValues[i] = []byte(p)
	}
	logArgs := make([]any, len(params))
	for i, p := range params {
		logArgs[i] = p
	}

	inTx := false
	defer func() {
		if inTx {
			_, _ = c.conn.Exec(ctx, "ROLLBACK")
		}
	}()

	for _, stmt := range stmts {
		switch stmt {
		case "BEGIN":
			c.logSQL(stmt)
			if _, err = c.conn.Exec(ctx, stmt); err != nil {
				return nil, fmt.Errorf("begin: %w", err)
			}
			inTx = true
		case "ROLLBACK":
			c.logSQL(stmt)
			if _, err = c.conn.Exec(ctx, stmt); err != nil {
				return nil, fmt.Errorf("rollback: %w", err)
			}
			inTx = false
		default:
			// The EXPLAIN statement, with the $N placeholders bound. nil OIDs => unknown
			// (server infers); nil formats => text in/out.
			c.logSQL(stmt, logArgs...)
			res := c.conn.PgConn().ExecParams(ctx, stmt, paramValues, nil, nil, nil).Read()
			if res.Err != nil {
				return nil, fmt.Errorf("run EXPLAIN: %w", res.Err)
			}
			if len(res.Rows) > 0 && len(res.Rows[0]) > 0 {
				json = append([]byte(nil), res.Rows[0][0]...)
			}
		}
	}
	if json == nil {
		return nil, fmt.Errorf("no EXPLAIN output produced")
	}
	return json, nil
}

// readExplainJSON runs a single EXPLAIN (FORMAT JSON) statement via the raw simple-query
// protocol and returns the JSON value from its one row / one column. Using PgConn().Exec
// (not QueryRow) means any "$1" in the statement text is sent literally — required for
// EXPLAIN (GENERIC_PLAN) of a parameterized query, which has no value to bind.
func readExplainJSON(ctx context.Context, conn *pgx.Conn, stmt string) ([]byte, error) {
	results, err := conn.PgConn().Exec(ctx, stmt).ReadAll()
	if err != nil {
		return nil, err
	}
	for _, r := range results {
		if r.Err != nil {
			return nil, r.Err
		}
		if len(r.Rows) > 0 && len(r.Rows[0]) > 0 {
			// Copy: the bytes are owned by the result reader.
			return append([]byte(nil), r.Rows[0][0]...), nil
		}
	}
	return nil, fmt.Errorf("EXPLAIN returned no rows")
}
