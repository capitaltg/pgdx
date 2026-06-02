package cmd

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/db"
	"github.com/capitaltg/pgdx/internal/explain"
	"github.com/capitaltg/pgdx/internal/render"
)

var (
	flagAnalyze    bool
	flagSuggestIdx bool
	flagExplainPID int
	flagPlanFile   string
	flagParams     []string
	flagExplainV   int
)

func newExplainCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "explain <query>",
		Short: "Diagnose a query plan in plain language",
		Long: "explain runs EXPLAIN on a query and tells you what's expensive.\n\n" +
			"By default it does NOT execute your query — it only describes the plan.\n" +
			"Pass --analyze to execute and collect real timings. Writes (and SELECTs that\n" +
			"hide a data-modifying CTE) run inside BEGIN..ROLLBACK; statements with side\n" +
			"effects a rollback can't undo (NOTIFY, nextval, COPY..PROGRAM, dblink) are refused.\n\n" +
			"Instead of a query argument, pass --pid <pid> (a PID from `get activity`) to diagnose\n" +
			"the query that backend is running right now. Parameterized/prepared statements (with\n" +
			"$1, $2, …) can't be planned without values, so pgdx uses EXPLAIN (GENERIC_PLAN) for\n" +
			"them — estimates only, and requires PostgreSQL 16+.\n\n" +
			"For a parameterized query you paste yourself (e.g. straight from `get slow-queries`),\n" +
			"supply values with --param/-p, once per placeholder in order: pgdx binds them to\n" +
			"$1, $2, … so the planner sees REAL values (real selectivity, and real timings with\n" +
			"--analyze) — the accurate alternative to GENERIC_PLAN. Values are bound through the\n" +
			"protocol (never spliced into the SQL) and Postgres infers each type from context, so\n" +
			"`LIMIT $7` takes a number and `col ILIKE $1` takes text, no casting needed.\n\n" +
			"--suggest-index turns a 'sequential scan filters out most rows' finding into a\n" +
			"candidate CREATE INDEX CONCURRENTLY statement (needs --analyze, which provides the\n" +
			"rows-removed evidence). It's a STARTING POINT, not a guarantee — verify against your\n" +
			"workload and existing indexes before creating it.\n\n" +
			"--plan diagnoses an EXISTING plan offline: pass a file containing EXPLAIN (FORMAT\n" +
			"JSON) output (or '-' to read stdin), and pgdx applies the same diagnosis without\n" +
			"connecting to any database. Handy for a plan pasted from a slow-query log, auto_explain,\n" +
			"or a colleague — `psql -c 'EXPLAIN (FORMAT JSON) ...' | pgdx explain --plan -`.\n\n" +
			"-v/--verbose is tiered (repeat the flag): -v prints the plan tree beneath the\n" +
			"diagnosis with the flagged node marked (← flagged); -vv adds a plain-language\n" +
			"explanation of the plan (what the cost means, per-worker rows, two-phase parallel\n" +
			"aggregation); -vvv adds the live catalog stats (table size, reltuples, cost settings)\n" +
			"and an approximate cost breakdown for the flagged node. -v and -vv are rendered from\n" +
			"the captured plan, so they never re-run your query (safe with --analyze) and work\n" +
			"offline with --plan; -vvv needs a live connection for catalog stats, so it's not\n" +
			"available with --plan. Most useful when the verdict is 'no obvious problem' but the\n" +
			"query is still slow.",
		Args: cobra.MaximumNArgs(1),
		RunE: runExplain,
	}
	c.Flags().BoolVar(&flagAnalyze, "analyze", false,
		"EXECUTE the query to collect real timings (off by default; guarded for writes)")
	c.Flags().BoolVar(&flagSuggestIdx, "suggest-index", false,
		"emit a candidate CREATE INDEX for a missing-index finding (a starting point; verify before use)")
	c.Flags().IntVar(&flagExplainPID, "pid", 0,
		"explain the query running on this backend PID (from `get activity`) instead of a query argument")
	c.Flags().StringVar(&flagPlanFile, "plan", "",
		"diagnose an existing EXPLAIN (FORMAT JSON) plan from a file (or '-' for stdin), no DB connection")
	c.Flags().StringArrayVarP(&flagParams, "param", "p", nil,
		"value for a $N placeholder, in order (repeatable); lets you explain a parameterized query verbatim")
	c.Flags().CountVarP(&flagExplainV, "verbose", "v",
		"-v: print the plan tree (flagged node marked); -vv: add a plain-language explanation; "+
			"-vvv: add the live catalog stats + cost breakdown behind the flagged node")
	return c
}

