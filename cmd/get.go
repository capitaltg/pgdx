package cmd

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/db"
	"github.com/capitaltg/pgdx/internal/render"
)

// newGetCmd is the resource-browsing verb (kubectl-style). v0.1 ships `get tables`;
// indexes/activity/locks/etc. slot in here later (design doc, Build Sequencing).
func newGetCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "get <resource>",
		Short: "List database objects (read-only)",
	}
	c.AddCommand(newGetTablesCmd())
	c.AddCommand(newGetIndexesCmd())
	c.AddCommand(newGetActivityCmd())
	c.AddCommand(newGetSlowQueriesCmd())
	c.AddCommand(newGetLocksCmd())
	c.AddCommand(newGetViewsCmd())
	c.AddCommand(newGetSequencesCmd())
	c.AddCommand(newGetFunctionsCmd())
	c.AddCommand(newGetSchemasCmd())
	c.AddCommand(newGetExtensionsCmd())
	c.AddCommand(newGetSettingsCmd())
	c.AddCommand(newGetConnectionsCmd())
	c.AddCommand(newGetProgressCmd())
	c.AddCommand(newGetReplicationCmd())
	c.AddCommand(newGetDatabasesCmd())
	c.AddCommand(newGetRolesCmd())
	c.AddCommand(newGetBloatCmd())
	c.AddCommand(newGetTransactionAgeCmd())
	c.AddCommand(newGetVacuumHealthCmd())
	return c
}

func newGetRolesCmd() *cobra.Command {
	c := &cobra.Command{
		Use:     "roles",
		Aliases: []string{"users"},
		Short:   "List roles/users with their attributes and memberships",
		Long: "roles lists database roles (Postgres calls users 'roles') with their attributes\n" +
			"(superuser, create db/role, login, replication), connection limit, validity, which\n" +
			"roles they're members of, and SESSIONS (connections open RIGHT NOW). Built-in pg_*\n" +
			"roles are excluded. Read-only. `pgdx get users` is an alias.\n\n" +
			"Note: SESSIONS is current connections, NOT last login — core Postgres keeps no\n" +
			"last-login history (you'd need log analysis or an audit extension for that).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())
			roles, err := catalog.ListRoles(context.Background(), conn)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if roles == nil {
					roles = []catalog.Role{}
				}
				return render.Render(cmd.OutOrStdout(), format, roles)
			}
			if len(roles) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no roles found")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, rolesView(roles))
		},
	}
	return c
}

type rolesView []catalog.Role

func (v rolesView) Headers() []string { return []string{"NAME", "ATTRIBUTES", "SESSIONS", "MEMBER-OF"} }
func (v rolesView) Aligns() []render.Align {
	return []render.Align{render.AlignLeft, render.AlignLeft, render.AlignRight, render.AlignLeft}
}
func (v rolesView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, r := range v {
		var attrs []string
		if r.Superuser {
			attrs = append(attrs, "Superuser")
		}
		if r.CreateDB {
			attrs = append(attrs, "Create DB")
		}
		if r.CreateRole {
			attrs = append(attrs, "Create role")
		}
		if r.Replication {
			attrs = append(attrs, "Replication")
		}
		if !r.Login {
			attrs = append(attrs, "No login")
		}
		if r.ConnLimit >= 0 {
			attrs = append(attrs, fmt.Sprintf("Conn limit %d", r.ConnLimit))
		}
		if r.ValidUntil != "" && r.ValidUntil != "infinity" {
			until := r.ValidUntil
			if len(until) >= 10 {
				until = until[:10]
			}
			attrs = append(attrs, "Until "+until)
		}
		attrStr := "—"
		if len(attrs) > 0 {
			attrStr = strings.Join(attrs, ", ")
		}
		out = append(out, []string{r.Name, attrStr, withThousands(r.Sessions), dashIfEmpty(strings.Join(r.MemberOf, ", "))})
	}
	return out
}

func newGetDatabasesCmd() *cobra.Command {
	var sort string
	var sample time.Duration
	var wide bool
	c := &cobra.Command{
		Use:   "databases",
		Short: "List databases in the cluster (size where you have CONNECT)",
		Long: "databases lists non-template databases with owner, encoding, size, current\n" +
			"connection count, and activity (COMMITS/WRITES since stats_reset). Size requires\n" +
			"CONNECT privilege on each database — it shows '—' for databases you can't connect\n" +
			"to, rather than failing. Read-only.\n\n" +
			"Default sort is by name; use --sort size for biggest-first.\n\n" +
			"--wide adds per-database health columns: HIT% (shared-buffer cache hit ratio),\n" +
			"ROLLBACK% (rolled-back vs committed transactions), DEADLOCKS, TEMP (bytes spilled\n" +
			"to disk under work_mem pressure), and STATS-RESET (when the cumulative counters\n" +
			"were last reset — the window the totals and ratios cover). JSON output (-o json)\n" +
			"always includes these fields regardless of --wide.\n\n" +
			"--sample <interval> (e.g. 5s) takes two readings and reports COMMITS/s and\n" +
			"WRITES/s instead of cumulative totals — the real signal for whether a database is\n" +
			"actually being used right now (an idle DB shows ~0 writes/s and only a steady\n" +
			"monitoring commit trickle).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			ctx := context.Background()
			defer conn.Close(ctx)
			switch sort {
			case "name", "size":
			default:
				return usageError{fmt.Sprintf("invalid --sort %q (want: name, size)", sort)}
			}

			if sample > 0 {
				return sampleDatabaseRates(cmd, conn, sort, sample, format)
			}

			dbs, err := catalog.ListDatabases(ctx, conn, sort)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if dbs == nil {
					dbs = []catalog.Database{}
				}
				return render.Render(cmd.OutOrStdout(), format, dbs)
			}
			if len(dbs) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no databases found")
				return nil
			}
			if wide {
				// Explain the non-obvious health columns inline (to stderr, so stdout stays
				// clean for piping) — mirrors `get slow-queries`' column legend.
				fmt.Fprintln(cmd.ErrOrStderr(),
					"wide columns — HIT%: shared-buffer cache hit ratio (higher is better); ROLLBACK%: rolled-back vs committed transactions; TEMP: bytes spilled to disk under work_mem pressure; STATS-RESET: when these cumulative counters were last reset.")
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, databasesView{rows: dbs, wide: wide})
		},
	}
	c.Flags().StringVar(&sort, "sort", "name", "sort order: name | size (biggest first)")
	c.Flags().DurationVar(&sample, "sample", 0, "sample COMMITS/s and WRITES/s over this interval (e.g. 5s) instead of totals")
	c.Flags().BoolVar(&wide, "wide", false, "add health columns: HIT%, ROLLBACK%, DEADLOCKS, TEMP, STATS-RESET")
	return c
}

// sampleDatabaseRates takes two readings <interval> apart and reports per-second commit
// and write rates per database — the real "is anything actually happening" signal,
// since cumulative totals are dominated by background monitoring.
func sampleDatabaseRates(cmd *cobra.Command, conn *db.Conn, sort string, d time.Duration, format render.Format) error {
	ctx := context.Background()
	first, err := catalog.ListDatabases(ctx, conn, sort)
	if err != nil {
		return err
	}
	fmt.Fprintf(cmd.ErrOrStderr(), "sampling over %s...\n", d)
	time.Sleep(d)
	second, err := catalog.ListDatabases(ctx, conn, sort)
	if err != nil {
		return err
	}

	prev := make(map[string]catalog.Database, len(first))
	for _, x := range first {
		prev[x.Name] = x
	}
	secs := d.Seconds()
	rates := make(dbRateView, 0, len(second))
	for _, x := range second {
		p, ok := prev[x.Name]
		if !ok {
			continue
		}
		rates = append(rates, dbRate{
			Name:          x.Name,
			Conns:         x.Connections,
			CommitsPerSec: perSecFloat(x.Commits-p.Commits, secs),
			WritesPerSec:  perSecFloat(x.Writes-p.Writes, secs),
		})
	}

	if format == render.FormatJSON {
		return render.Render(cmd.OutOrStdout(), format,
			map[string]any{"sample_seconds": secs, "databases": rates})
	}
	return render.Render(cmd.OutOrStdout(), render.FormatTable, rates)
}

type dbRate struct {
	Name          string  `json:"name"`
	Conns         int64   `json:"connections"`
	CommitsPerSec float64 `json:"commits_per_sec"`
	WritesPerSec  float64 `json:"writes_per_sec"`
}

type dbRateView []dbRate

func (v dbRateView) Headers() []string { return []string{"NAME", "CONNS", "COMMITS/s", "WRITES/s"} }
func (v dbRateView) Aligns() []render.Align {
	return []render.Align{render.AlignLeft, render.AlignRight, render.AlignRight, render.AlignRight}
}
func (v dbRateView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, r := range v {
		out = append(out, []string{r.Name, withThousands(r.Conns), fmtRate(r.CommitsPerSec), fmtRate(r.WritesPerSec)})
	}
	return out
}

