# Postgres Diagnostics

**pgdx** — a kubectl-style command-line tool for Postgres: browse your schema, see what the
server is doing right now, and diagnose slow queries — with a consistent, scriptable
grammar instead of arcane `psql` meta-commands.

```console
$ pgdx explain "select count(*) from awards"
⚠ Full sequential scan on "awards" dominates the query
  The plan aggregates over the entire table with no filter, and that scan is the bulk
  of the cost. An unfiltered aggregate like COUNT(*) over a whole table can't be sped
  up by an index — Postgres must read every row.
  → If an exact, live count isn't required, pg_class.reltuples gives an instant
    estimate. For a fast exact count, maintain a summary/counter table.
```

pgdx is **read-only by default.** It diagnoses; it doesn't change your data. The only
write commands (`vacuum`, `config set-password`, and `get slow-queries --reset` — which
clears statistics, not data) are explicit and guarded.

## Install

Requires Go 1.25+.

```bash
go build -o pgdx .
# or
go install github.com/capitaltg/pgdx@latest
```

### Run with Docker

Prebuilt images are published to GitHub Container Registry:

```bash
docker pull ghcr.io/capitaltg/pgdx:latest
```

The `pgdx` binary is the image's entrypoint, so arguments go straight after the
image name. Because the container has its own isolated filesystem and network,
the simplest way to point it at a database is an explicit `--dsn` (or `PG*`
environment variables) — the host's `~/.pgpass` and `~/.pg_service.conf` are not
visible inside the container unless you mount them (see below).

```bash
# Connect via a DSN
docker run --rm ghcr.io/capitaltg/pgdx \
  status --dsn "postgres://analyst@db.example.com:5432/shop?sslmode=require"

# ...or via PG* environment variables
docker run --rm \
  -e PGHOST=db.example.com -e PGPORT=5432 -e PGUSER=analyst \
  -e PGDATABASE=shop -e PGPASSWORD \
  ghcr.io/capitaltg/pgdx status
```

**Reaching a Postgres on your host machine:** `localhost` inside the container
refers to the container itself, not your host. Use `host.docker.internal`
(Docker Desktop) or `--network host` (Linux):

```bash
docker run --rm --network host \
  ghcr.io/capitaltg/pgdx status --dsn "postgres://analyst@localhost:5432/shop"
```

**Reusing host config files** (`.pgpass`, named contexts) — mount them read-only
into the non-root user's home (`/home/nonroot`):

```bash
docker run --rm \
  -v "$HOME/.pgpass:/home/nonroot/.pgpass:ro" \
  -v "$HOME/.pg_service.conf:/home/nonroot/.pg_service.conf:ro" \
  -e PGSERVICE=prod \
  ghcr.io/capitaltg/pgdx status
```

## Connecting

pgdx connects the same way `psql` does — it honors all of Postgres's standard
mechanisms, so there's nothing new to configure if you already use them:

- `PG*` environment variables (`PGHOST`, `PGPORT`, `PGUSER`, `PGDATABASE`, `PGPASSWORD`)
- a connection URI: `pgdx ... --dsn "postgres://user@host:5432/dbname?sslmode=require"`
- `~/.pgpass` for passwords
- `~/.pg_service.conf` named services (`PGSERVICE=prod`)

### Named contexts

For switching between databases, pgdx wraps `pg_service.conf` with a kubectl-style
context workflow (everything it writes is a normal Postgres file `psql` can also use):

```bash
pgdx config set-context prod --host prod.db.example.com --port 5432 --user analyst --dbname shop
pgdx config set-password prod          # prompts; writes ~/.pgpass (0600), never via argv
pgdx config use-context prod           # optional: make it the default

pgdx config get-contexts               # list (current marked with *)
pgdx config current-context            # show the active context
pgdx config get-context prod           # show one (password never shown)
pgdx config delete-context prod
```

**Precedence** when choosing where to connect: an explicit `--dsn` or `$PGHOST`/
`$PGSERVICE` always wins over the stored default context.

## Interactive shell