func runExplain(cmd *cobra.Command, args []string) error {
	out := cmd.OutOrStdout() // DATA only (D4)
	errOut := cmd.ErrOrStderr()

	format, err := render.ParseFormat(flagOutput)
	if err != nil {
		return usageError{err.Error()}
	}

	// --plan diagnoses an existing plan offline — no query, no PID, no connection.
	if flagPlanFile != "" {
		if len(args) > 0 || flagExplainPID != 0 {
			return usageError{"--plan diagnoses a saved plan; don't also pass a query or --pid"}
		}
		if flagAnalyze {
			return usageError{"--analyze executes a query; it has no meaning with --plan (the plan is already captured)"}
		}
		if flagExplainV >= 3 {
			return usageError{"-vvv needs a live connection for catalog stats (table size, cost settings); --plan is offline — use -vv for the plan explanation"}
		}
		return runExplainPlanFile(cmd, format)
	}

	// Exactly one source of the query: a positional arg OR --pid.
	if flagExplainPID != 0 && len(args) > 0 {
		return usageError{"pass either a query or --pid, not both"}
	}
	if flagExplainPID == 0 && len(args) == 0 {
		return usageError{"provide a query to explain, or --pid <pid> to explain a running backend's query"}
	}
	if flagExplainPID < 0 {
		return usageError{fmt.Sprintf("--pid must be a positive integer, got %d", flagExplainPID)}
	}
	// --param applies to a pasted query; a running backend (--pid) already has its values bound.
	if len(flagParams) > 0 && flagExplainPID != 0 {
		return usageError{"--param applies to a query argument; a --pid backend's values are already bound"}
	}

	// Apply pgdx's default context if nothing more specific was given (--dsn / $PGSERVICE).
	noteContext(cmd)

	ctx := context.Background()
	conn, release, err := dial(ctx, cmd, flagDatabase)
	if err != nil {
		return err
	}
	// Closure (not `defer release()`) so a --pid reconnect releases the FINAL conn;
	// nil-guarded so a failed reconnect can't panic on release. In a shell session the
	// initial release is a no-op (the connection is shared) — only a cross-database
	// reconnect produces a real connection to close here.
	defer func() {
		if release != nil {
			release()
		}
	}()

	// With --pid we fetch the backend's query from the server first; otherwise the
	// query is the argument. Both then flow through the same guard + diagnosis.
	var query string
	var decision explain.Decision
	if flagExplainPID != 0 {
		info, q, lerr := backendQuery(cmd, ctx, conn, int32(flagExplainPID))
		if lerr != nil {
			return lerr
		}
		query = q
		// EXPLAIN runs in pgdx's connected database; the backend's query references
		// objects in ITS database. If they differ, reconnect there (needs CONNECT) so we
		// don't fail with a misleading "relation does not exist".
		if info.Database != "" && info.Database != conn.Database() {
			fmt.Fprintf(errOut, "note: backend %d is on database %q; connecting there to explain its query.\n",
				flagExplainPID, info.Database)
			// Open the new connection BEFORE releasing the old one, so a failure (e.g. no
			// CONNECT on an internal database like rdsadmin) leaves `conn` valid for the
			// deferred release — and never nil. info.Database differs from conn.Database()
			// here, so dial always returns a fresh connection, even in a shell session;
			// releasing the old one is a no-op there (it is the shared session conn).
			newConn, newRelease, cerr := dial(ctx, cmd, info.Database)
			if cerr != nil {
				return fmt.Errorf("can't connect to %q (the backend's database) to explain there — you may lack CONNECT on it: %w", info.Database, cerr)
			}
			release()
			conn, release = newConn, newRelease
		}
		decision, err = explainDecisionForPID(cmd, ctx, conn, int32(flagExplainPID), query)
		if err != nil {
			return err
		}
	} else {
		query = args[0]
		decision = explain.Decide(query, flagAnalyze)
	}

	if decision.Action == explain.ActionRefuse {
		// Safety/usability refusal — runtime error, nothing executed.
		return fmt.Errorf("refused: %s", decision.Reason)
	}
	if decision.Warning != "" {
		fmt.Fprintf(errOut, "warning: %s\n", decision.Warning)
	}

	// A pasted parameterized query needs values: bind them (--param) so the planner sees
	// real values, rather than failing on an unbindable $N.
	if flagExplainPID == 0 {
		want := explain.MaxParamIndex(query)
		switch {
		case want > 0 && len(flagParams) == 0:
			return usageError{fmt.Sprintf(
				"query has %d placeholder(s) ($1…$%d) but no values — pass them with --param/-p (once each, in order), e.g. -p 'mercy%%' -p 20",
				want, want)}
		case len(flagParams) > 0 && want == 0:
			return usageError{"--param given but the query has no $N placeholders"}
		case len(flagParams) > 0 && len(flagParams) != want:
			return usageError{fmt.Sprintf("query references $1…$%d (%d values) but %d --param value(s) were given",
				want, want, len(flagParams))}
		}
	}

	stmts := explain.BuildStatements(decision, query)
	var raw []byte
	if len(flagParams) > 0 {
		raw, err = conn.RunExplainWithParams(ctx, stmts, flagParams)
	} else {
		raw, err = conn.RunExplain(ctx, stmts)
	}
	if err != nil {
		// --analyze runs the query, so a slow one trips pgdx's statement_timeout
		// (SQLSTATE 57014). Point at --timeout unless the user already raised it.
		var pgErr *pgconn.PgError
		if flagAnalyze && flagTimeout == "" && errors.As(err, &pgErr) && pgErr.Code == "57014" {
			return fmt.Errorf("%w\nhint: --analyze executes the query and pgdx caps it at %s; raise the limit, e.g. `--timeout 2m`", err, db.DefaultStatementTimeout)
		}
		return err
	}

	parsed, err := explain.Parse(raw)
	if err != nil {
		return err
	}
	diag := diagnose(parsed)
	// -vvv grounds the cost in live catalog stats — only reachable here (the offline
	// --plan path rejects -vvv upstream). Failures degrade to -vv with a note, never error.
	var cost *costReport
	if flagExplainV >= 3 {
		cost = buildCostReport(ctx, conn, parsed, diag, errOut)
	}
	return renderDiagnosisParsed(out, format, parsed, diag, flagAnalyze, flagExplainV, cost)
}