// perSecFloat computes a non-negative per-second rate (clamps to 0 if stats reset mid-sample).
func perSecFloat(delta int64, secs float64) float64 {
	if secs <= 0 || delta < 0 {
		return 0
	}
	return float64(delta) / secs
}

func fmtRate(r float64) string {
	switch {
	case r == 0:
		return "0"
	case r < 10:
		return fmt.Sprintf("%.2f", r)
	default:
		return fmt.Sprintf("%.0f", r)
	}
}

// databasesView renders the database list. wide=true appends per-database health
// columns (HIT% / ROLLBACK% / DEADLOCKS / TEMP / STATS-RESET) that are noisy in the
// default view but invaluable when triaging a specific cluster.
type databasesView struct {
	rows []catalog.Database
	wide bool
}

func (v databasesView) Headers() []string {
	h := []string{"NAME", "OWNER", "ENCODING", "SIZE", "CONNS", "COMMITS", "WRITES"}
	if v.wide {
		h = append(h, "HIT%", "ROLLBACK%", "DEADLOCKS", "TEMP", "STATS-RESET")
	}
	return h
}
func (v databasesView) Aligns() []render.Align {
	a := []render.Align{
		render.AlignLeft, render.AlignLeft, render.AlignLeft,
		render.AlignRight, render.AlignRight, render.AlignRight, render.AlignRight,
	}
	if v.wide {
		a = append(a, render.AlignRight, render.AlignRight, render.AlignRight, render.AlignRight, render.AlignLeft)
	}
	return a
}
func (v databasesView) Rows() [][]string {
	out := make([][]string, 0, len(v.rows))
	for _, d := range v.rows {
		size := "—" // no CONNECT privilege
		if d.SizeBytes >= 0 {
			size = humanBytes(d.SizeBytes)
		}
		row := []string{
			d.Name, d.Owner, d.Encoding, size,
			withThousands(d.Connections), withThousands(d.Commits), withThousands(d.Writes),
		}
		if v.wide {
			row = append(row,
				hitPct(cacheHitPct(d.BlksHit, d.BlksRead)),
				rollbackPct(d.Commits, d.Rollbacks),
				withThousands(d.Deadlocks),
				humanBytes(d.TempBytes),
				dashIfEmpty(shortTimestamp(d.StatsReset)),
			)
		}
		out = append(out, row)
	}
	return out
}

// cacheHitPct is the shared-buffer hit ratio as a percentage, or -1 when the database
// has touched no blocks yet (so hitPct renders "—" rather than a misleading 0%).
func cacheHitPct(hit, read int64) float64 {
	total := hit + read
	if total <= 0 {
		return -1
	}
	return 100 * float64(hit) / float64(total)
}

// rollbackPct is rolled-back transactions as a share of all transactions; "—" when none
// have completed yet.
func rollbackPct(commits, rollbacks int64) string {
	total := commits + rollbacks
	if total <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.1f%%", 100*float64(rollbacks)/float64(total))
}

// shortTimestamp keeps a Postgres timestamp string readable in a table cell by dropping
// the sub-second/timezone tail, leaving "2006-01-02 15:04:05".
func shortTimestamp(ts string) string {
	if len(ts) >= 19 {
		return ts[:19]
	}
	return ts
}

func newGetProgressCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "progress",
		Short: "Show in-progress vacuum / create index / analyze / cluster operations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())
			ops, err := catalog.ListProgress(context.Background(), conn)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if ops == nil {
					ops = []catalog.Progress{}
				}
				return render.Render(cmd.OutOrStdout(), format, ops)
			}
			if len(ops) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no maintenance operations in progress")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, progressView(ops))
		},
	}
	return c
}

type progressView []catalog.Progress

func (v progressView) Headers() []string {
	return []string{"PID", "OPERATION", "TABLE", "PHASE", "PROGRESS"}
}
func (v progressView) Aligns() []render.Align {
	return []render.Align{render.AlignRight, render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignRight}
}
func (v progressView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, p := range v {
		out = append(out, []string{
			strconv.FormatInt(int64(p.PID), 10),
			p.Operation, dashIfEmpty(p.Table), dashIfEmpty(p.Phase),
			fmt.Sprintf("%.1f%%", p.Percent),
		})
	}
	return out
}

func newGetReplicationCmd() *cobra.Command {
	var slots bool
	c := &cobra.Command{
		Use:   "replication",
		Short: "Show connected standbys and their lag (or --slots for replication slots)",
		Long: "replication lists downstream replicas from pg_stat_replication, with sync state\n" +
			"and how far each is behind (bytes sent-but-not-replayed, and replay lag time).\n" +
			"Empty unless this server is a primary with connected standbys.\n\n" +
			"--slots instead lists replication slots (pg_replication_slots): their type\n" +
			"(physical/logical), whether a consumer is currently ACTIVE, and how much WAL each\n" +
			"RETAINS. An INACTIVE slot is the silent disk-filler — it pins WAL for a replica or\n" +
			"logical consumer that has gone away, and unlike standby lag it shows up nowhere in\n" +
			"pg_stat_replication. Watch for wal_status 'unreserved'/'lost' (retention past the\n" +
			"limit). Read-only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())

			if slots {
				return runReplicationSlots(cmd, conn, format)
			}

			reps, err := catalog.ListReplication(context.Background(), conn)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if reps == nil {
					reps = []catalog.Replication{}
				}
				return render.Render(cmd.OutOrStdout(), format, reps)
			}
			if len(reps) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no standbys connected (not a primary, or no downstream replicas)")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, replicationView(reps))
		},
	}
	c.Flags().BoolVar(&slots, "slots", false, "list replication slots (active state + retained WAL) instead of standbys")
	return c
}

func runReplicationSlots(cmd *cobra.Command, conn *db.Conn, format render.Format) error {
	slots, err := catalog.ListReplicationSlots(context.Background(), conn)
	if err != nil {
		return err
	}
	if format == render.FormatJSON {
		if slots == nil {
			slots = []catalog.ReplicationSlot{}
		}
		return render.Render(cmd.OutOrStdout(), format, slots)
	}
	if len(slots) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "no replication slots defined")
		return nil
	}
	// Call out inactive slots retaining WAL — the disk-fill risk this view exists for.
	for _, s := range slots {
		if !s.Active && s.RetainedBytes > 0 {
			fmt.Fprintf(cmd.ErrOrStderr(),
				"⚠ slot %q is INACTIVE but retains %s of WAL — it will pin WAL (and fill the disk) until reconnected or dropped.\n",
				s.Name, humanBytes(s.RetainedBytes))
		}
	}
	return render.Render(cmd.OutOrStdout(), render.FormatTable, slotsView(slots))
}

type slotsView []catalog.ReplicationSlot

func (v slotsView) Headers() []string {
	return []string{"NAME", "TYPE", "DATABASE", "ACTIVE", "RETAINED-WAL", "WAL-STATUS"}
}
func (v slotsView) Aligns() []render.Align {
	return []render.Align{
		render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignRight, render.AlignLeft,
	}
}
func (v slotsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, s := range v {
		out = append(out, []string{
			s.Name, s.Type, dashIfEmpty(s.Database), yesNo(s.Active),
			humanBytes(s.RetainedBytes), dashIfEmpty(s.WalStatus),
		})
	}
	return out
}

type replicationView []catalog.Replication

func (v replicationView) Headers() []string {
	return []string{"PID", "APPLICATION", "CLIENT", "STATE", "SYNC", "LAG", "REPLAY-LAG"}
}
func (v replicationView) Aligns() []render.Align {
	return []render.Align{
		render.AlignRight, render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft,
		render.AlignRight, render.AlignRight,
	}
}
func (v replicationView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, r := range v {
		replay := secondsHuman(r.ReplayLagSec)
		out = append(out, []string{
			strconv.FormatInt(int64(r.PID), 10),
			dashIfEmpty(r.Application), dashIfEmpty(r.ClientAddr), dashIfEmpty(r.State),
			dashIfEmpty(r.SyncState), humanBytes(r.LagBytes), replay,
		})
	}
	return out
}

// humanBytes renders a byte count as B/kB/MB/GB.
func humanBytes(n int64) string {
	f := float64(n)
	neg := ""
	if n < 0 {
		f, neg = -f, "-"
	}
	switch {
	case f >= 1<<30:
		return fmt.Sprintf("%s%.1f GB", neg, f/(1<<30))
	case f >= 1<<20:
		return fmt.Sprintf("%s%.1f MB", neg, f/(1<<20))
	case f >= 1<<10:
		return fmt.Sprintf("%s%.1f kB", neg, f/(1<<10))
	default:
		return fmt.Sprintf("%s%d B", neg, int64(f))
	}
}

