package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/db"
	"github.com/capitaltg/pgdx/internal/render"
)

// status is the triage front door: one connection, a handful of fast checks, and a
// plain verdict on whether anything needs attention RIGHT NOW. It does not introduce
// new data — every check composes an existing catalog query — it just samples the
// signals that mean "something's on fire" and stays quiet about the rest, then points
// at the command to drill into. Read-only and stateless, like the rest of pgdx.

// Severity of one status check.
const (
	sevCrit = "crit" // needs attention now
	sevOK   = "ok"   // checked, healthy
	sevInfo = "info" // neutral / informational / not applicable
)

// Thresholds. Kept conservative so a healthy database produces a calm, mostly-OK
// screen — silence is the signal. connSaturatedPct matches `get connections`' own warn.
const (
	connSaturatedPct    = 80.0    // % of max_connections that counts as a warning
	idleTxCritSec       = 60.0    // an idle-in-transaction backend older than this is flagged
	longXactCritSec     = 600.0   // any open transaction older than this is flagged (10m)
	replayLagCritSec    = 30.0    // standby replay lag past this is flagged
	wraparoundCritPct   = 90.0    // XID age this close to the freeze threshold is flagged
	slotRetainCritBytes = 1 << 30 // an inactive slot retaining >1 GB of WAL is flagged
	checkpointForcedPct = 50.0    // forced/total checkpoints above this suggests small max_wal_size
	checkpointMinSample = 10      // need at least this many checkpoints before judging the ratio
	cacheHitWarnPct     = 90.0    // buffer cache hit ratio below this is worth a tuning note
	cacheMinSample      = 10000   // need at least this many block accesses before judging the ratio
)

func symbol(sev string) string {
	switch sev {
	case sevCrit:
		return "⚠"
	case sevOK:
		return "✓"
	default:
		return "○"
	}
}

// A check's scope: whether its numbers describe the whole server or just the connected
// database. Surfacing it stops the reader from having to know which catalogs are global
// (pg_stat_activity, WAL, replication) and which are per-database (pg_stat_database,
// pg_class).
const (
	scopeCluster  = "cluster"  // spans/affects the whole server (all databases)
	scopeDatabase = "database" // describes only the connected database
)

// statusCheck is one line of the rollup. Detail holds the offending rows shown under
// the line in --verbose mode (the PIDs, tables, standbys behind the count); it stays
// empty otherwise, so the default screen and its JSON shape are unchanged.
type statusCheck struct {
	Name     string   `json:"name"`
	Scope    string   `json:"scope,omitempty"` // cluster | database
	Severity string   `json:"severity"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"`   // the command to drill into
	Detail   []string `json:"detail,omitempty"` // verbose-only: the rows behind the count
}

// checkScope maps each check (by Name) to whether it reports on the whole cluster or only
// the connected database. Keyed by Name so it also covers checks that degraded to a
// failCheck line. Wraparound is database-scoped here because the XID age it shows is read
// from this database's pg_class — though the wraparound hazard itself shuts down the whole
// cluster, so other databases warrant the same check.
var checkScope = map[string]string{
	"Connections":  scopeCluster,
	"Locks":        scopeCluster,
	"Transactions": scopeCluster,
	"Replication":  scopeCluster,
	"Slots":        scopeCluster,
	"WAL":          scopeCluster,
	"Checkpoints":  scopeCluster,
	"Cache":        scopeDatabase,
	"Temp files":   scopeDatabase,
	"Bloat":        scopeDatabase,
	"Wraparound":   scopeDatabase,
}

// errStatUnavailable degrades the cache/temp checks to an informational line when the
// single pg_stat_database round trip failed, rather than aborting the whole rollup.
var errStatUnavailable = errors.New("database statistics unavailable")

// detailRows caps how many offending rows --verbose prints per check — enough to act on,
// not so many that status stops being one screen. Drill into the Hint command for the rest.
const detailRows = 8

// statusSignals captures the few cross-check facts the final "start here" pointer
// needs, so it can correlate (e.g. the lock root blocker that's ALSO the oldest xact).
type statusSignals struct {
	connCrit        bool
	blockedCount    int
	rootBlockers    []int32
	oldestXactPID   int32
	oldestXactSec   float64
	oldestXactState string
	txCrit          bool
	wraparoundCrit  bool
	wraparoundRel   string
}