// diagnose runs the pattern set and, when --suggest-index is set, attaches candidate DDL.
func diagnose(parsed *explain.ExplainOutput) explain.Diagnosis {
	d := explain.Diagnose(parsed)
	if flagSuggestIdx {
		explain.AddIndexSuggestions(&d)
	}
	return d
}

// runExplainPlanFile diagnoses an existing EXPLAIN (FORMAT JSON) plan read from a file
// (or stdin when the path is "-"), with no database connection. Whether the plan was
// captured with ANALYZE is inferred from the presence of execution timings in the JSON.
func runExplainPlanFile(cmd *cobra.Command, format render.Format) error {
	var raw []byte
	var err error
	if flagPlanFile == "-" {
		raw, err = io.ReadAll(cmd.InOrStdin())
	} else {
		raw, err = os.ReadFile(flagPlanFile)
	}
	if err != nil {
		return fmt.Errorf("read plan: %w", err)
	}
	if len(strings.TrimSpace(string(raw))) == 0 {
		return usageError{"the plan is empty — pass EXPLAIN (FORMAT JSON) output"}
	}
	parsed, err := explain.Parse(raw)
	if err != nil {
		return fmt.Errorf("parse plan (expected EXPLAIN (FORMAT JSON) output): %w", err)
	}
	// A plan carrying execution time was produced with ANALYZE; reflect that in the view.
	// -vvv is rejected upstream for --plan (offline), so there's never a cost report here.
	return renderDiagnosisParsed(cmd.OutOrStdout(), format, parsed, diagnose(parsed), parsed.ExecutionTime > 0, flagExplainV, nil)
}