func newGetSettingsCmd() *cobra.Command {
	var all bool
	c := &cobra.Command{
		Use:   "settings [name...]",
		Short: "Show server config (curated set by default)",
		Long: "settings shows server configuration from pg_settings. With no arguments it shows a\n" +
			"curated set of the operationally important ones (work_mem, shared_buffers, ...).\n" +
			"Give name substrings to filter (e.g. `get settings vacuum`), or --all for everything.",
		Args: cobra.ArbitraryArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())
			settings, err := catalog.ListSettings(context.Background(), conn, args, all)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if settings == nil {
					settings = []catalog.Setting{}
				}
				return render.Render(cmd.OutOrStdout(), format, settings)
			}
			if len(settings) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no matching settings")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, settingsView(settings))
		},
	}
	c.Flags().BoolVar(&all, "all", false, "show all settings, not just the curated set")
	return c
}

type settingsView []catalog.Setting

func (v settingsView) Headers() []string { return []string{"NAME", "VALUE", "SOURCE", "DESCRIPTION"} }
func (v settingsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, s := range v {
		val := s.Value
		if s.Unit != "" {
			val += s.Unit
		}
		out = append(out, []string{s.Name, val, s.Source, compactQuery(s.Description, 50)})
	}
	return out
}

func newGetConnectionsCmd() *cobra.Command {
	var detail bool
	var fUser, fState, fApp string
	c := &cobra.Command{
		Use:   "connections",
		Short: "Connection usage vs max_connections, with breakdown and idle-in-tx watch",
		Long: "connections shows how many backends are connected vs max_connections, a breakdown\n" +
			"by database/user/application/state, and any idle-in-transaction backends (which\n" +
			"hold locks and block vacuum). Use --detail for one row per connection with client\n" +
			"address, connection age, and time in current state.\n\n" +
			"Filter with --user (exact), --state (exact, e.g. 'idle in transaction'), and --app\n" +
			"(substring, e.g. 'DBeaver'). Filters narrow the breakdown/detail; the used/max\n" +
			"header and idle-in-tx watch always reflect the whole cluster.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			ctx := context.Background()
			defer conn.Close(ctx)
			filter := catalog.ConnFilter{User: fUser, State: fState, App: fApp}

			used, max, err := catalog.ConnUsage(ctx, conn)
			if err != nil {
				return err
			}
			idle, err := catalog.ListIdleInTx(ctx, conn)
			if err != nil {
				return err
			}
			maybeNotePooler(cmd, ctx, conn)

			if detail {
				rowsD, err := catalog.ListConnectionDetail(ctx, conn, filter)
				if err != nil {
					return err
				}
				if format == render.FormatJSON {
					if rowsD == nil {
						rowsD = []catalog.ConnDetail{}
					}
					return render.Render(cmd.OutOrStdout(), format,
						map[string]any{"used": used, "max": max, "idle_in_transaction": idle, "connections": rowsD})
				}
				printConnHeader(cmd, used, max, idle)
				return render.Render(cmd.OutOrStdout(), render.FormatTable, connDetailView(rowsD))
			}

			groups, err := catalog.ListConnections(ctx, conn, filter)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if groups == nil {
					groups = []catalog.ConnStat{}
				}
				return render.Render(cmd.OutOrStdout(), format,
					map[string]any{"used": used, "max": max, "idle_in_transaction": idle, "by": groups})
			}
			printConnHeader(cmd, used, max, idle)
			return render.Render(cmd.OutOrStdout(), render.FormatTable, connectionsView(groups))
		},
	}
	c.Flags().BoolVar(&detail, "detail", false, "list each connection individually (client addr, age, idle time)")
	c.Flags().StringVar(&fUser, "user", "", "filter to a role/user (exact match)")
	c.Flags().StringVar(&fState, "state", "", "filter to a state (exact, e.g. 'active', 'idle in transaction')")
	c.Flags().StringVar(&fApp, "app", "", "filter by application_name (substring match)")
	return c
}

// poolerConcentration is the share of client backends from one address (and the minimum
// backend count) above which we suggest a connection pooler may be in front. Heuristic and
// deliberately conservative — it's an informational note, never a hard claim.
const (
	poolerConcentration = 0.8
	poolerMinBackends   = 10
)

// maybeNotePooler prints a best-effort note when one client address owns the large
// majority of backends — the classic sign these are a pooler's server connections, so
// the used/max figure reflects PgBouncer→Postgres links, not end-client connections.
func maybeNotePooler(cmd *cobra.Command, ctx context.Context, conn *db.Conn) {
	addr, count, total, err := catalog.TopClientAddr(ctx, conn)
	if err != nil || total < poolerMinBackends {
		return
	}
	if float64(count)/float64(total) >= poolerConcentration {
		fmt.Fprintf(cmd.ErrOrStderr(),
			"note: %d of %d client backends come from one host (%s). If that's a connection pooler (e.g. PgBouncer), these are pooled server connections, not end clients — the real client count lives in the pooler's own stats (`SHOW POOLS`).\n",
			count, total, addr)
	}
}

// printConnHeader writes the usage line and idle-in-transaction watch to stderr.
func printConnHeader(cmd *cobra.Command, used, max int64, idle []catalog.IdleInTx) {
	e := cmd.ErrOrStderr()
	pct := 0.0
	if max > 0 {
		pct = 100 * float64(used) / float64(max)
	}
	fmt.Fprintf(e, "Connections: %d / %d (%.0f%% of max_connections)\n", used, max, pct)
	if pct >= 80 {
		fmt.Fprintln(e, "⚠ approaching max_connections — risk of refused connections.")
	}
	if len(idle) > 0 {
		longest := secondsHuman(idle[0].IdleSec) // ListIdleInTx is longest-first
		fmt.Fprintf(e, "⚠ %d idle-in-transaction backend(s) — they hold locks and block vacuum; longest idle %s (pid %d).\n",
			len(idle), longest, idle[0].PID)
	}
}

type connectionsView []catalog.ConnStat

func (v connectionsView) Headers() []string {
	return []string{"DATABASE", "USER", "APPLICATION", "STATE", "COUNT"}
}
func (v connectionsView) Aligns() []render.Align {
	return []render.Align{render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignRight}
}
func (v connectionsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, c := range v {
		out = append(out, []string{c.Database, c.User, c.Application, c.State, withThousands(c.Count)})
	}
	return out
}

type connDetailView []catalog.ConnDetail

func (v connDetailView) Headers() []string {
	return []string{"PID", "USER", "DB", "APPLICATION", "CLIENT", "STATE", "AGE", "IDLE-FOR"}
}
func (v connDetailView) Aligns() []render.Align {
	return []render.Align{
		render.AlignRight, // PID
		render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft,
		render.AlignRight, render.AlignRight, // AGE, IDLE-FOR
	}
}
func (v connDetailView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, c := range v {
		out = append(out, []string{
			strconv.FormatInt(int64(c.PID), 10),
			dashIfEmpty(c.User), dashIfEmpty(c.Database), dashIfEmpty(c.Application),
			dashIfEmpty(c.ClientAddr), dashIfEmpty(c.State),
			secondsHuman(c.AgeSec), secondsHuman(c.IdleSec),
		})
	}
	return out
}

func newGetExtensionsCmd() *cobra.Command {
	var available bool
	c := &cobra.Command{
		Use:   "extensions",
		Short: "List installed extensions (or --available to install)",
		Long: "extensions lists the extensions installed in the current database, flagging any with\n" +
			"a newer version available (installed → default).\n\n" +
			"--available instead lists every extension installable on this server (what's on disk,\n" +
			"from pg_available_extensions) — its default version, whether it's already installed,\n" +
			"and whether it's TRUSTED (a non-superuser with CREATE on the database can install it,\n" +
			"PG13+). That answers 'what CAN I enable here', not just what's already on. Read-only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			ctx := context.Background()
			defer conn.Close(ctx)

			if available {
				avail, err := catalog.ListAvailableExtensions(ctx, conn)
				if err != nil {
					return err
				}
				if format == render.FormatJSON {
					if avail == nil {
						avail = []catalog.AvailableExtension{}
					}
					return render.Render(cmd.OutOrStdout(), format, avail)
				}
				if len(avail) == 0 {
					fmt.Fprintln(cmd.ErrOrStderr(), "no installable extensions found on this server")
					return nil
				}
				return render.Render(cmd.OutOrStdout(), render.FormatTable, availableExtensionsView(avail))
			}

			exts, err := catalog.ListExtensions(ctx, conn)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if exts == nil {
					exts = []catalog.Extension{}
				}
				return render.Render(cmd.OutOrStdout(), format, exts)
			}
			if len(exts) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no extensions installed")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, extensionsView(exts))
		},
	}
	c.Flags().BoolVar(&available, "available", false, "list extensions installable on this server (incl. whether installed / trusted), not just installed ones")
	return c
}

type availableExtensionsView []catalog.AvailableExtension