type statusReport struct {
	Database   string         `json:"database"`
	Host       string         `json:"host"`
	Port       uint16         `json:"port"`
	Version    string         `json:"server_version,omitempty"`
	SizeBytes  int64          `json:"size_bytes,omitempty"`
	ServerTime string         `json:"server_time,omitempty"`
	Summary    map[string]int `json:"summary"`
	Checks     []statusCheck  `json:"checks"`
	StartHere  string         `json:"start_here,omitempty"`
}

func newStatusCmd() *cobra.Command {
	var verbose bool
	var watch time.Duration
	c := &cobra.Command{
		Use:   "status",
		Short: "One-screen triage: is anything wrong right now?",
		Long: "status answers the first question in an incident — 'where do I even look?' — with a\n" +
			"single read-only snapshot. It checks connection saturation, blocked locks, the oldest\n" +
			"open transaction, replication lag, pg_wal volume, XID-wraparound risk, buffer cache\n" +
			"hit ratio, temp-file spill, and top bloat, marks each as\n" +
			"⚠ (attention) / ✓ (healthy) / ○ (informational), and points at the command to drill\n" +
			"into. A calm, mostly-✓ screen is itself the answer. Composes existing checks; adds no\n" +
			"new load beyond a few quick catalog queries.\n\n" +
			"Checks are split into two sections — cluster-wide (connections, locks, WAL, replication,\n" +
			"checkpoints: the whole server) and the connected database (cache, temp files, bloat,\n" +
			"wraparound) — so it's clear which scope each number describes.\n\n" +
			"-v/--verbose expands each line into the rows behind it (the blocked PIDs, the lagging\n" +
			"standbys, the top bloated tables) so you can act without a second command.\n" +
			"--watch re-renders the snapshot on an interval (default 2s; --watch=5s to change it),\n" +
			"like `watch pgdx status` — press Ctrl-C to stop.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runStatus(cmd, verbose, watch)
		},
	}
	c.Flags().BoolVarP(&verbose, "verbose", "v", false,
		"expand each check into the offending rows (PIDs, tables, standbys) behind the count")
	c.Flags().DurationVar(&watch, "watch", 0,
		"re-render on a repeating `interval` (bare --watch = 2s; e.g. --watch=5s); Ctrl-C to stop")
	// Allow a bare --watch (no value) to mean the default interval.
	c.Flags().Lookup("watch").NoOptDefVal = "2s"
	return c
}

func runStatus(cmd *cobra.Command, verbose bool, watch time.Duration) error {
	format, conn, release, err := connectForGet(cmd)
	if err != nil {
		return err
	}
	ctx := context.Background()
	defer release()

	// A privilege note (not a check): without pg_monitor, other users' detail is masked,
	// but the counts these checks rely on still hold. Print once, even when watching.
	if ok, perr := catalog.HasMonitorPrivilege(ctx, conn); perr == nil && !ok {
		fmt.Fprintln(cmd.ErrOrStderr(),
			"note: limited privilege — without pg_monitor some sessions' detail is hidden; counts are still accurate.")
	}

	if watch <= 0 {
		return emitStatus(ctx, cmd, conn, format, verbose)
	}
	return watchStatus(ctx, cmd, conn, format, verbose, watch)
}