`pgdx shell` opens a session on a **single read-only connection** and keeps it open, so a
triage session runs many commands without reconnecting each time. At the prompt you type
the same commands you'd pass on the CLI — `get`, `describe`, `status`, `explain`, `query`,
`diff`, `snapshot`, `vacuum` — with the same flags.

```console
$ pgdx shell                              # or: pgdx shell -d analytics / --dsn ... / PGSERVICE=prod pgdx shell
pgdx shell — read-only by default, on db.example.com:5432/shop
pgdx/shop=> get tables
pgdx/shop=> describe table public.orders
pgdx/shop=> use analytics                 # switch the whole session to another database
pgdx/analytics=> use schema reporting      # set the default schema (search_path) for unqualified names
pgdx/analytics:reporting=> query "select count(*) from events"
pgdx/analytics:reporting=> \q              # or: exit / quit / Ctrl-D
```

- **Editing & history:** Tab-completes commands, flags, and — for `describe table|view|index`
  and `vacuum` — live object names; ↑/↓ recall prior commands (history is saved to
  `~/.config/pgdx/shell_history`), Ctrl-R searches it, Ctrl/Alt-←/→ move by word, Home/End
  jump to the line ends, Ctrl-W deletes the previous word.
- **`use <database>`** switches the session to another database (same server); **`use schema <name>`**
  sets `search_path` so unqualified names in `query` resolve there. The prompt reflects both
  (`pgdx/db:schema=>`).
- **Ctrl-C** cancels the running command — a slow query, explain, or a long `analyze`/`vacuum`
  — and returns to the prompt; it does *not* kill the session.
- Read-only by default, exactly like one-shot `pgdx`; every query still runs under the
  `statement_timeout`. The single connection means context notes are shown once, in the banner.

## Commands

### Triage

```bash
pgdx summarize                        # one-screen inventory: object counts, sizes, index health, biggest tables
pgdx summarize -d analytics           # …of another database (default: the connected one)
pgdx summarize --top 10               # list more (or fewer) of the largest tables (default 5)
pgdx status                           # one-screen health check: is anything wrong right now?
pgdx status -v                        # verbose: expand each line into the rows behind it
pgdx status --watch                   # live: re-render every 2s (--watch=5s to change); Ctrl-C to stop
pgdx status -v --watch                # a live, detailed dashboard
```

`summarize` is the at-a-glance inventory of one database — how many tables, views,
indexes, sequences, functions, and extensions it has; a size breakdown; an index-health
rollup (unused + redundant counts); estimated reclaimable bloat; and the largest tables.
It's the "what's in here and how big is it" companion to `status` ("is anything wrong")
and `get databases` (the cluster-wide list). Defaults to the connected database; `-d`
points it at another. `-o json` emits every figure.

`status` is the front door for an incident or a morning check. In one read-only snapshot
it checks connection saturation, blocked locks, the oldest open transaction, replication
lag, inactive replication slots (pinned WAL), XID-wraparound risk, forced-vs-scheduled
checkpoints, and top bloat — marks each ⚠ / ✓ / ○ — and points at the command to drill
into. It correlates signals where it can (a lock root blocker that's *also* the
oldest transaction is the textbook idle-in-transaction incident) and ends with a single
"start here" pointer. A calm, mostly-✓ screen is itself the answer.

`-v`/`--verbose` hangs the offending rows under each line — the blocked PIDs and what
they're waiting on, the oldest open transactions, the lagging standbys, the connection
mix (active vs idle vs idle-in-transaction), the top bloated tables — so you can act
without running a second command. Each section caps at a handful of rows with a "+N more"
pointer to the drill-down command. `--watch` re-renders the same snapshot on an interval
(like `watch pgdx status`), turning it into a live dashboard for an incident; it streams
newline-delimited JSON under `-o json` so it stays pipeable.