func (v availableExtensionsView) Headers() []string {
	return []string{"NAME", "DEFAULT", "INSTALLED", "TRUSTED", "DESCRIPTION"}
}
func (v availableExtensionsView) Aligns() []render.Align {
	return []render.Align{render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft}
}
func (v availableExtensionsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, a := range v {
		installed := "—" // not installed
		if a.Installed {
			installed = a.InstalledVersion
			if installed == "" {
				installed = "yes"
			}
		}
		out = append(out, []string{a.Name, a.DefaultVersion, installed, yesNo(a.Trusted), compactQuery(a.Description, 50)})
	}
	return out
}

// yesNo renders a bool as a compact table cell.
func yesNo(b bool) string {
	if b {
		return "yes"
	}
	return "no"
}

type extensionsView []catalog.Extension

func (v extensionsView) Headers() []string {
	return []string{"NAME", "VERSION", "SCHEMA", "DESCRIPTION"}
}
func (v extensionsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, e := range v {
		ver := e.InstalledVersion
		if e.DefaultVersion != "" && e.DefaultVersion != e.InstalledVersion {
			ver = fmt.Sprintf("%s → %s", e.InstalledVersion, e.DefaultVersion) // upgrade available
		}
		out = append(out, []string{e.Name, ver, e.Schema, compactQuery(e.Description, 50)})
	}
	return out
}

// connectForGet handles the shared format-parse + default-context + connect dance.
func connectForGet(cmd *cobra.Command) (render.Format, *db.Conn, error) {
	format, err := render.ParseFormat(flagOutput)
	if err != nil {
		return "", nil, usageError{err.Error()}
	}
	noteContext(cmd)
	conn, err := db.Connect(context.Background(), flagDSN, flagTimeout, flagDatabase, sqlLog(cmd))
	if err != nil {
		return "", nil, err
	}
	return format, conn, nil
}

func newGetViewsCmd() *cobra.Command {
	var schema string
	c := &cobra.Command{
		Use:   "views",
		Short: "List views and materialized views",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())
			views, err := catalog.ListViews(context.Background(), conn, schema)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if views == nil {
					views = []catalog.View{}
				}
				return render.Render(cmd.OutOrStdout(), format, views)
			}
			if len(views) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no views found")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, viewsView(views))
		},
	}
	c.Flags().StringVar(&schema, "schema", "", "limit to a single schema")
	return c
}

type viewsView []catalog.View

func (v viewsView) Headers() []string { return []string{"SCHEMA", "NAME", "TYPE", "OWNER", "SIZE"} }
func (v viewsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, x := range v {
		out = append(out, []string{x.Schema, x.Name, x.Type, x.Owner, x.Size})
	}
	return out
}

func newGetSequencesCmd() *cobra.Command {
	var schema string
	c := &cobra.Command{
		Use:   "sequences",
		Short: "List sequences with their last value",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())
			seqs, err := catalog.ListSequences(context.Background(), conn, schema)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if seqs == nil {
					seqs = []catalog.Sequence{}
				}
				return render.Render(cmd.OutOrStdout(), format, seqs)
			}
			if len(seqs) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no sequences found")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, sequencesView(seqs))
		},
	}
	c.Flags().StringVar(&schema, "schema", "", "limit to a single schema")
	return c
}

type sequencesView []catalog.Sequence

func (v sequencesView) Headers() []string {
	return []string{"SCHEMA", "NAME", "TYPE", "INCREMENT", "LAST-VALUE"}
}
func (v sequencesView) Aligns() []render.Align {
	return []render.Align{render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignRight, render.AlignRight}
}
func (v sequencesView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, s := range v {
		last := "—" // never used
		if s.LastValue != nil {
			last = withThousands(*s.LastValue)
		}
		out = append(out, []string{s.Schema, s.Name, s.DataType, withThousands(s.Increment), last})
	}
	return out
}

func newGetFunctionsCmd() *cobra.Command {
	var schema string
	c := &cobra.Command{
		Use:   "functions",
		Short: "List functions and procedures",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())
			fns, err := catalog.ListFunctions(context.Background(), conn, schema)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if fns == nil {
					fns = []catalog.Function{}
				}
				return render.Render(cmd.OutOrStdout(), format, fns)
			}
			if len(fns) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no functions found")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, functionsView(fns))
		},
	}
	c.Flags().StringVar(&schema, "schema", "", "limit to a single schema")
	return c
}

type functionsView []catalog.Function

func (v functionsView) Headers() []string {
	return []string{"SCHEMA", "NAME", "KIND", "ARGS", "RETURNS"}
}
func (v functionsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, f := range v {
		out = append(out, []string{f.Schema, f.Name, f.Kind, compactQuery(f.Args, 40), compactQuery(f.Result, 30)})
	}
	return out
}

func newGetSchemasCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "schemas",
		Short: "List schemas with table counts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			defer conn.Close(context.Background())
			schemas, err := catalog.ListSchemas(context.Background(), conn)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if schemas == nil {
					schemas = []catalog.Schema{}
				}
				return render.Render(cmd.OutOrStdout(), format, schemas)
			}
			if len(schemas) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no user schemas found")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, schemasView(schemas))
		},
	}
	return c
}

type schemasView []catalog.Schema

func (v schemasView) Headers() []string { return []string{"NAME", "OWNER", "TABLES"} }
func (v schemasView) Aligns() []render.Align {
	return []render.Align{render.AlignLeft, render.AlignLeft, render.AlignRight}
}
func (v schemasView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, s := range v {
		out = append(out, []string{s.Name, s.Owner, withThousands(s.Tables)})
	}
	return out
}

func newGetSlowQueriesCmd() *cobra.Command {
	var limit int
	var sortKey string
	var reset bool
	var force bool
	var full bool
	var allDB bool
	c := &cobra.Command{
		Use:   "slow-queries",
		Short: "Top queries from pg_stat_statements, sortable by total/mean/calls/rows/io/temp",
		Long: "slow-queries lists the heaviest queries from the pg_stat_statements extension. If\n" +
			"the extension isn't installed, pgdx tells you how to enable it rather than erroring.\n\n" +
			"pg_stat_statements is CLUSTER-WIDE, so by default pgdx scopes the list to the\n" +
			"connected database (the one -d selects). Pass --all-databases for every database's\n" +
			"statements in the instance (the raw cluster-wide view).\n\n" +
			"--sort picks the axis (default total):\n" +
			"  total   cumulative execution time — usually just the most-frequent query\n" +
			"  mean    average time per call — the slow-but-rare query total hides\n" +
			"  max     worst single execution — the latency spike, not the average\n" +
			"  stddev  most inconsistent — usually fast, occasionally catastrophic\n" +
			"  calls   call count\n" +
			"  rows    rows returned/affected\n" +
			"  io      physical block reads (cache misses) — what's thrashing shared_buffers\n" +
			"  temp    temp blocks — sorts/hashes spilling to disk (raise work_mem)\n\n" +
			"Columns include MAX (the worst single execution), STDDEV (a high one means the query\n" +
			"is sometimes catastrophic, not uniformly slow), and HIT% (shared-buffer cache hit\n" +
			"ratio). The QUERY column is\n" +
			"truncated to keep the table readable; use --full for the complete text (or -o json,\n" +
			"which is never truncated). Read-only.\n\n" +
			"--reset discards all accumulated statement statistics (pg_stat_statements_reset);\n" +
			"it asks to confirm (skip with --force). This resets STATISTICS only, never data —\n" +
			"useful to start a clean measurement window (pair with `pgdx snapshot`/`diff`).",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			if !catalog.ValidSlowQuerySort(sortKey) {
				return usageError{fmt.Sprintf("invalid --sort %q (want: %s)", sortKey, strings.Join(catalog.SlowQuerySortKeys, ", "))}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, err := db.Connect(ctx, flagDSN, flagTimeout, flagDatabase, sqlLog(cmd))
			if err != nil {
				return err
			}
			defer conn.Close(ctx)

			// D3: degrade gracefully when the extension isn't present.
			avail, err := catalog.PgStatStatementsAvailable(ctx, conn)
			if err != nil {
				return err
			}
			if !avail {
				e := cmd.ErrOrStderr()
				fmt.Fprintln(e, "pg_stat_statements is not available in this database.")
				fmt.Fprintln(e, "To enable it:")
				fmt.Fprintln(e, "  1. add 'pg_stat_statements' to shared_preload_libraries in postgresql.conf")
				fmt.Fprintln(e, "  2. restart Postgres")
				fmt.Fprintln(e, "  3. run: CREATE EXTENSION pg_stat_statements;")
				return nil // not an error — exit 0
			}

			if reset {
				return resetSlowQueries(cmd, ctx, conn, force)
			}

			if ok, _ := catalog.HasMonitorPrivilege(ctx, conn); !ok {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"note: limited privilege — you may only see your own statements (others need pg_monitor/pg_read_all_stats).")
			}

			qs, err := catalog.ListSlowQueries(ctx, conn, limit, sortKey, !allDB)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if qs == nil {
					qs = []catalog.SlowQuery{}
				}
				return render.Render(cmd.OutOrStdout(), format, qs)
			}
			if len(qs) == 0 {
				scope := fmt.Sprintf("in database %q", conn.Database())
				if allDB {
					scope = "in the cluster"
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "no statements recorded yet %s\n", scope)
				return nil
			}
			scope := fmt.Sprintf("database %q", conn.Database())
			if allDB {
				scope = "all databases (cluster-wide)"
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "%s, sorted by %s; TEMP is on-disk sort/hash traffic, HIT%% is the shared-buffer cache hit ratio.\n", scope, sortKey)
			return render.Render(cmd.OutOrStdout(), render.FormatTable, slowQueriesView{rows: qs, full: full})
		},
	}
	c.Flags().IntVar(&limit, "limit", 20, "number of queries to show")
	c.Flags().StringVar(&sortKey, "sort", "total", "sort axis: total | mean | max | stddev | calls | rows | io | temp")
	c.Flags().BoolVar(&reset, "reset", false, "reset pg_stat_statements counters (statistics only, not data; asks to confirm)")
	c.Flags().BoolVar(&force, "force", false, "skip the --reset confirmation prompt")
	c.Flags().BoolVar(&full, "full", false, "show the full query text (don't truncate the QUERY column)")
	c.Flags().BoolVar(&allDB, "all-databases", false, "show statements from every database in the cluster, not just the connected one")
	return c
}