// buildReport runs every check against one connection and assembles the rollup. It adds
// no new data — each check composes an existing catalog query — and is reused verbatim by
// the single-shot path and each --watch tick.
func buildReport(ctx context.Context, conn *db.Conn, verbose bool) statusReport {
	now, _ := catalog.ServerTime(ctx, conn) // best-effort header stamp
	// One round trip for the header (size, version) and the cache/temp checks below; a
	// failure here just leaves those fields empty rather than aborting the rollup.
	cdb, _ := catalog.GatherCurrentDB(ctx, conn)

	var sig statusSignals
	checks := []statusCheck{
		checkConnections(ctx, conn, &sig, verbose),
		checkLocks(ctx, conn, &sig, verbose),
		checkTransactions(ctx, conn, &sig, verbose),
		checkReplication(ctx, conn, verbose),
		checkReplicationSlots(ctx, conn, verbose),
		checkWAL(ctx, conn),
		checkWraparound(ctx, conn, &sig, verbose),
		checkCheckpoints(ctx, conn),
		checkCache(cdb),
		checkTempFiles(cdb),
		checkBloat(ctx, conn, verbose),
	}

	for i := range checks {
		checks[i].Scope = checkScope[checks[i].Name]
	}

	report := statusReport{
		Database:   conn.Database(),
		Host:       conn.Host(),
		Port:       conn.Port(),
		ServerTime: now,
		Summary:    map[string]int{sevCrit: 0, sevOK: 0, sevInfo: 0},
		Checks:     checks,
		StartHere:  synthesizeStartHere(sig),
	}
	if cdb != nil {
		report.Version = shortVersion(cdb.Version)
		report.SizeBytes = cdb.SizeBytes
	}
	for _, c := range checks {
		report.Summary[c.Severity]++
	}
	return report
}

// emitStatus builds and renders one snapshot (JSON or the table screen).
func emitStatus(ctx context.Context, cmd *cobra.Command, conn *db.Conn, format render.Format, verbose bool) error {
	report := buildReport(ctx, conn, verbose)
	if format == render.FormatJSON {
		return render.Render(cmd.OutOrStdout(), format, report)
	}
	printStatus(cmd, report)
	return nil
}

// watchStatus re-renders the snapshot every interval until interrupted (Ctrl-C). The
// table view clears the screen each tick so it reads like a live dashboard; JSON streams
// one object per tick (newline-separated) so it stays pipeable. The connection is reused
// across ticks; a transient query error degrades to an informational line rather than
// aborting the loop.
func watchStatus(ctx context.Context, cmd *cobra.Command, conn *db.Conn, format render.Format, verbose bool, interval time.Duration) error {
	return watchLoop(cmd, interval, format == render.FormatJSON, func() error {
		return emitStatus(ctx, cmd, conn, format, verbose)
	})
}

// failCheck turns a query error into an informational line rather than failing the
// whole rollup — partial triage still beats none.
func failCheck(name string, err error) statusCheck {
	return statusCheck{Name: name, Severity: sevInfo, Message: "unavailable: " + err.Error()}
}

func checkConnections(ctx context.Context, conn *db.Conn, sig *statusSignals, verbose bool) statusCheck {
	used, max, err := catalog.ConnUsage(ctx, conn)
	if err != nil {
		return failCheck("Connections", err)
	}
	pct := 0.0
	if max > 0 {
		pct = 100 * float64(used) / float64(max)
	}
	c := statusCheck{Name: "Connections", Severity: sevOK, Hint: "pgdx get connections",
		Message: fmt.Sprintf("%d / %d (%.0f%% of max_connections)", used, max, pct)}
	if pct >= connSaturatedPct {
		c.Severity = sevCrit
		c.Message += " — approaching the limit; new connections may be refused"
		sig.connCrit = true
	}
	// Verbose: where the connections are going — a wall of idle-in-transaction is a very
	// different problem from genuine active load.
	if verbose {
		if stats, serr := catalog.ListConnections(ctx, conn, catalog.ConnFilter{}); serr == nil {
			byState := map[string]int64{}
			for _, s := range stats {
				byState[dashIfEmpty(s.State)] += s.Count
			}
			c.Detail = topCounts(byState)
		}
	}
	return c
}

func checkLocks(ctx context.Context, conn *db.Conn, sig *statusSignals, verbose bool) statusCheck {
	waits, err := catalog.ListLockWaits(ctx, conn)
	if err != nil {
		return failCheck("Locks", err)
	}
	if len(waits) == 0 {
		return statusCheck{Name: "Locks", Severity: sevOK, Message: "no sessions blocked", Hint: "pgdx get locks"}
	}
	roots := rootBlockers(waits)
	sig.blockedCount = len(waits)
	sig.rootBlockers = roots
	msg := fmt.Sprintf("%d session(s) blocked waiting on locks", len(waits))
	if len(roots) > 0 {
		msg += fmt.Sprintf(" (root blocker: %s)", pidList(roots))
	}
	c := statusCheck{Name: "Locks", Severity: sevCrit, Message: msg, Hint: "pgdx get locks"}
	if verbose {
		rows := make([]string, 0, len(waits))
		for _, w := range waits {
			rows = append(rows, fmt.Sprintf("pid %d waiting on %s (%s) — blocked by %s",
				w.PID, w.Object, dashIfEmpty(w.Mode), dashIfEmpty(w.BlockedBy)))
		}
		c.Detail = capRows(rows)
	}
	return c
}