```console
$ pgdx status
Database "shop" @ db.example.com:5432 — 2026-05-31 14:22:03

⚠  Connections    188 / 200 (94% of max_connections) — approaching the limit; new connections may be refused
⚠  Locks          3 session(s) blocked waiting on locks (root blocker: pid 8821)
⚠  Transactions   oldest open 12m (pid 8821, idle in transaction); 1 idle-in-transaction — blocking VACUUM cluster-wide
✓  Replication    1 standby(s), max replay lag 0s
✓  Wraparound     max XID age 6% of freeze threshold (public.orders)
○  Bloat          top: public.orders ~1.2 GB reclaimable

→ Start with the lock root blocker pid 8821. pid 8821 is also the oldest transaction (12m, idle in transaction) — inspect, then `cancel`/`kill` it.
```

### Diagnose a query

```bash
pgdx explain "<query>"            # plan diagnosis — does NOT run your query
pgdx explain "<query>" -v         # + the plan tree (evidence), flagged node marked
pgdx explain "<query>" -vv        # + a plain-language explanation of the plan
pgdx explain "<query>" -vvv       # + live catalog stats & a cost breakdown (needs a connection)
pgdx explain "<query>" --analyze  # runs it for real timings (writes are wrapped in
                                  #   BEGIN..ROLLBACK; unsafe side effects are refused)
pgdx explain "<query>" --analyze --suggest-index  # also print a candidate CREATE INDEX
pgdx explain --pid 20464          # diagnose the query a running backend is executing
pgdx explain "<...$1...$2...>" -p val1 -p val2   # bind values for a parameterized query (real plan)
pgdx explain --plan plan.json     # diagnose an existing EXPLAIN (FORMAT JSON) plan, offline (no DB)
psql -c 'EXPLAIN (FORMAT JSON) ...' | pgdx explain --plan -   # …or from stdin
```

`explain` detects common problems (full-table scans, missing indexes via row-estimate
blowup, sorts spilling to disk) and, when nothing's wrong, tells you *why* it's fine
(the index it uses, cost, row estimate).

`-v`/`--verbose` is tiered — repeat the flag for more depth:

- **`-v`** prints the plan tree beneath the diagnosis (the evidence), with the node pgdx
  flagged marked `← flagged`.
- **`-vv`** adds a plain-language explanation of *this* plan — what the cost number means
  (effort, not rows), why a parallel node's `rows` are per-worker, how two-phase parallel
  aggregation satisfies `COUNT/SUM/AVG`.
- **`-vvv`** adds the live catalog facts behind the flagged node (table size, `reltuples`,
  the cost-model settings) and an approximate breakdown of how its cost is computed.

`-v` and `-vv` are rendered from the plan pgdx already parsed, so they never re-run your
query (safe under `--analyze`) and work offline with `--plan`. `-vvv` needs a live
connection for the catalog stats, so it isn't available with `--plan`. This is most useful
exactly when the verdict is "no obvious problem" but the query is still slow.

`--pid <pid>` closes the loop from `get activity`: instead of copy-pasting, point `explain`
at a backend PID and it fetches that session's current query and diagnoses it. It reconnects
to the backend's own database automatically, and for prepared/parameterized statements
(`$1`, `$2`, …) — which can't be planned without values — it uses `EXPLAIN (GENERIC_PLAN)`
(estimates only; requires PostgreSQL 16+).

`--suggest-index` turns a "sequential scan filters out most rows" finding into a
candidate `CREATE INDEX CONCURRENTLY` (equality columns first, then ranges; casts like
`(col)::text` are unwrapped). It's a *starting point*, not a guarantee — pgdx declines to
guess when the filter uses `OR` or a function on a column (`lower(email)`), since a plain
B-tree index wouldn't serve those. Verify against your workload before creating it.

`--param`/`-p` lets you explain a **parameterized** query verbatim — the `$1, $2, …` form
you get straight from `get slow-queries` or `pg_stat_statements`. Supply one value per
placeholder, in order (`-p 'mercy%' -p 20`); pgdx binds them so the planner sees *real*
values (real selectivity, and real timings with `--analyze`) — the accurate alternative
to `GENERIC_PLAN`. Values are bound through the protocol, never spliced into the SQL
(injection-safe), and Postgres infers each placeholder's type from context, so `LIMIT $7`
takes a number and `col ILIKE $1` takes text with no casting.