// resetSlowQueries clears pg_stat_statements after confirmation — a fresh measurement
// window. It's a stats-only write, guarded like vacuum --full.
func resetSlowQueries(cmd *cobra.Command, ctx context.Context, conn *db.Conn, force bool) error {
	if !force {
		ok, err := promptConfirm(cmd, "Reset ALL pg_stat_statements counters? This clears query history cluster-wide (statistics only, not data). [y/N]: ")
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(cmd.ErrOrStderr(), "aborted.")
			return nil
		}
	}
	if err := catalog.ResetStatStatements(ctx, conn); err != nil {
		return err
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "pg_stat_statements reset. New statistics will accumulate from now.")
	return nil
}

// slowQueriesView renders pg_stat_statements rows. full=true shows the complete query
// text (still one-lined) instead of truncating QUERY — QUERY is the last, left-aligned
// column, so an un-truncated query only lengthens its own row. Mirrors activityView.
type slowQueriesView struct {
	rows []catalog.SlowQuery
	full bool
}

func (v slowQueriesView) Headers() []string {
	return []string{"CALLS", "TOTAL", "MEAN", "MAX", "STDDEV", "ROWS", "HIT%", "TEMP", "QUERY"}
}
func (v slowQueriesView) Aligns() []render.Align {
	return []render.Align{
		render.AlignRight, render.AlignRight, render.AlignRight, render.AlignRight,
		render.AlignRight, render.AlignRight, render.AlignRight, render.AlignRight, render.AlignLeft,
	}
}
func (v slowQueriesView) Rows() [][]string {
	out := make([][]string, 0, len(v.rows))
	for _, s := range v.rows {
		query := compactQuery(s.Query, 50)
		if v.full {
			query = flattenQuery(s.Query)
		}
		out = append(out, []string{
			withThousands(s.Calls), msHuman(s.TotalMs), msHuman(s.MeanMs), msHuman(s.MaxMs), msHuman(s.StddevMs),
			withThousands(s.Rows), hitPct(s.HitPct), tempTraffic(s.TempRead, s.TempWritten),
			query,
		})
	}
	return out
}

// hitPct renders the shared-buffer cache hit ratio; "—" when the query touched no blocks.
func hitPct(pct float64) string {
	if pct < 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", pct)
}

// tempTraffic renders temp-block traffic (read+written) as bytes, assuming the default
// 8 KB block size. "0" when nothing spilled to disk.
func tempTraffic(read, written int64) string {
	blocks := read + written
	if blocks <= 0 {
		return "0"
	}
	return humanBytes(blocks * 8192)
}

func newGetLocksCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "locks",
		Short: "Show sessions waiting on locks and who's blocking them",
		Long: "locks shows ungranted lock requests — the sessions actually stuck — with the\n" +
			"lock mode, the object, and which PIDs hold the conflicting lock. An empty result\n" +
			"is the healthy state (nothing is blocked). Read-only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, err := db.Connect(ctx, flagDSN, flagTimeout, flagDatabase, sqlLog(cmd))
			if err != nil {
				return err
			}
			defer conn.Close(ctx)

			waits, err := catalog.ListLockWaits(ctx, conn)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if waits == nil {
					waits = []catalog.LockWait{}
				}
				return render.Render(cmd.OutOrStdout(), format, waits)
			}
			if len(waits) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no lock waits — nothing is currently blocked")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, locksView(waits))
		},
	}
	return c
}

type locksView []catalog.LockWait

func (v locksView) Headers() []string {
	return []string{"PID", "USER", "LOCKTYPE", "MODE", "OBJECT", "BLOCKED-BY", "QUERY"}
}
func (v locksView) Aligns() []render.Align {
	return []render.Align{
		render.AlignRight, // PID
		render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft,
	}
}
func (v locksView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, w := range v {
		out = append(out, []string{
			strconv.FormatInt(int64(w.PID), 10),
			dashIfEmpty(w.User), w.LockType, w.Mode, w.Object,
			dashIfEmpty(w.BlockedBy), compactQuery(w.Query, 50),
		})
	}
	return out
}

// msHuman formats a millisecond duration: <1ms, Nms, or seconds via secondsHuman.
func msHuman(ms float64) string {
	switch {
	case ms <= 0:
		return "0"
	case ms < 1:
		return "<1ms"
	case ms < 1000:
		return fmt.Sprintf("%.0fms", ms)
	default:
		return secondsHuman(ms / 1000)
	}
}

func newGetActivityCmd() *cobra.Command {
	var all bool
	var full bool
	var sort string
	var datname string
	var minDuration time.Duration
	var watch time.Duration
	c := &cobra.Command{
		Use:   "activity",
		Short: "Show current sessions (running queries, waits, blocking)",
		Long: "activity lists sessions from pg_stat_activity: what's running, how long, what\n" +
			"they're waiting on, and which PIDs are blocking them. By default it shows active\n" +
			"client sessions (use --all for idle + background). Read-only.\n\n" +
			"The QUERY column is truncated to keep the table readable; use --full for the complete\n" +
			"text (or -o json, which is never truncated).\n\n" +
			"DURATION is time in the CURRENT state: for an active session it's how long the query\n" +
			"has been running; for an idle (or idle-in-transaction) session it's how long it's been\n" +
			"sitting in that state — so an idle row with a big DURATION is a parked connection, NOT\n" +
			"a long-running query. Always read DURATION together with STATE.\n\n" +
			"pg_stat_activity is CLUSTER-WIDE, so the global -d/--database (which only changes\n" +
			"which database pgdx connects to) does not scope this list. To narrow to one\n" +
			"database, use --datname: it filters the cluster-wide view and needs no CONNECT on\n" +
			"the named database (so you can stay connected to one you can reach). --datname and\n" +
			"--all are independent: --datname picks the database, --all decides whether idle\n" +
			"sessions are included — so to see idle connections on a database you need both.\n\n" +
			"Sorted blocked-first by default (the stuck sessions you're chasing in an incident\n" +
			"surface at the top), then by duration. Use --sort duration for longest-running first.\n\n" +
			"--min-duration keeps only sessions that have been in their current state at least that\n" +
			"long (e.g. --min-duration 30s); read with the default view (idle hidden) that's the\n" +
			"long-running-query filter. --watch re-renders on an interval (bare --watch = 2s) until\n" +
			"Ctrl-C. Together — `pgdx get activity --watch --min-duration 30s --sort duration` — they\n" +
			"are a live monitor for runaway queries; spot one and `pgdx cancel`/`kill` its PID.\n\n" +
			"Privilege note: without pg_monitor (or superuser) Postgres hides other users'\n" +
			"query text — pgdx warns when that's the case.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			switch sort {
			case "blocked", "duration":
			default:
				return usageError{fmt.Sprintf("invalid --sort %q (want: blocked, duration)", sort)}
			}
			if minDuration < 0 {
				return usageError{"--min-duration can't be negative"}
			}
			if watch < 0 {
				return usageError{"--watch interval can't be negative"}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, err := db.Connect(ctx, flagDSN, flagTimeout, flagDatabase, sqlLog(cmd))
			if err != nil {
				return err
			}
			defer conn.Close(ctx)

			// D8: warn if this role can't see other sessions' query text.
			if ok, perr := catalog.HasMonitorPrivilege(ctx, conn); perr == nil && !ok {
				fmt.Fprintln(cmd.ErrOrStderr(),
					"note: limited privilege — other users' query text will show as '<insufficient privilege>'. Grant pg_monitor for full visibility.")
			}

			out := cmd.OutOrStdout()
			minDurSec := minDuration.Seconds()

			// emit renders one snapshot and reports how many sessions matched, so the caller
			// can show a tailored "nothing matched" line. Shared by single-shot and --watch.
			emit := func() (int, error) {
				sessions, err := catalog.ListActivity(ctx, conn, all, sort, datname, minDurSec)
				if err != nil {
					return 0, err
				}
				if format == render.FormatJSON {
					if sessions == nil {
						sessions = []catalog.Activity{}
					}
					return len(sessions), render.Render(out, format, sessions)
				}
				if len(sessions) > 0 {
					return len(sessions), render.Render(out, render.FormatTable, activityView{rows: sessions, full: full})
				}
				return 0, nil
			}

			if watch > 0 {
				// In watch mode the empty line shares the cleared screen (stdout), so an idle
				// monitor shows something rather than a blank panel.
				return watchLoop(cmd, watch, format == render.FormatJSON, func() error {
					n, err := emit()
					if err != nil {
						return err
					}
					if n == 0 && format != render.FormatJSON {
						fmt.Fprintln(out, activityEmptyMsg(datname, minDuration))
					}
					return nil
				})
			}

			n, err := emit()
			if err != nil {
				return err
			}
			if n == 0 && format != render.FormatJSON {
				fmt.Fprintln(cmd.ErrOrStderr(), activityEmptyMsg(datname, minDuration))
			}
			return nil
		},
	}
	c.Flags().BoolVar(&all, "all", false, "include idle connections and background workers")
	c.Flags().StringVar(&sort, "sort", "blocked", "sort order: blocked (blocked sessions first) | duration (longest-running first)")
	c.Flags().StringVar(&datname, "datname", "", "filter to one database (cluster-wide view; no CONNECT needed, unlike -d)")
	c.Flags().BoolVar(&full, "full", false, "show the full query text (don't truncate the QUERY column)")
	c.Flags().DurationVar(&minDuration, "min-duration", 0,
		"only show sessions in their current state at least this long (e.g. 30s, 5m) — the long-running-query filter")
	c.Flags().DurationVar(&watch, "watch", 0,
		"re-render on a repeating `interval` (bare --watch = 2s; e.g. --watch=5s); Ctrl-C to stop")
	c.Flags().Lookup("watch").NoOptDefVal = "2s"
	return c
}