// renderDiagnosisParsed renders an already-diagnosed plan in the requested format. Shared
// by the live (query/--pid) and offline (--plan) paths. verbose is the -v count (0–3) and
// cost is the live -vvv breakdown (nil unless verbose>=3 and the lookup succeeded).
func renderDiagnosisParsed(out io.Writer, format render.Format, parsed *explain.ExplainOutput, diag explain.Diagnosis, analyzed bool, verbose int, cost *costReport) error {
	view := newDiagnosisView(parsed, diag, analyzed)
	// -v: the plan tree as evidence (rendered from parsed JSON, no re-run, works offline).
	if verbose >= 1 {
		view.Plan = explain.PlanTree(parsed, diag, analyzed)
	}
	// -vv: a plain-language explanation of the plan (also purely derived).
	if verbose >= 2 {
		view.Explanation = explain.Explanation(parsed, diag, analyzed)
	}
	// -vvv: the live catalog stats + cost breakdown (assembled by the caller).
	if cost != nil {
		view.CostBreakdown = cost.json()
	}

	if format == render.FormatJSON {
		return render.Render(out, format, view)
	}

	// Human path: a readable stacked layout (prose findings wrap badly in a table).
	// Say "no obvious problem" out loud rather than inventing one (D7).
	if !diag.HasFindings() {
		fmt.Fprintln(out, "No obvious problem found.")
		if diag.Note != "" {
			fmt.Fprintf(out, "  Why: %s\n", wrapText(diag.Note, "  ", 86))
		}
	} else {
		for i, f := range diag.Findings {
			if i > 0 {
				fmt.Fprintln(out)
			}
			printFinding(out, f)
		}
	}
	if view.Plan != "" {
		fmt.Fprintf(out, "\nPlan:\n%s\n", view.Plan)
	}
	printExplanation(out, view.Explanation)
	if cost != nil {
		printCostReport(out, cost)
	}
	printTimings(out, parsed)
	return nil
}

// backendQuery fetches the query a backend is running (or last ran), validates it,
// and prints what it grabbed (warning when the backend is idle, so the query is its
// LAST statement, not a running one). It returns the backend info too, so the caller
// can reconnect to the backend's database before explaining.
func backendQuery(cmd *cobra.Command, ctx context.Context, conn *db.Conn, pid int32) (*catalog.Backend, string, error) {
	e := cmd.ErrOrStderr()
	info, found, err := catalog.BackendInfo(ctx, conn, pid)
	if err != nil {
		return nil, "", err
	}
	if !found {
		return nil, "", fmt.Errorf("no backend with PID %d (it may have already ended)", pid)
	}
	query := strings.TrimSpace(info.Query)
	if query == "" {
		return nil, "", fmt.Errorf(
			"backend %d has no visible query text — it may be idle with no last statement, or you may lack pg_monitor to see another user's query", pid)
	}

	// Show the FULL query we're about to explain (flattened to one line), not a
	// truncated preview — when you're diagnosing a query, you need to see all of it.
	// This is on stderr, so it never mixes with -o json on stdout.
	fmt.Fprintf(e, "explaining backend %d's query (user=%s db=%s state=%s):\n  %s\n",
		pid, dashIfEmpty(info.User), dashIfEmpty(info.Database), dashIfEmpty(info.State), flattenQuery(query))
	if !backendIsRunning(info.State) {
		fmt.Fprintf(e, "note: backend %d is %s — this is its LAST statement, not one running now.\n",
			pid, dashIfEmpty(info.State))
	}
	return info, query, nil
}

// explainDecisionForPID decides how to explain a backend's query. Parameterized
// statements ($1, …) can't be planned without values, so they go through EXPLAIN
// (GENERIC_PLAN) (PG16+); everything else flows through the normal safety guard.
func explainDecisionForPID(cmd *cobra.Command, ctx context.Context, conn *db.Conn, pid int32, query string) (explain.Decision, error) {
	e := cmd.ErrOrStderr()
	if explain.HasParameters(query) {
		// Prepared/parameterized statement: no values to bind, so plan it generically.
		if ver, verr := catalog.ServerVersionNum(ctx, conn); verr == nil && ver < 160000 {
			return explain.Decision{}, fmt.Errorf(
				"backend %d's query is parameterized ($1, …) and this server is older than PostgreSQL 16, which can't EXPLAIN it without values; substitute literals and pass the query to `pgdx explain` directly", pid)
		}
		if flagAnalyze {
			fmt.Fprintln(e, "note: --analyze can't run a parameterized query without values; using EXPLAIN (GENERIC_PLAN) (estimates only).")
		} else {
			fmt.Fprintln(e, "note: query is parameterized; using EXPLAIN (GENERIC_PLAN) — estimates only, since no values are bound.")
		}
		return explain.Decision{Category: explain.Classify(query), Action: explain.ActionExplainPlain, GenericPlan: true}, nil
	}
	return explain.Decide(query, flagAnalyze), nil
}