`--plan` diagnoses a plan you *already have* — no database connection. Point it at a file
of `EXPLAIN (FORMAT JSON)` output, or pass `-` to read stdin, and pgdx applies the same
diagnosis. Handy for a plan pulled from a slow-query log, `auto_explain`, or a colleague.

### Browse

```bash
pgdx get tables [--schema S]          # tables: size, est rows, DEAD% (bloat)
pgdx get tables --usage               # read/write patterns: seq vs index scans (IDX%), ins/upd/del
pgdx get tables -o mermaid            # schema ER diagram: tables + FK relationships (--all-columns for every column)
pgdx get indexes [--table T] [--unused]  # indexes + scan counts; --unused = drop candidates
pgdx get indexes --sort size          # order by index size (biggest first); also --sort scans | name
pgdx get indexes --redundant          # indexes covered by another (duplicate or prefix) — safe drops
pgdx get bloat [--limit N]            # leaderboard: tables ranked by est. reclaimable space
pgdx get views | sequences | functions | schemas | extensions
pgdx get sequences                    # incl. MAX-VALUE and USED% — catch int4 sequences nearing overflow
pgdx get extensions --available       # everything installable here (version, installed?, trusted?)
pgdx get databases [--sort name|size] # databases: size, conns, commits/writes (activity)
pgdx get databases --sample 5s        # COMMITS/s + WRITES/s — is a database actually active?
pgdx get databases --wide             # add health columns: HIT%, ROLLBACK%, DEADLOCKS, TEMP, STATS-RESET
pgdx get roles                        # roles/users: attributes, memberships, live sessions (alias: users)
pgdx get settings [name...] [--all]   # server config (curated subset by default)

pgdx describe table <name>            # columns, indexes, constraints, incoming FKs, partitions, bloat
pgdx describe table <name> --stats    # + per-column planner stats (n_distinct, null frac, correlation)
pgdx describe table <name> -o mermaid # ER diagram: columns + keys + FK relationships
pgdx describe index <name>            # method, validity, usage, definition
pgdx describe view <name>             # columns + definition

pgdx query "<read-only sql>"          # run arbitrary SELECT in a READ ONLY tx, rendered like everything else
```