// activityEmptyMsg tailors the "nothing to show" line to the active filters, so a quiet
// monitor reads as "nothing matched these filters" rather than a bare empty result.
func activityEmptyMsg(datname string, minDur time.Duration) string {
	switch {
	case minDur > 0 && datname != "":
		return fmt.Sprintf("no sessions on %q running longer than %s", datname, minDur)
	case minDur > 0:
		return fmt.Sprintf("no sessions running longer than %s", minDur)
	case datname != "":
		return fmt.Sprintf("no active sessions on %q (try --all to include idle/background)", datname)
	default:
		return "no active sessions (try --all to include idle/background)"
	}
}

// activityView renders sessions. full=true shows the complete query text (still
// one-lined) instead of truncating the QUERY column. QUERY is the last column and
// left-aligned, so an un-truncated query only lengthens its own row — it doesn't pad
// the whole table.
type activityView struct {
	rows []catalog.Activity
	full bool
}

func (v activityView) Headers() []string {
	return []string{"PID", "USER", "DB", "STATE", "WAIT", "DURATION", "BLOCKED-BY", "CLIENT", "QUERY"}
}

func (v activityView) Aligns() []render.Align {
	return []render.Align{
		render.AlignRight, // PID
		render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft,
		render.AlignRight,                                    // DURATION
		render.AlignLeft, render.AlignLeft, render.AlignLeft, // BLOCKED-BY, CLIENT, QUERY
	}
}

func (v activityView) Rows() [][]string {
	out := make([][]string, 0, len(v.rows))
	for _, a := range v.rows {
		wait := dashIfEmpty(joinWait(a.WaitEventType, a.WaitEvent))
		blocked := dashIfEmpty(a.BlockedBy)
		query := compactQuery(a.Query, 60)
		if v.full {
			query = flattenQuery(a.Query)
		}
		out = append(out, []string{
			strconv.FormatInt(int64(a.PID), 10),
			dashIfEmpty(a.User), dashIfEmpty(a.Database), dashIfEmpty(a.State),
			wait, secondsHuman(a.DurationSec), blocked, dashIfEmpty(a.ClientAddr), query,
		})
	}
	return out
}

func joinWait(t, e string) string {
	switch {
	case t == "" && e == "":
		return ""
	case e == "":
		return t
	default:
		return t + ":" + e
	}
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}

// secondsHuman formats a duration in seconds; -1 (unknown) renders as "—".
func secondsHuman(s float64) string {
	switch {
	case s < 0:
		return "—"
	case s < 1:
		return fmt.Sprintf("%.0fms", s*1000)
	case s < 60:
		return fmt.Sprintf("%.0fs", s)
	case s < 3600:
		return fmt.Sprintf("%dm%02ds", int(s)/60, int(s)%60)
	default:
		return fmt.Sprintf("%dh%02dm", int(s)/3600, (int(s)%3600)/60)
	}
}

// flattenQuery collapses all whitespace (incl. newlines) to single spaces so a query
// stays on one line in the table — without truncating it.
func flattenQuery(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// compactQuery flattens and truncates for the table view; -o json keeps the full query.
func compactQuery(s string, max int) string {
	s = flattenQuery(s)
	r := []rune(s)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

func newGetTablesCmd() *cobra.Command {
	var schema string
	var usage bool
	c := &cobra.Command{
		Use:   "tables",
		Short: "List tables (all non-system schemas by default)",
		Long: "tables lists ordinary and partitioned tables across every non-system schema,\n" +
			"with a SCHEMA column so nothing is hidden the way \\dt hides non-search_path\n" +
			"schemas. Use --schema to narrow to one. Read-only; needs no special privilege.\n\n" +
			"--usage swaps the size/bloat columns for access patterns: sequential vs index\n" +
			"scans (with IDX% — the share served by an index) and insert/update/delete counts,\n" +
			"most sequentially-scanned first. A big, heavily seq-scanned table with a low IDX%\n" +
			"is a prime candidate for a better index.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}

			noteContext(cmd)
			ctx := context.Background()
			conn, err := db.Connect(ctx, flagDSN, flagTimeout, flagDatabase, sqlLog(cmd))
			if err != nil {
				return err
			}
			defer conn.Close(ctx)

			if usage {
				return runTableUsage(cmd, ctx, conn, schema, format)
			}

			tables, err := catalog.ListTables(ctx, conn, schema)
			if err != nil {
				return err
			}

			if format == render.FormatJSON {
				// Never emit a bare `null` for an empty result — use [].
				if tables == nil {
					tables = []catalog.Table{}
				}
				return render.Render(cmd.OutOrStdout(), format, tables)
			}
			if len(tables) == 0 {
				msg := "no tables found"
				if schema != "" {
					msg += fmt.Sprintf(" in schema %q", schema)
				}
				fmt.Fprintln(cmd.ErrOrStderr(), msg)
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, tablesView(tables))
		},
	}
	c.Flags().StringVar(&schema, "schema", "", "limit to a single schema (default: all non-system schemas)")
	c.Flags().BoolVar(&usage, "usage", false, "show read/write access patterns (seq vs index scans, ins/upd/del) instead of size")
	return c
}

func runTableUsage(cmd *cobra.Command, ctx context.Context, conn *db.Conn, schema string, format render.Format) error {
	rows, err := catalog.ListTableUsage(ctx, conn, schema)
	if err != nil {
		return err
	}
	if format == render.FormatJSON {
		if rows == nil {
			rows = []catalog.TableUsage{}
		}
		return render.Render(cmd.OutOrStdout(), format, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "no tables found")
		return nil
	}
	return render.Render(cmd.OutOrStdout(), render.FormatTable, tableUsageView(rows))
}

type tableUsageView []catalog.TableUsage

func (v tableUsageView) Headers() []string {
	return []string{"SCHEMA", "NAME", "SEQ-SCAN", "IDX-SCAN", "IDX%", "INS", "UPD", "DEL"}
}
func (v tableUsageView) Aligns() []render.Align {
	return []render.Align{
		render.AlignLeft, render.AlignLeft,
		render.AlignRight, render.AlignRight, render.AlignRight,
		render.AlignRight, render.AlignRight, render.AlignRight,
	}
}
func (v tableUsageView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, u := range v {
		idxPct := "—" // never scanned
		if u.IdxPct >= 0 {
			idxPct = fmt.Sprintf("%.0f%%", u.IdxPct)
		}
		out = append(out, []string{
			u.Schema, u.Name,
			withThousands(u.SeqScan), withThousands(u.IdxScan), idxPct,
			withThousands(u.Inserts), withThousands(u.Updates), withThousands(u.Deletes),
		})
	}
	return out
}

func newGetBloatCmd() *cobra.Command {
	var schema string
	var limit int
	c := &cobra.Command{
		Use:   "bloat",
		Short: "Rank tables by estimated reclaimable (dead-tuple) space",
		Long: "bloat is a whole-database leaderboard of the tables where VACUUM would reclaim the\n" +
			"most space, so you don't have to `describe` each one to find the problem. EST-WASTE\n" +
			"is an ESTIMATE — the dead-tuple fraction applied to the table's heap size — computed\n" +
			"from already-collected statistics, so it adds no load (it does not scan pages the way\n" +
			"pgstattuple would). Treat it as 'where vacuuming pays off most', not an exact figure.\n\n" +
			"Plain VACUUM makes this space reusable in place; VACUUM FULL (blocking) returns it to\n" +
			"the OS. Tables with zero dead tuples are omitted. Read-only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			ctx := context.Background()
			defer conn.Close(ctx)
			rows, err := catalog.ListTableBloat(ctx, conn, schema, limit)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if rows == nil {
					rows = []catalog.TableBloat{}
				}
				return render.Render(cmd.OutOrStdout(), format, rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no bloat detected — no tables have dead tuples (or stats were recently reset)")
				return nil
			}
			fmt.Fprintln(cmd.ErrOrStderr(), "EST-WASTE is an estimate from dead-tuple stats; for an exact figure use the pgstattuple extension.")
			return render.Render(cmd.OutOrStdout(), render.FormatTable, bloatView(rows))
		},
	}
	c.Flags().StringVar(&schema, "schema", "", "limit to a single schema")
	c.Flags().IntVar(&limit, "limit", 20, "number of tables to show")
	return c
}