func checkTransactions(ctx context.Context, conn *db.Conn, sig *statusSignals, verbose bool) statusCheck {
	txns, err := catalog.ListLongTransactions(ctx, conn, 0)
	if err != nil {
		return failCheck("Transactions", err)
	}
	idle, err := catalog.ListIdleInTx(ctx, conn)
	if err != nil {
		return failCheck("Transactions", err)
	}
	if len(txns) == 0 {
		return statusCheck{Name: "Transactions", Severity: sevOK,
			Message: "no open transactions holding back the xmin horizon", Hint: "pgdx get transaction-age"}
	}
	oldest := txns[0] // ListLongTransactions is oldest-first
	sig.oldestXactPID = oldest.PID
	sig.oldestXactSec = oldest.XactSec
	sig.oldestXactState = oldest.State

	msg := fmt.Sprintf("oldest open %s (pid %d, %s)", secondsHuman(oldest.XactSec), oldest.PID, dashIfEmpty(oldest.State))
	if len(idle) > 0 {
		msg += fmt.Sprintf("; %d idle-in-transaction", len(idle))
	}
	c := statusCheck{Name: "Transactions", Severity: sevOK, Message: msg, Hint: "pgdx get transaction-age"}
	// Crit when something is genuinely pinning cleanup: a lingering idle-in-tx, or any
	// very old open transaction.
	if (len(idle) > 0 && idle[0].IdleSec >= idleTxCritSec) || oldest.XactSec >= longXactCritSec {
		c.Severity = sevCrit
		c.Message += " — blocking VACUUM cluster-wide"
		sig.txCrit = true
	}
	if verbose {
		rows := make([]string, 0, len(txns))
		for _, t := range txns {
			rows = append(rows, fmt.Sprintf("pid %d open %s (%s) user=%s db=%s",
				t.PID, secondsHuman(t.XactSec), dashIfEmpty(t.State), dashIfEmpty(t.User), dashIfEmpty(t.Database)))
		}
		c.Detail = capRows(rows)
	}
	return c
}

func checkReplication(ctx context.Context, conn *db.Conn, verbose bool) statusCheck {
	reps, err := catalog.ListReplication(ctx, conn)
	if err != nil {
		return failCheck("Replication", err)
	}
	if len(reps) == 0 {
		return statusCheck{Name: "Replication", Severity: sevInfo,
			Message: "no standbys connected (not a primary, or none attached)", Hint: "pgdx get replication"}
	}
	var maxReplay float64
	for _, r := range reps {
		if r.ReplayLagSec > maxReplay {
			maxReplay = r.ReplayLagSec
		}
	}
	c := statusCheck{Name: "Replication", Severity: sevOK, Hint: "pgdx get replication",
		Message: fmt.Sprintf("%d standby(s), max replay lag %s", len(reps), secondsHuman(maxReplay))}
	if maxReplay >= replayLagCritSec {
		c.Severity = sevCrit
		c.Message += " — a standby is falling behind"
	}
	if verbose {
		rows := make([]string, 0, len(reps))
		for _, r := range reps {
			where := dashIfEmpty(r.Application)
			if r.ClientAddr != "" {
				where += " @ " + r.ClientAddr
			}
			rows = append(rows, fmt.Sprintf("%s — replay lag %s, %s behind (%s)",
				where, secondsHuman(r.ReplayLagSec), humanBytes(r.LagBytes), dashIfEmpty(r.SyncState)))
		}
		c.Detail = capRows(rows)
	}
	return c
}

