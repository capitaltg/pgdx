package cmd

import (
	"context"
	"fmt"
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

// statusCheck is one line of the rollup. Detail holds the offending rows shown under
// the line in --verbose mode (the PIDs, tables, standbys behind the count); it stays
// empty otherwise, so the default screen and its JSON shape are unchanged.
type statusCheck struct {
	Name     string   `json:"name"`
	Severity string   `json:"severity"`
	Message  string   `json:"message"`
	Hint     string   `json:"hint,omitempty"`   // the command to drill into
	Detail   []string `json:"detail,omitempty"` // verbose-only: the rows behind the count
}

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
			"open transaction, replication lag, XID-wraparound risk, and top bloat, marks each as\n" +
			"⚠ (attention) / ✓ (healthy) / ○ (informational), and points at the command to drill\n" +
			"into. A calm, mostly-✓ screen is itself the answer. Composes existing checks; adds no\n" +
			"new load beyond a few quick catalog queries.\n\n" +
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

	var sig statusSignals
	checks := []statusCheck{
		checkConnections(ctx, conn, &sig, verbose),
		checkLocks(ctx, conn, &sig, verbose),
		checkTransactions(ctx, conn, &sig, verbose),
		checkReplication(ctx, conn, verbose),
		checkReplicationSlots(ctx, conn, verbose),
		checkWraparound(ctx, conn, &sig, verbose),
		checkCheckpoints(ctx, conn),
		checkBloat(ctx, conn, verbose),
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
	if r.ServerTime != "" {
		fmt.Fprintf(out, " — %s", r.ServerTime)
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out)

	// Align the labels by their widest rune-count.
	width := 0
	for _, c := range r.Checks {
		if n := utf8.RuneCountInString(c.Name); n > width {
			width = n
		}
	}
	// Detail rows (verbose) hang under their check, indented to line up with the message.
	indent := strings.Repeat(" ", width+6) // "X  " (3) + name+pad (width) + "   " (3)
	for _, c := range r.Checks {
		pad := strings.Repeat(" ", width-utf8.RuneCountInString(c.Name))
		fmt.Fprintf(out, "%s  %s%s   %s\n", symbol(c.Severity), c.Name, pad, c.Message)
		for _, d := range c.Detail {
			fmt.Fprintf(out, "%s%s\n", indent, d)
		}
	}
	if r.StartHere != "" {
		fmt.Fprintf(out, "\n→ %s\n", r.StartHere)
	}
}