type bloatView []catalog.TableBloat

func (v bloatView) Headers() []string {
	return []string{"SCHEMA", "NAME", "SIZE", "DEAD%", "EST-WASTE", "LAST-VACUUM"}
}
func (v bloatView) Aligns() []render.Align {
	return []render.Align{
		render.AlignLeft, render.AlignLeft, render.AlignRight,
		render.AlignRight, render.AlignRight, render.AlignLeft,
	}
}
func (v bloatView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, b := range v {
		last := dashIfEmpty(b.LastVacuum)
		if len(last) >= 10 && last != "—" {
			last = last[:10] // date is enough
		}
		out = append(out, []string{
			b.Schema, b.Name, humanBytes(b.SizeBytes),
			fmt.Sprintf("%.0f%%", 100*b.DeadRatio), humanBytes(b.WasteBytes), last,
		})
	}
	return out
}

func newGetTransactionAgeCmd() *cobra.Command {
	var min time.Duration
	c := &cobra.Command{
		Use:     "transaction-age",
		Aliases: []string{"xact-age"},
		Short:   "Show open transactions, oldest first (they hold back VACUUM)",
		Long: "transaction-age lists backends with an open transaction, oldest first. A long-lived\n" +
			"transaction — even one sitting idle in transaction — pins the xmin horizon, so VACUUM\n" +
			"can't remove dead rows newer than it ANYWHERE in the database. That's a leading cause\n" +
			"of creeping, database-wide bloat, and of XID-wraparound pressure.\n\n" +
			"XACT-AGE is how long the transaction has been open; STATE-AGE is how long it's sat in\n" +
			"its current state (a large STATE-AGE on 'idle in transaction' is the classic offender).\n" +
			"Use --min to hide short transactions (e.g. --min 30s). Read-only.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			ctx := context.Background()
			defer conn.Close(ctx)
			txns, err := catalog.ListLongTransactions(ctx, conn, min.Seconds())
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if txns == nil {
					txns = []catalog.LongTxn{}
				}
				return render.Render(cmd.OutOrStdout(), format, txns)
			}
			if len(txns) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no open transactions match — nothing holding back the xmin horizon")
				return nil
			}
			// The oldest transaction sets the cluster-wide cleanup horizon; call it out.
			oldest := txns[0]
			fmt.Fprintf(cmd.ErrOrStderr(),
				"⚠ oldest open transaction: pid %d, %s (state: %s) — VACUUM can't clean rows newer than it anywhere in the database.\n",
				oldest.PID, secondsHuman(oldest.XactSec), dashIfEmpty(oldest.State))
			return render.Render(cmd.OutOrStdout(), render.FormatTable, longTxnView(txns))
		},
	}
	c.Flags().DurationVar(&min, "min", 0, "only show transactions at least this old (e.g. 30s, 5m)")
	return c
}

type longTxnView []catalog.LongTxn

func (v longTxnView) Headers() []string {
	return []string{"PID", "USER", "DB", "STATE", "XACT-AGE", "STATE-AGE", "WAIT", "QUERY"}
}
func (v longTxnView) Aligns() []render.Align {
	return []render.Align{
		render.AlignRight, render.AlignLeft, render.AlignLeft, render.AlignLeft,
		render.AlignRight, render.AlignRight, render.AlignLeft, render.AlignLeft,
	}
}
func (v longTxnView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, t := range v {
		out = append(out, []string{
			strconv.FormatInt(int64(t.PID), 10),
			dashIfEmpty(t.User), dashIfEmpty(t.Database), dashIfEmpty(t.State),
			secondsHuman(t.XactSec), secondsHuman(t.StateSec),
			dashIfEmpty(t.WaitEvent), compactQuery(t.Query, 50),
		})
	}
	return out
}

func newGetVacuumHealthCmd() *cobra.Command {
	var limit int
	var schema string
	c := &cobra.Command{
		Use:     "vacuum-health",
		Aliases: []string{"wraparound"},
		Short:   "Show transaction-ID age — wraparound risk per relation",
		Long: "vacuum-health ranks relations by transaction-ID age (age of relfrozenxid): how many\n" +
			"transactions have elapsed since the rows were last frozen. As this approaches\n" +
			"autovacuum_freeze_max_age, Postgres forces an anti-wraparound autovacuum; if XIDs were\n" +
			"ever actually exhausted the database stops accepting writes. TO-FREEZE% is XID-age as a\n" +
			"share of that threshold — anything climbing past ~100% means a forced vacuum is due (or\n" +
			"overdue, often because a long transaction or a stuck autovacuum is holding the horizon).\n\n" +
			"Wraparound is cluster-wide, so by default this includes system and TOAST relations, not\n" +
			"just your tables. A TOAST relation is shown with the table it backs (NAME → owner) so it\n" +
			"is actionable. --schema narrows to one schema but still keeps the TOAST relations of\n" +
			"that schema's tables (they physically live in pg_toast, yet are often the real risk).\n" +
			"Read-only. Pair with `get transaction-age` when ages stay stubbornly high.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			ctx := context.Background()
			defer conn.Close(ctx)
			rows, err := catalog.ListWraparoundRisk(ctx, conn, schema, limit)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				if rows == nil {
					rows = []catalog.WraparoundRisk{}
				}
				return render.Render(cmd.OutOrStdout(), format, rows)
			}
			if len(rows) == 0 {
				msg := "no relations with a frozen-XID age to report"
				if schema != "" {
					msg += fmt.Sprintf(" in schema %q", schema)
				}
				fmt.Fprintln(cmd.ErrOrStderr(), msg)
				return nil
			}
			if top := rows[0]; top.PctToFreeze >= 50 {
				fmt.Fprintf(cmd.ErrOrStderr(),
					"⚠ %q is at %.0f%% of autovacuum_freeze_max_age — a forced anti-wraparound vacuum is approaching.\n",
					wraparoundLabel(top), top.PctToFreeze)
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, wraparoundView(rows))
		},
	}
	c.Flags().IntVar(&limit, "limit", 20, "number of relations to show")
	c.Flags().StringVar(&schema, "schema", "",
		"limit to one schema (still includes the TOAST relations of that schema's tables)")
	return c
}

// wraparoundLabel identifies a relation for a human: a TOAST relation is named by the
// table it backs, since its own pg_toast_<oid> name is not actionable on its own.
func wraparoundLabel(w catalog.WraparoundRisk) string {
	if w.Owner != "" {
		return w.Owner + " (TOAST)"
	}
	return w.Schema + "." + w.Name
}

type wraparoundView []catalog.WraparoundRisk

func (v wraparoundView) Headers() []string {
	return []string{"SCHEMA", "NAME", "XID-AGE", "TO-FREEZE%", "SIZE", "LAST-VACUUM"}
}
func (v wraparoundView) Aligns() []render.Align {
	return []render.Align{
		render.AlignLeft, render.AlignLeft, render.AlignRight,
		render.AlignRight, render.AlignRight, render.AlignLeft,
	}
}
func (v wraparoundView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, w := range v {
		last := dashIfEmpty(w.LastVacuum)
		if len(last) >= 10 && last != "—" {
			last = last[:10]
		}
		name := w.Name
		if w.Owner != "" { // TOAST relation: point at the table it backs
			name += " → " + w.Owner
		}
		out = append(out, []string{
			w.Schema, name, withThousands(w.XIDAge),
			fmt.Sprintf("%.0f%%", w.PctToFreeze), w.Size, last,
		})
	}
	return out
}