// checkReplicationSlots flags the silent disk-filler: a slot that pins WAL. An inactive
// slot retaining significant WAL (its consumer is gone) or a slot whose wal_status has
// slipped to 'unreserved'/'lost' is the warning; otherwise it's quiet.
func checkReplicationSlots(ctx context.Context, conn *db.Conn, verbose bool) statusCheck {
	slots, err := catalog.ListReplicationSlots(ctx, conn)
	if err != nil {
		return failCheck("Slots", err)
	}
	if len(slots) == 0 {
		return statusCheck{Name: "Slots", Severity: sevOK, Message: "no replication slots", Hint: "pgdx get replication --slots"}
	}
	var inactive int
	var worstRetain int64
	var worstName string
	var statusRisk bool
	for _, s := range slots {
		if s.WalStatus == "unreserved" || s.WalStatus == "lost" {
			statusRisk = true
		}
		if !s.Active {
			inactive++
			if s.RetainedBytes > worstRetain {
				worstRetain, worstName = s.RetainedBytes, s.Name
			}
		}
	}
	c := statusCheck{Name: "Slots", Severity: sevOK, Hint: "pgdx get replication --slots",
		Message: fmt.Sprintf("%d slot(s), %d inactive", len(slots), inactive)}
	if statusRisk || (inactive > 0 && worstRetain >= slotRetainCritBytes) {
		c.Severity = sevCrit
		c.Message = fmt.Sprintf("%d slot(s), %d inactive — slot %q retains %s of WAL (pins it from removal; can fill the disk)",
			len(slots), inactive, worstName, humanBytes(worstRetain))
	}
	if verbose {
		rows := make([]string, 0, len(slots))
		for _, s := range slots {
			rows = append(rows, fmt.Sprintf("%s (%s) active=%s retains %s%s",
				s.Name, s.Type, yesNo(s.Active), humanBytes(s.RetainedBytes), slotStatusSuffix(s.WalStatus)))
		}
		c.Detail = capRows(rows)
	}
	return c
}

// checkWAL reports the size of the pg_wal directory and its segment count — the disk the
// write-ahead log occupies. It's a neutral readout (○): pg_wal legitimately holds up to
// roughly max_wal_size plus whatever archiving or a replication slot is retaining (the
// Slots check flags the latter). Reading pg_ls_waldir() needs pg_monitor or superuser, so
// it degrades to an informational note rather than failing when the role can't.
func checkWAL(ctx context.Context, conn *db.Conn) statusCheck {
	info, err := catalog.WALUsage(ctx, conn)
	if err != nil {
		return statusCheck{Name: "WAL", Severity: sevInfo, Hint: "pgdx get settings max_wal_size",
			Message: "size unavailable — reading pg_wal needs pg_monitor or superuser"}
	}
	return statusCheck{Name: "WAL", Severity: sevInfo, Hint: "pgdx get settings max_wal_size",
		Message: formatWAL(info)}
}

// formatWAL renders the WAL line: the footprint, then its governors in parentheses so the
// number reads against the limits that set it (e.g. "2.2 GB ... (max_wal_size 6.0 GB,
// wal_keep_size 2.0 GB)"). wal_keep_size is shown only when set.
func formatWAL(info *catalog.WALInfo) string {
	msg := fmt.Sprintf("%s across %s segment(s) in pg_wal", humanBytes(info.Bytes), withThousands(info.Segments))
	var refs []string
	if info.MaxWALBytes > 0 {
		refs = append(refs, "max_wal_size "+humanBytes(info.MaxWALBytes))
	}
	if info.WALKeepBytes > 0 {
		refs = append(refs, "wal_keep_size "+humanBytes(info.WALKeepBytes))
	}
	if len(refs) > 0 {
		msg += " (" + strings.Join(refs, ", ") + ")"
	}
	return msg
}

func slotStatusSuffix(walStatus string) string {
	if walStatus == "" || walStatus == "reserved" {
		return ""
	}
	return " [wal_status: " + walStatus + "]"
}