`get indexes --redundant` flags non-unique indexes whose leading columns are already
covered by another index on the same table — an exact duplicate, or a prefix of a wider
index. Unlike `--unused`, this needs no usage history (it's structural), so it's a
high-confidence drop list; it shows the covering index (`COVERED-BY`) and excludes
partial/expression indexes, whose equivalence can't be judged from columns alone.

`describe table` always lists **incoming foreign keys** (what references this table — the
"is it safe to drop?" answer). `--stats` adds per-column `pg_stats`: a wrong `n_distinct`
or stale stats are a leading cause of the row-estimate blowups `explain` flags, so this is
where you confirm them.

`query` is the read-only escape hatch: arbitrary SQL with pgdx's table/JSON rendering and
guard rails, without dropping to `psql`. It runs inside a `READ ONLY` transaction (any
write — or a data-modifying CTE — is refused by the server) under the standard
`statement_timeout`.

### Live state / incidents

```bash
pgdx get activity [--sort blocked|duration]  # running queries, waits, blocking, client IP (blocked-first)
pgdx get activity --all --datname mydb  # include idle sessions, scoped to one database (cluster-wide view)
pgdx get activity --min-duration 30s    # only queries running longer than 30s (the long-running-query filter)
pgdx get activity --full                # don't truncate the QUERY column (see the whole statement)
pgdx get activity --watch --min-duration 30s --sort duration  # live runaway-query monitor (Ctrl-C to stop)
pgdx get locks                        # who's waiting on what lock, and who holds it
pgdx get connections [--detail]       # usage vs max_connections, idle-in-transaction watch
                                      #   filter: --user / --state / --app (e.g. --app DBeaver)
pgdx get slow-queries [--sort total|mean|max|stddev|calls|rows|io|temp]  # top queries, connected DB (needs pg_stat_statements)
pgdx get slow-queries --all-databases # every database's statements (pg_stat_statements is cluster-wide)
pgdx get slow-queries --full          # don't truncate the QUERY column (see the whole statement)
pgdx get slow-queries --limit 50      # how many queries to show (default 20)
pgdx get slow-queries --reset         # clear pg_stat_statements counters (stats only; asks to confirm)
pgdx get progress                     # in-flight vacuum / create index / analyze / cluster
pgdx get replication                  # standby lag
pgdx get replication --slots          # replication slots: active? + WAL retained (inactive = disk-filler)
pgdx get transaction-age [--min 30s]  # oldest open transactions (they hold back VACUUM)
pgdx get vacuum-health [--limit N]    # XID age vs wraparound threshold per relation
pgdx get vacuum-health --schema app   # narrow to one schema (keeps its tables' TOAST relations)

pgdx cancel <pid>                     # cancel a backend's running query (gentle, Ctrl-C-like)
pgdx kill <pid>                       # terminate a backend (drops connection; confirms)
```

PIDs for `cancel`/`kill` come from `get activity`, `get locks` (incl. `BLOCKED-BY`), or
`get connections --detail`. Both show the target's identity before acting; `kill` also
confirms (`--force` to skip). Cancelling/terminating another user's backend needs
superuser or `pg_signal_backend`.

`get slow-queries` reads `pg_stat_statements`, which is **cluster-wide** — every database's
statements live in one view. pgdx scopes the list to the **connected database** by default
(so `-d sbn` shows only `sbn`'s queries); pass `--all-databases` for the raw cluster-wide
view across every database in the instance.

It also re-sorts by the axis that matters: `--sort mean`
catches the slow-but-rare query that `total` (dominated by call frequency) hides; `--sort max`
the worst single execution (the latency spike, not the average); `--sort stddev` the most
*inconsistent* query (usually fast, occasionally catastrophic); `io`
ranks by physical block reads (what's thrashing `shared_buffers`); `temp` by on-disk sort/
hash traffic (raise `work_mem`). The table also shows `STDDEV` (a high one means *sometimes*
catastrophic, not uniformly slow) and `HIT%` (cache hit ratio). `--reset` clears the
counters — statistics only, never data — to start a clean measurement window.

`get replication --slots` is the companion to standby lag: an **inactive** replication
slot silently pins WAL for a consumer that went away and can fill the disk — a failure
mode that shows up nowhere in `pg_stat_replication`. It surfaces in `status` too.

### Trends — snapshot & diff

Postgres exposes its statistics only as cumulative totals since the last reset, which are
nearly useless on their own. The signal is the *delta* between two readings — and core
Postgres has no way to take it. `snapshot` captures `pg_stat_statements` + per-table
counters to a local file; `diff` subtracts two captures and shows the top movers.

```bash
pgdx snapshot --label before-deploy   # capture a baseline (writes a local JSON file, not the DB)
# ... time passes, or a deploy happens ...
pgdx snapshot --label after-deploy
pgdx diff                              # diff the two most recent snapshots
pgdx diff before-deploy                # diff a baseline against a fresh live reading (→ now)
pgdx diff before-deploy after-deploy   # diff two named snapshots
pgdx snapshot --list                   # list stored snapshots
```

`diff` answers "what got slow since this morning" (queries by **added** execution time)
and "which table absorbed the writes" (tables by insert/update/delete delta) — questions
raw cumulative totals can't. A counter that went backwards (someone reset stats between
snapshots) is treated as fresh accumulation, not a misleading negative. Snapshots live
under `$PGDX_STATE_DIR` (default `~/.local/state/pgdx/snapshots`). Read-only against the
database — `snapshot` only writes a local file.

### Maintenance (write)

```bash
pgdx vacuum <table>            # reclaim dead tuples (online, non-destructive)
pgdx vacuum <table> --analyze  # also refresh planner stats
pgdx vacuum <table> --full     # rewrite table (ACCESS EXCLUSIVE lock — asks to confirm)

pgdx analyze <table>           # refresh planner statistics for one table
pgdx analyze --schema app      # …for every table in a schema
pgdx analyze --all             # …for every table in the current database
```

Table/index/view names can be bare (`orders`) or schema-qualified (`reporting.orders`).
A bare name that exists in multiple schemas asks you to qualify it.

`analyze` is the fix when `get tables` shows blank ROWS/DEAD% (no statistics — e.g. just
after a restore). ANALYZE only samples rows and runs online, but whole-database analyze is
deliberate: it requires `--all`, never a bare `pgdx analyze`.

## Output

**Targeting another database:** Postgres can't query across databases from one
connection, so the global `-d`/`--database` flag makes pgdx *connect* to a different
database for that command (it overrides a context's dbname). Needs CONNECT on the target;
a superuser can reach any database.

```bash
pgdx get tables -d analytics          # browse a database other than your default
pgdx describe table -d analytics events
```

Every command supports `-o json` for scripting; the default is a human-readable table.
Data goes to stdout, warnings/errors to stderr, so pipelines stay clean:

```bash
pgdx get indexes -o json | jq '.[] | select(.scans == 0 and .unique == false)'
```

`describe table` and `get tables` additionally support `-o mermaid`, which emits a Mermaid
`erDiagram`. For `describe table` it's one table — its columns (with `PK`/`FK`/`UK` tags and
nullability) plus its outgoing and incoming foreign-key relationships. For `get tables` it's
the whole schema — every table and the foreign keys between them, showing only key columns by
default (`--all-columns` for every column; pair with `--schema` on a big database to keep it
readable). Paste the output into [mermaid.live](https://mermaid.live) or a `mermaid` fenced
block, or pipe it straight to an image:

```bash
pgdx describe table orders -o mermaid | mmdc -i - -o orders.svg
pgdx get tables --schema public -o mermaid | mmdc -i - -o schema.svg
```

**See the SQL:** the global `--sql` flag prints every query pgdx runs (to stderr — so it
never mixes with `-o json` on stdout). Handy for learning, copying a query, or auditing.

```bash
pgdx describe table orders --sql        # shows each underlying catalog query + bind args
pgdx get tables -o json --sql 2>queries.sql | jq .   # data to jq, SQL saved to a file
```

## A typical workflow

```bash
pgdx get tables                       # orders shows DEAD% 32% — bloated?
pgdx describe table orders            # confirms: 32% dead, last autovacuum: never
pgdx vacuum orders --analyze          # reclaim it
pgdx get indexes --unused             # orders_note_idx: 0 scans over 142 days — drop it
pgdx get activity                     # something's blocked → see the blocker PID
```

## Safety

- **Read-only by default.** Browsing and `explain` never modify data.
- `explain` does not execute your query unless you pass `--analyze`; even then, writes
  run inside a rolled-back transaction and statements with side effects a rollback
  can't undo (e.g. `nextval`, `NOTIFY`, `COPY ... PROGRAM`) are refused.
- `query` runs in a `READ ONLY` transaction, so any write (or data-modifying CTE) in the
  SQL you pass is rejected by the server, and the transaction is always rolled back.
- `vacuum --full` takes a blocking lock and asks for confirmation (`--force` to skip).
- `get slow-queries --reset` clears `pg_stat_statements` counters (statistics, not data)
  and asks for confirmation (`--force` to skip).
- `config set-password` prompts for the password (never reads it from the command line)
  and writes `~/.pgpass` with mode 0600.
- pgdx sets a conservative `statement_timeout` on its own session so it never becomes
  the query that hangs a struggling database.

## Development

```bash
make build    # compile ./pgdx (version stamped from git)
make test     # run the suite
make vet fmt  # vet + format
make all      # fmt, vet, test, build
```

(or plain `go build ./...` / `go test ./...`)

Layout: `cmd/` (Cobra commands), `internal/catalog` (read-only catalog/stat queries),
`internal/explain` (the diagnosis engine + plan parser), `internal/snapshot` (stat
snapshot store + diff), `internal/render` (table/json output), `internal/db` (pgx
connection), `internal/pgservice` & `internal/pgpass` & `internal/pgdxconfig` (connection
config).