// printFinding renders one finding in a stacked layout with wrapped body text:
//
//	⚠ <title>
//	  <detail, wrapped>
//	  → <suggestion, wrapped>
func printFinding(w io.Writer, f explain.Finding) {
	const indent = "  "
	fmt.Fprintf(w, "⚠ %s\n", f.Title)
	if f.Detail != "" {
		fmt.Fprintf(w, "%s%s\n", indent, wrapText(f.Detail, indent, 86))
	}
	if f.Suggestion != "" {
		fmt.Fprintf(w, "%s→ %s\n", indent, wrapText(f.Suggestion, indent+"  ", 84))
	}
	if f.IndexSuggestion != "" {
		fmt.Fprintf(w, "%scandidate: %s\n", indent, f.IndexSuggestion)
		fmt.Fprintf(w, "%s%s\n", indent,
			wrapText("(a starting point — verify it helps and isn't redundant with an existing index before creating it)", indent+"  ", 84))
	}
}

// wrapText word-wraps s to width columns, prefixing continuation lines with indent.
// The first line is NOT prefixed (the caller positions it).
func wrapText(s, indent string, width int) string {
	words := strings.Fields(s)
	if len(words) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString(words[0])
	lineLen := len([]rune(words[0]))
	for _, word := range words[1:] {
		wl := len([]rune(word))
		if lineLen+1+wl > width {
			b.WriteString("\n")
			b.WriteString(indent)
			b.WriteString(word)
			lineLen = wl
		} else {
			b.WriteString(" ")
			b.WriteString(word)
			lineLen += 1 + wl
		}
	}
	return b.String()
}

func printTimings(w interface{ Write([]byte) (int, error) }, p *explain.ExplainOutput) {
	if p.PlanningTime > 0 {
		fmt.Fprintf(w, "\nplanning: %.2f ms", p.PlanningTime)
	}
	if p.ExecutionTime > 0 {
		fmt.Fprintf(w, "  execution: %.2f ms", p.ExecutionTime)
	}
	if p.PlanningTime > 0 || p.ExecutionTime > 0 {
		fmt.Fprintln(w)
	}
}

// diagnosisView adapts a diagnosis for the shared renderer (D2): Tabular for the
// table path, plain struct fields for JSON.
type diagnosisView struct {
	Findings      []explain.Finding  `json:"findings"`
	Note          string             `json:"note,omitempty"`
	Plan          string             `json:"plan,omitempty"`           // -v: the rendered plan tree
	Explanation   []string           `json:"explanation,omitempty"`    // -vv: plain-language notes
	CostBreakdown *costBreakdownJSON `json:"cost_breakdown,omitempty"` // -vvv: live catalog stats + cost math
	Analyzed      bool               `json:"analyzed"`
	PlanningTime  float64            `json:"planning_time_ms,omitempty"`
	ExecutionTime float64            `json:"execution_time_ms,omitempty"`
}

// costReport bundles the live catalog facts and the (optional) cost decomposition for the
// scan node `explain -vvv` profiles. It's assembled in the live path and rendered below.
type costReport struct {
	node     *explain.PlanNode
	stats    catalog.RelCostStats
	settings catalog.CostSettings
	workers  int
	cost     explain.SeqScanCost
	hasCost  bool // false => node type not decomposed; show facts only
}

// buildCostReport looks up the catalog stats behind the flagged/costliest scan and
// decomposes its cost. Any failure (no scan, unresolved relation, missing privilege)
// degrades gracefully to a stderr note and a nil report — -vvv never turns into an error.
func buildCostReport(ctx context.Context, conn *db.Conn, parsed *explain.ExplainOutput, diag explain.Diagnosis, errOut io.Writer) *costReport {
	scan := explain.PrimaryScanNode(parsed, diag)
	if scan == nil || scan.RelationName == "" {
		fmt.Fprintln(errOut, "note: -vvv found no single scan/relation to profile in this plan; showing -vv.")
		return nil
	}
	stats, err := catalog.RelationCostStats(ctx, conn, scan.RelationName)
	if err != nil {
		fmt.Fprintf(errOut, "note: couldn't read catalog stats for %q (%v); showing -vv.\n", scan.RelationName, err)
		return nil
	}
	settings, err := catalog.PlannerCostSettings(ctx, conn)
	if err != nil {
		fmt.Fprintf(errOut, "note: couldn't read planner cost settings (%v); showing -vv.\n", err)
		return nil
	}
	workers := explain.PlannedWorkers(parsed)
	cost, ok := explain.DecomposeScanCost(scan, explain.ScanCostInputs{
		Reltuples:       stats.Reltuples,
		Relpages:        stats.Relpages,
		SeqPageCost:     settings.SeqPageCost,
		CPUTupleCost:    settings.CPUTupleCost,
		CPUOperatorCost: settings.CPUOperatorCost,
		Workers:         workers,
	})
	return &costReport{node: scan, stats: *stats, settings: settings, workers: workers, cost: cost, hasCost: ok}
}