// checkCheckpoints flags checkpoints being forced by WAL volume rather than the timer —
// the usual sign max_wal_size is too small. It's tuning guidance, not an incident, so a
// high forced ratio is informational (○), never critical.
func checkCheckpoints(ctx context.Context, conn *db.Conn) statusCheck {
	cs, err := catalog.CheckpointActivity(ctx, conn)
	if err != nil {
		return failCheck("Checkpoints", err)
	}
	total := cs.Timed + cs.Requested
	c := statusCheck{Name: "Checkpoints", Severity: sevOK, Hint: "pgdx get settings max_wal_size",
		Message: fmt.Sprintf("%s timed / %s requested since stats reset", withThousands(cs.Timed), withThousands(cs.Requested))}
	if total < checkpointMinSample {
		return c // too few to draw any conclusion
	}
	forcedPct := 100 * float64(cs.Requested) / float64(total)
	if forcedPct >= checkpointForcedPct {
		c.Severity = sevInfo
		c.Message += fmt.Sprintf(" — %.0f%% forced; consider raising max_wal_size", forcedPct)
	}
	return c
}

// checkCache reports the database's shared-buffer cache hit ratio. Like checkpoints it's
// a cumulative-since-reset tuning signal, not a same-minute incident, so a low ratio is
// informational (○), never critical. Quiet until enough I/O has happened to be meaningful.
func checkCache(cdb *catalog.CurrentDB) statusCheck {
	if cdb == nil {
		return failCheck("Cache", errStatUnavailable)
	}
	c := statusCheck{Name: "Cache", Severity: sevOK, Hint: "pgdx get settings shared_buffers"}
	total := cdb.BlksHit + cdb.BlksRead
	if total < cacheMinSample {
		c.Severity = sevInfo
		c.Message = "too little I/O recorded to judge the buffer cache hit ratio"
		return c
	}
	pct := 100 * float64(cdb.BlksHit) / float64(total)
	c.Message = fmt.Sprintf("%.1f%% buffer cache hit ratio since stats reset", pct)
	if pct < cacheHitWarnPct {
		c.Severity = sevInfo
		c.Message += " — low; the working set may not fit in shared_buffers"
	}
	return c
}

// checkTempFiles flags queries spilling to temp files — sorts or hashes that exceeded
// work_mem. Cumulative since stats reset, so it's a tuning signal (○), never critical.
func checkTempFiles(cdb *catalog.CurrentDB) statusCheck {
	if cdb == nil {
		return failCheck("Temp files", errStatUnavailable)
	}
	c := statusCheck{Name: "Temp files", Severity: sevOK, Hint: "pgdx get settings work_mem",
		Message: "no queries have spilled to temp files since stats reset"}
	if cdb.TempBytes > 0 {
		c.Severity = sevInfo
		c.Message = fmt.Sprintf("%s spilled to temp files since stats reset — sorts/hashes exceeding work_mem",
			humanBytes(cdb.TempBytes))
	}
	return c
}

// shortVersion trims the server_version GUC ("16.2 (Ubuntu 16.2-1.pgdg…)") to its leading
// version token for the header.
func shortVersion(v string) string {
	if i := strings.IndexByte(v, ' '); i > 0 {
		return v[:i]
	}
	return v
}

func checkWraparound(ctx context.Context, conn *db.Conn, sig *statusSignals, verbose bool) statusCheck {
	// Fetch one row for the headline; a few more when expanding so the detail is useful.
	limit := 1
	if verbose {
		limit = detailRows
	}
	risks, err := catalog.ListWraparoundRisk(ctx, conn, "", limit)
	if err != nil {
		return failCheck("Wraparound", err)
	}
	if len(risks) == 0 {
		return statusCheck{Name: "Wraparound", Severity: sevOK, Message: "no relations with measurable XID age", Hint: "pgdx get vacuum-health"}
	}
	top := risks[0]
	c := statusCheck{Name: "Wraparound", Severity: sevOK, Hint: "pgdx get vacuum-health",
		Message: fmt.Sprintf("max XID age %.0f%% of freeze threshold (%s.%s)", top.PctToFreeze, top.Schema, top.Name)}
	if top.PctToFreeze >= wraparoundCritPct {
		c.Severity = sevCrit
		c.Message += " — a forced anti-wraparound vacuum is due or overdue"
		sig.wraparoundCrit = true
		sig.wraparoundRel = top.Schema + "." + top.Name
	}
	if verbose {
		rows := make([]string, 0, len(risks))
		for _, r := range risks {
			line := fmt.Sprintf("%s.%s — %.0f%% of threshold (XID age %d, %s)",
				r.Schema, r.Name, r.PctToFreeze, r.XIDAge, r.Size)
			if r.Owner != "" {
				line += " — TOAST of " + r.Owner
			}
			rows = append(rows, line)
		}
		c.Detail = rows
	}
	return c
}