func newGetIndexesCmd() *cobra.Command {
	var schema, table string
	var unused bool
	var redundant bool
	var sortKey string
	c := &cobra.Command{
		Use:   "indexes",
		Short: "List indexes, with scan counts (0 scans = unused; --redundant = duplicate-of)",
		Long: "indexes lists indexes across non-system schemas. The SCANS column comes from\n" +
			"pg_stat_user_indexes — an index with 0 scans is likely dead weight (it slows\n" +
			"writes and wastes space). Use --table/--schema to narrow. Read-only.\n" +
			"The full index definition is included in -o json.\n\n" +
			"--sort orders the list: name (default), size (biggest first), or scans (most-used\n" +
			"first). It applies to the normal and --unused lists (--unused still defaults to\n" +
			"size); --redundant has its own structural ordering and ignores --sort.\n\n" +
			"--unused shows only never-scanned, non-unique indexes (drop candidates), biggest\n" +
			"first. SCANS is cumulative since the last stats reset, so verify stats age before\n" +
			"dropping (SELECT stats_reset FROM pg_stat_database WHERE datname=current_database()).\n\n" +
			"--redundant shows non-unique indexes whose leading columns are already covered by\n" +
			"another index on the same table — an exact duplicate, or a prefix of a wider index.\n" +
			"These earn nothing on reads while still costing write amplification and disk, so\n" +
			"they're safe, high-confidence drop candidates (unlike --unused, this needs no usage\n" +
			"history — it's structural). Partial and expression indexes are excluded, since their\n" +
			"equivalence can't be judged from columns alone.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			if unused && redundant {
				return usageError{"pass either --unused or --redundant, not both"}
			}
			if !catalog.ValidIndexSort(sortKey) {
				return usageError{fmt.Sprintf("invalid --sort %q (want: %s)", sortKey, strings.Join(catalog.IndexSortKeys, ", "))}
			}
			if redundant && cmd.Flags().Changed("sort") {
				return usageError{"--sort doesn't apply to --redundant (it lists structural duplicates in its own order)"}
			}
			// --unused has always shown biggest-wasted-space first; preserve that as its
			// default while still letting an explicit --sort override it.
			if unused && !cmd.Flags().Changed("sort") {
				sortKey = "size"
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, err := db.Connect(ctx, flagDSN, flagTimeout, flagDatabase, sqlLog(cmd))
			if err != nil {
				return err
			}
			defer conn.Close(ctx)

			if redundant {
				return runRedundantIndexes(cmd, ctx, conn, schema, table, format)
			}

			indexes, err := catalog.ListIndexes(ctx, conn, schema, table, unused, sortKey)
			if err != nil {
				return err
			}
			if unused {
				printUnusedCaveat(cmd, ctx, conn)
			}
			if format == render.FormatJSON {
				if indexes == nil {
					indexes = []catalog.Index{}
				}
				return render.Render(cmd.OutOrStdout(), format, indexes)
			}
			if len(indexes) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no indexes found")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, indexesView(indexes))
		},
	}
	c.Flags().StringVar(&schema, "schema", "", "limit to a single schema")
	c.Flags().StringVar(&table, "table", "", "limit to indexes on a single table")
	c.Flags().BoolVar(&unused, "unused", false, "show only never-scanned, non-unique indexes (drop candidates), biggest first")
	c.Flags().BoolVar(&redundant, "redundant", false, "show non-unique indexes already covered by another index (duplicate or prefix)")
	c.Flags().StringVar(&sortKey, "sort", "name", "sort order: name | size (biggest first) | scans (most-used first)")
	return c
}

func runRedundantIndexes(cmd *cobra.Command, ctx context.Context, conn *db.Conn, schema, table string, format render.Format) error {
	red, err := catalog.ListRedundantIndexes(ctx, conn, schema, table)
	if err != nil {
		return err
	}
	if format == render.FormatJSON {
		if red == nil {
			red = []catalog.RedundantIndex{}
		}
		return render.Render(cmd.OutOrStdout(), format, red)
	}
	if len(red) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "no redundant indexes found — every non-unique index covers distinct leading columns")
		return nil
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "these indexes are covered by another (COVERED-BY); confirm the covering index is healthy before dropping.")
	return render.Render(cmd.OutOrStdout(), render.FormatTable, redundantIndexesView(red))
}

type redundantIndexesView []catalog.RedundantIndex

func (v redundantIndexesView) Headers() []string {
	return []string{"SCHEMA", "TABLE", "NAME", "SIZE", "SCANS", "COVERED-BY", "REASON"}
}
func (v redundantIndexesView) Aligns() []render.Align {
	return []render.Align{
		render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignRight,
		render.AlignRight, render.AlignLeft, render.AlignLeft,
	}
}
func (v redundantIndexesView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, r := range v {
		out = append(out, []string{
			r.Schema, r.Table, r.Name, r.Size, withThousands(r.Scans), r.CoveredBy, r.Reason,
		})
	}
	return out
}

// printUnusedCaveat fetches stats_reset and prints a self-verifying caveat: how long
// the 0-scan window is and whether it's long enough to trust.
func printUnusedCaveat(cmd *cobra.Command, ctx context.Context, conn *db.Conn) {
	e := cmd.ErrOrStderr()
	resetText, ageSec, err := catalog.StatsResetAge(ctx, conn)
	if err != nil || (resetText == "" && ageSec < 0) {
		// NULL stats_reset = never explicitly reset. But Postgres records no start time
		// for the window, and a brand-NEW database also shows NULL — so this isn't
		// automatically trustworthy.
		fmt.Fprintln(e, "showing non-unique 0-scan indexes; stats were never explicitly reset, so this spans the database's full history — but Postgres records no start date, and a newly-created database looks the same. Trust the 0s only if this database isn't recent (confirm with `get databases --sample`).")
		return
	}
	when := resetText
	if len(when) >= 10 {
		when = when[:10] // date is enough
	}
	var verdict string
	switch {
	case ageSec >= 30*86400:
		verdict = "long enough to trust for most usage cycles."
	case ageSec >= 7*86400:
		verdict = "covers daily/weekly use, but may miss monthly jobs — check before dropping."
	default:
		verdict = "a SHORT window — these may just be idle right now, not truly unused. Don't drop yet."
	}
	fmt.Fprintf(e, "showing non-unique indexes with 0 scans over the last %s (stats reset %s) — %s\n",
		ageHuman(ageSec), when, verdict)
}

// ageHuman renders a long duration: days when >= 1 day, else falls back to secondsHuman.
func ageHuman(sec float64) string {
	if sec < 0 {
		return "unknown"
	}
	if d := sec / 86400; d >= 1 {
		return fmt.Sprintf("%.0f days", d)
	}
	return secondsHuman(sec)
}

type indexesView []catalog.Index

func (v indexesView) Headers() []string {
	return []string{"SCHEMA", "TABLE", "NAME", "UNIQUE", "SIZE", "SCANS"}
}

func (v indexesView) Aligns() []render.Align {
	return []render.Align{
		render.AlignLeft, render.AlignLeft, render.AlignLeft,
		render.AlignLeft, render.AlignLeft, render.AlignRight, // SCANS
	}
}

func (v indexesView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, ix := range v {
		unique := "no"
		if ix.Unique {
			unique = "yes"
		}
		out = append(out, []string{ix.Schema, ix.Table, ix.Name, unique, ix.Size, withThousands(ix.Scans)})
	}
	return out
}

type tablesView []catalog.Table

func (v tablesView) Headers() []string {
	return []string{"SCHEMA", "NAME", "OWNER", "SIZE", "ROWS", "DEAD%"}
}

// Aligns right-aligns the trailing numeric ROWS and DEAD% columns.
func (v tablesView) Aligns() []render.Align {
	return []render.Align{render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignLeft, render.AlignRight, render.AlignRight}
}

func (v tablesView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, t := range v {
		rows := "—" // never analyzed (reltuples = -1) reads as unknown, not "0"
		if t.EstRows >= 0 {
			rows = withThousands(t.EstRows) // table view only; JSON keeps the raw int
		}
		out = append(out, []string{t.Schema, t.Name, t.Owner, t.Size, rows, deadPct(t.LiveTup, t.DeadTup)})
	}
	return out
}

// deadPct is the dead-tuple ratio for the table list; "—" when there are no stats.
func deadPct(live, dead int64) string {
	total := live + dead
	if total <= 0 {
		return "—"
	}
	return fmt.Sprintf("%.0f%%", 100*float64(dead)/float64(total))
}

// withThousands formats a non-negative integer with comma group separators
// (500000 -> "500,000") for human-readable table output.
func withThousands(n int64) string {
	s := strconv.FormatInt(n, 10)
	if len(s) <= 3 {
		return s
	}
	var b strings.Builder
	pre := len(s) % 3
	if pre > 0 {
		b.WriteString(s[:pre])
		b.WriteByte(',')
	}
	for i := pre; i < len(s); i += 3 {
		b.WriteString(s[i : i+3])
		if i+3 < len(s) {
			b.WriteByte(',')
		}
	}
	return b.String()
}