// costBreakdownJSON is the -vvv block as it appears under `-o json`.
type costBreakdownJSON struct {
	Relation  string               `json:"relation"`
	Stats     catalog.RelCostStats `json:"stats"`
	Settings  catalog.CostSettings `json:"settings"`
	Workers   int                  `json:"workers_planned"`
	NodeType  string               `json:"node_type"`
	PlanTotal float64              `json:"plan_total_cost"`
	Cost      *explain.SeqScanCost `json:"breakdown,omitempty"` // nil => node type not decomposed
}

func (c *costReport) json() *costBreakdownJSON {
	j := &costBreakdownJSON{
		Relation:  c.stats.Relname,
		Stats:     c.stats,
		Settings:  c.settings,
		Workers:   c.workers,
		NodeType:  c.node.NodeType,
		PlanTotal: c.node.TotalCost,
	}
	if c.hasCost {
		cost := c.cost
		j.Cost = &cost
	}
	return j
}

// printExplanation renders the -vv plain-language notes as wrapped bullets.
func printExplanation(w io.Writer, bullets []string) {
	if len(bullets) == 0 {
		return
	}
	fmt.Fprintln(w, "\nExplanation:")
	for _, b := range bullets {
		fmt.Fprintf(w, "  • %s\n", wrapText(b, "    ", 84))
	}
}

// printCostReport renders the -vvv block: exact catalog facts, then the approximate cost
// arithmetic for the flagged scan (or a note when the node type isn't decomposed).
func printCostReport(w io.Writer, c *costReport) {
	s, set := c.stats, c.settings
	fmt.Fprintf(w, "\nCatalog facts for %q (ground truth):\n", s.Relname)
	fmt.Fprintf(w, "  rows ~%s · pages %s · heap %s\n", comma(s.Reltuples), comma(float64(s.Relpages)), s.SizePretty)
	fmt.Fprintf(w, "  seq_page_cost %g · cpu_tuple_cost %g · cpu_operator_cost %g\n",
		set.SeqPageCost, set.CPUTupleCost, set.CPUOperatorCost)

	if !c.hasCost {
		fmt.Fprintf(w, "  (cost breakdown is shown only for sequential scans; this node is a %s)\n", c.node.NodeType)
		return
	}

	cost := c.cost
	perRow := set.CPUTupleCost
	rowTerm := fmt.Sprintf("CPU per row  %g × %s", perRow, comma(s.Reltuples))
	if cost.HasFilter {
		perRow += set.CPUOperatorCost
		rowTerm = fmt.Sprintf("CPU + filter per row  %g × %s", perRow, comma(s.Reltuples))
	}
	fmt.Fprintf(w, "Cost breakdown (%s — approximate; the cost model varies by version):\n", c.node.NodeType)
	fmt.Fprintf(w, "  read all heap pages   %g × %s = %s\n", set.SeqPageCost, comma(float64(s.Relpages)), comma(cost.DiskCost))
	fmt.Fprintf(w, "  %s = %s\n", rowTerm, comma(cost.CPURaw))
	if cost.Divisor != 1 {
		leader := cost.Divisor - float64(c.workers)
		fmt.Fprintf(w, "  ÷ parallel divisor (%d workers + %.1f leader = %.1f) = %s\n",
			c.workers, leader, cost.Divisor, comma(cost.CPUDivided))
	}
	fmt.Fprintf(w, "  total ≈ %s   (plan says %s)\n", comma(cost.Total), comma(c.node.TotalCost))
}

// comma formats a number with thousands separators and no decimals (e.g. 4546528 ->
// "4,546,528"), for the cost-breakdown arithmetic.
func comma(n float64) string {
	s := fmt.Sprintf("%.0f", n)
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out []byte
	for i, d := range []byte(s) {
		if i > 0 && (len(s)-i)%3 == 0 {
			out = append(out, ',')
		}
		out = append(out, d)
	}
	if neg {
		return "-" + string(out)
	}
	return string(out)
}

func newDiagnosisView(p *explain.ExplainOutput, d explain.Diagnosis, analyzed bool) diagnosisView {
	return diagnosisView{
		Findings:      d.Findings,
		Note:          d.Note,
		Analyzed:      analyzed,
		PlanningTime:  p.PlanningTime,
		ExecutionTime: p.ExecutionTime,
	}
}