func checkBloat(ctx context.Context, conn *db.Conn, verbose bool) statusCheck {
	limit := 1
	if verbose {
		limit = detailRows
	}
	rows, err := catalog.ListTableBloat(ctx, conn, "", limit)
	if err != nil {
		return failCheck("Bloat", err)
	}
	if len(rows) == 0 {
		return statusCheck{Name: "Bloat", Severity: sevOK, Message: "no significant table bloat", Hint: "pgdx get bloat"}
	}
	top := rows[0]
	// Informational: bloat is a maintenance backlog, rarely a same-minute emergency.
	c := statusCheck{Name: "Bloat", Severity: sevInfo, Hint: "pgdx get bloat",
		Message: fmt.Sprintf("top: %s.%s ~%s reclaimable", top.Schema, top.Name, humanBytes(top.WasteBytes))}
	if verbose {
		detail := make([]string, 0, len(rows))
		for _, r := range rows {
			detail = append(detail, fmt.Sprintf("%s.%s ~%s reclaimable (DEAD %.0f%%)",
				r.Schema, r.Name, humanBytes(r.WasteBytes), r.DeadRatio*100))
		}
		c.Detail = detail
	}
	return c
}

// synthesizeStartHere produces the single most useful next action, correlating signals
// where it can (a lock root blocker that is also the oldest transaction is the textbook
// idle-in-transaction incident). Priority: locks → connections → transactions →
// wraparound. Empty-ish when nothing is urgent.
func synthesizeStartHere(s statusSignals) string {
	switch {
	case s.blockedCount > 0 && len(s.rootBlockers) > 0:
		msg := fmt.Sprintf("Start with the lock root blocker %s.", pidList(s.rootBlockers))
		for _, p := range s.rootBlockers {
			if p == s.oldestXactPID && s.oldestXactPID != 0 {
				msg += fmt.Sprintf(" pid %d is also the oldest transaction (%s, %s) — inspect, then `cancel`/`kill` it.",
					p, secondsHuman(s.oldestXactSec), dashIfEmpty(s.oldestXactState))
				return msg
			}
		}
		return msg + " Inspect it (`pgdx get activity`), then `cancel`/`kill` if appropriate."
	case s.connCrit:
		return "Connections are near the cap — check `pgdx get connections` for a leak or idle-in-transaction backends."
	case s.txCrit:
		return fmt.Sprintf("Investigate pid %d — the oldest transaction (%s, %s) is holding back VACUUM across the database.",
			s.oldestXactPID, secondsHuman(s.oldestXactSec), dashIfEmpty(s.oldestXactState))
	case s.wraparoundCrit:
		return fmt.Sprintf("XID-wraparound risk on %s — make sure autovacuum is keeping up (see `pgdx get vacuum-health` and `get transaction-age`).", s.wraparoundRel)
	default:
		return "Nothing urgent — the database looks healthy."
	}
}

// rootBlockers returns the blocking PIDs that are not themselves waiting — the PIDs at
// the head of a lock chain, where an incident actually starts.
func rootBlockers(waits []catalog.LockWait) []int32 {
	blocked := make(map[int32]bool, len(waits))
	for _, w := range waits {
		blocked[w.PID] = true
	}
	seen := map[int32]bool{}
	var roots []int32
	for _, w := range waits {
		for _, ps := range strings.Split(w.BlockedBy, ",") {
			ps = strings.TrimSpace(ps)
			if ps == "" {
				continue
			}
			pid64, err := strconv.ParseInt(ps, 10, 32)
			if err != nil {
				continue
			}
			pid := int32(pid64)
			if !blocked[pid] && !seen[pid] {
				seen[pid] = true
				roots = append(roots, pid)
			}
		}
	}
	sort.Slice(roots, func(i, j int) bool { return roots[i] < roots[j] })
	return roots
}

// capRows trims verbose detail to detailRows lines, appending a "+N more" pointer when
// it overflows — keeping status one screen while making clear nothing was silently hidden.
func capRows(rows []string) []string {
	if len(rows) <= detailRows {
		return rows
	}
	out := append([]string(nil), rows[:detailRows]...)
	out = append(out, fmt.Sprintf("… +%d more (see the drill-down command)", len(rows)-detailRows))
	return out
}

// topCounts renders a count-by-key map as "key N" lines, highest count first (ties broken
// by key for stable output). Used for the connection state breakdown.
func topCounts(counts map[string]int64) []string {
	type kv struct {
		k string
		n int64
	}
	pairs := make([]kv, 0, len(counts))
	for k, n := range counts {
		pairs = append(pairs, kv{k, n})
	}
	sort.Slice(pairs, func(i, j int) bool {
		if pairs[i].n != pairs[j].n {
			return pairs[i].n > pairs[j].n
		}
		return pairs[i].k < pairs[j].k
	})
	rows := make([]string, 0, len(pairs))
	for _, p := range pairs {
		rows = append(rows, fmt.Sprintf("%s: %d", p.k, p.n))
	}
	return capRows(rows)
}

func pidList(pids []int32) string {
	parts := make([]string, len(pids))
	for i, p := range pids {
		parts[i] = "pid " + strconv.FormatInt(int64(p), 10)
	}
	return strings.Join(parts, ", ")
}

func printStatus(cmd *cobra.Command, r statusReport) {
	out := cmd.OutOrStdout()
	host := r.Host
	if host == "" {
		host = "local"
	}
	fmt.Fprintf(out, "Database %q @ %s:%d", r.Database, host, r.Port)
	if r.Version != "" {
		fmt.Fprintf(out, " — PostgreSQL %s", r.Version)
	}
	if r.SizeBytes > 0 {
		fmt.Fprintf(out, ", %s", humanBytes(r.SizeBytes))
	}
	if r.ServerTime != "" {
		fmt.Fprintf(out, " — %s", r.ServerTime)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out)

	// Align the labels by their widest rune-count, across all checks so both sections line up.
	width := 0
	for _, c := range r.Checks {
		if n := utf8.RuneCountInString(c.Name); n > width {
			width = n
		}
	}

	// Render in two scoped sections so the reader can see at a glance which numbers describe
	// the whole server and which describe only the connected database. A check whose scope
	// is unset falls through to a final unsectioned pass (keeps older/synthetic reports
	// rendering sensibly).
	sections := []struct{ scope, heading string }{
		{scopeCluster, "Cluster-wide (the whole server)"},
		{scopeDatabase, fmt.Sprintf("This database (%q)", r.Database)},
	}
	printed := make([]bool, len(r.Checks))
	wrote := false
	for _, s := range sections {
		var idxs []int
		for i, c := range r.Checks {
			if c.Scope == s.scope {
				idxs = append(idxs, i)
			}
		}
		if len(idxs) == 0 {
			continue
		}
		if wrote {
			fmt.Fprintln(out)
		}
		fmt.Fprintf(out, "%s\n", s.heading)
		for _, i := range idxs {
			printCheckLine(out, r.Checks[i], width, "  ")
			printed[i] = true
		}
		wrote = true
	}
	for i, c := range r.Checks {
		if !printed[i] {
			printCheckLine(out, c, width, "")
		}
	}

	if r.StartHere != "" {
		fmt.Fprintf(out, "\n→ %s\n", r.StartHere)
	}
}

// printCheckLine renders one check (and its verbose detail rows) with the given left
// indent, aligning the message column at width and hanging detail rows beneath it.
func printCheckLine(out io.Writer, c statusCheck, width int, gutter string) {
	pad := strings.Repeat(" ", width-utf8.RuneCountInString(c.Name))
	fmt.Fprintf(out, "%s%s  %s%s   %s\n", gutter, symbol(c.Severity), c.Name, pad, c.Message)
	detailIndent := gutter + strings.Repeat(" ", width+6) // "X  " (3) + name+pad (width) + "   " (3)
	for _, d := range c.Detail {
		fmt.Fprintf(out, "%s%s\n", detailIndent, d)
	}
}
