package cmd

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/render"
)

func newDescribeRoleCmd() *cobra.Command {
	var privileges bool
	c := &cobra.Command{
		Use:     "role <name>",
		Aliases: []string{"user"},
		Short:   "Show one role's attributes, memberships, ownership, and effective privileges",
		Long: "role shows everything the catalogs know about one role (Postgres calls users\n" +
			"'roles'): its attributes, whether a password is set, memberships in BOTH directions,\n" +
			"role-scoped settings from ALTER ROLE ... SET, the objects it owns, its effective\n" +
			"privileges on databases/schemas/relations, ALTER DEFAULT PRIVILEGES entries touching\n" +
			"it, and the sessions open as it right now. Predefined pg_* roles can be described\n" +
			"too. Read-only. `pgdx describe user` is an alias.\n\n" +
			"Privileges are EFFECTIVE, not direct grants: they include what the role inherits\n" +
			"through its memberships and what was granted to PUBLIC. That is the answer to \"can\n" +
			"this role read that table?\" — it is NOT the list of GRANT statements someone ran.\n" +
			"Objects the role has NO access to are summarized rather than listed, so the output\n" +
			"stays roughly one screen; -o json always carries every row either way.\n\n" +
			"Role-scoped settings are worth a second look: they override postgresql.conf for this\n" +
			"role only, and `pgdx get settings` (which reports pgdx's own session) can't show\n" +
			"them. Default privileges likewise apply only to objects created AFTER they were set.\n\n" +
			"Relation privileges are summarized per schema by default. --privileges enumerates\n" +
			"every relation the role can touch, which costs a privilege check per relation.\n\n" +
			"Two things the catalogs cannot answer: SESSIONS is connections open now, not last\n" +
			"login (core Postgres keeps no login history), and the password hash is never read —\n" +
			"only whether one is set, and only if you're superuser.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			noteContext(cmd)
			ctx := context.Background()
			conn, release, err := dial(ctx, cmd, flagDatabase)
			if err != nil {
				return err
			}
			defer release()

			d, err := catalog.DescribeRole(ctx, conn, args[0], privileges)
			if err != nil {
				return err
			}
			if format == render.FormatJSON {
				return render.Render(cmd.OutOrStdout(), format, d)
			}
			return renderRoleDetail(cmd, d, conn.Database(), privileges)
		},
	}
	c.Flags().BoolVar(&privileges, "privileges", false,
		"enumerate every relation the role holds a privilege on, instead of a per-schema summary")
	return c
}

// roleTermWidth is the layout budget for pairing two tables side by side. Wider than 80 so
// the common pairs fit, but small enough that a pair which would wrap gets stacked instead.
const roleTermWidth = 110

// roleLabelWidth aligns the collapsed one-line summaries into a label column.
const roleLabelWidth = 12

// renderRoleDetail prints the human role detail as a sequence of tables. Two things keep
// it near one screen without giving up the tabular layout: rows for objects the role has
// NO access to are dropped (and named in a footer, so the list never looks complete when
// it isn't), and small related tables are paired into two columns. -o json is the lossless
// view. Data → stdout (D4); degradation notes go last.
func renderRoleDetail(cmd *cobra.Command, d *catalog.RoleDetail, database string, withGrants bool) error {
	out := cmd.OutOrStdout()

	fmt.Fprintf(out, "%s — %s\n", render.Bold(fmt.Sprintf("Role %q", d.Name)),
		strings.Join(roleAttributes(d), ", "))
	fmt.Fprintf(out, "Password: %s\n", rolePasswordStatus(d))
	for _, w := range roleHeaderWarnings(d) {
		fmt.Fprintln(out, w)
	}

	// Memberships: two directions, two narrow tables, one row of vertical space.
	fmt.Fprintln(out)
	if err := render.SideBySide(out, roleTermWidth,
		roleGrantsView{rows: d.MemberOf, heading: "MEMBER OF"},
		roleGrantsView{rows: d.Members, heading: "MEMBERS"}); err != nil {
		return err
	}

	if len(d.Settings) > 0 {
		fmt.Fprintln(out, "\nRole-scoped settings — override postgresql.conf for this role only, and are")
		fmt.Fprintln(out, "invisible to `pgdx get settings`:")
		if err := render.Render(out, render.FormatTable, roleSettingsView(d.Settings)); err != nil {
			return err
		}
	}

	if len(d.Owns) > 0 {
		fmt.Fprintln(out)
		if err := render.Render(out, render.FormatTable, ownedGroupsView(d.Owns)); err != nil {
			return err
		}
		fmt.Fprintf(out, "  ⚠ DROP ROLE blocked until these move: REASSIGN OWNED BY %s TO <role>, then DROP OWNED BY %s.\n",
			d.Name, d.Name)
	}

	fmt.Fprintf(out, "\n%s — inherited and PUBLIC grants included; no-access objects counted below.\n",
		render.Bold("Effective privileges"))
	if err := renderRolePrivileges(out, d, database, withGrants); err != nil {
		return err
	}

	if len(d.DefaultACLs) > 0 {
		fmt.Fprintln(out, "\nDefault privileges — apply to objects created LATER, not existing ones:")
		if err := render.Render(out, render.FormatTable, defaultACLsView(d.DefaultACLs)); err != nil {
			return err
		}
	}

	if len(d.Sessions) == 0 {
		// Postgres tracks no login history, so "no sessions" is not evidence of disuse —
		// worth saying inline, because the wrong reading gets roles dropped.
		fmt.Fprintln(out, "\nSessions: none open (Postgres keeps no login history — not evidence it's unused)")
	} else {
		fmt.Fprintf(out, "\nSessions — %s open now:\n", withThousands(int64(len(d.Sessions))))
		if err := render.Render(out, render.FormatTable, roleSessionsView(d.Sessions)); err != nil {
			return err
		}
		if d.MaskedQueries {
			fmt.Fprintln(out, "  ⚠ Rows reading <insufficient privilege> are masked: your role lacks pg_monitor,")
			fmt.Fprintln(out, "    so another role's query text, state, and duration are hidden (the connection")
			fmt.Fprintln(out, "    itself is real). GRANT pg_monitor TO <your role> to see them.")
		}
	}

	// One line naming everything the tables above left out, so brevity never reads as
	// completeness.
	if omitted := roleOmissions(d, withGrants); omitted != "" {
		fmt.Fprintf(out, "\nNo access to: %s\n", omitted)
	}

	if len(d.Notes) > 0 {
		fmt.Fprintln(out, "\nNotes:")
		for _, n := range d.Notes {
			fmt.Fprintf(out, "  • %s\n", n)
		}
	}
	return nil
}

// renderRolePrivileges writes the database / schema / relation privilege tables, pairing
// the two small ones. A superuser's are replaced by a single line: ACLs aren't consulted
// for one, so every cell would be a check mark for every object in the cluster.
// --privileges still forces the enumeration, since asking explicitly outranks brevity.
func renderRolePrivileges(out io.Writer, d *catalog.RoleDetail, database string, withGrants bool) error {
	if d.Superuser && !withGrants {
		fmt.Fprintln(out, "  everything, on every object — SUPERUSER is not subject to ACLs.")
		return nil
	}

	dbs, _ := splitDBGrants(d.Databases)
	schemas, _ := splitSchemaGrants(d.Schemas)

	// The table shows where this role is special; objects where it only has the defaults
	// every role gets collapse into a single line, whether that's all of them or the
	// remainder after a few exceptions.
	dbRows, dbGeneric, dbLine, dbFooter := planGrantBlock(
		dbs, partitionDBGrants, dbSignature, dbPhrase, "database")
	schemaRows, schemaGeneric, schemaLine, schemaFooter := planGrantBlock(
		schemas, partitionSchemaGrants, schemaSignature, schemaPhrase, "schema")

	dbBlock := grantBlock{
		line: dbLine, footer: dbFooter,
		table: dbGrantsView(append(append([]catalog.DBGrant{}, dbRows...), dbGeneric...)),
	}
	schemaBlock := grantBlock{
		line: schemaLine, footer: schemaFooter,
		table: schemaGrantsView(append(append([]catalog.SchemaGrant{}, schemaRows...), schemaGeneric...)),
	}

	switch {
	case dbBlock.line != "" && schemaBlock.line != "":
		fmt.Fprintf(out, "%-*s%s\n", roleLabelWidth, "Databases:", dbBlock.line)
		fmt.Fprintf(out, "%-*s%s\n", roleLabelWidth, "Schemas:", schemaBlock.line)
	case dbBlock.line != "":
		fmt.Fprintf(out, "%-*s%s\n", roleLabelWidth, "Databases:", dbBlock.line)
		if err := schemaBlock.write(out); err != nil {
			return err
		}
	case schemaBlock.line != "":
		if err := dbBlock.write(out); err != nil {
			return err
		}
		fmt.Fprintf(out, "%-*s%s\n", roleLabelWidth, "Schemas:", schemaBlock.line)
	default:
		// Both are tables: pair them, then their footers (SideBySide can't carry per-table
		// trailing notes).
		if err := render.SideBySide(out, roleTermWidth, dbBlock.table, schemaBlock.table); err != nil {
			return err
		}
		for _, f := range []string{dbBlock.footer, schemaBlock.footer} {
			if f != "" {
				fmt.Fprintln(out, f)
			}
		}
	}

	if anyPublicGrants(dbs, schemas) {
		fmt.Fprintln(out, "  * = via PUBLIC, so every role has it — an unmarked ✓ is what THIS role was given.")
		fmt.Fprintln(out, "      (Postgres grants CONNECT+TEMP on every database and USAGE on schema public.)")
	}
	if !d.Login {
		fmt.Fprintln(out, "  Note: CONNECT is only consulted at login, so it is inert for a NOLOGIN role.")
	}

	if withGrants {
		fmt.Fprintf(out, "\nRelations in %q the role can touch:\n", database)
		if len(d.TableGrants) == 0 {
			fmt.Fprintln(out, "  (none)")
			return nil
		}
		return render.Render(out, render.FormatTable, tableGrantsView(d.TableGrants))
	}

	reachable, _ := splitTableSummary(d.TableSummary)
	if len(reachable) == 0 {
		fmt.Fprintf(out, "\nRelations in %q: none reachable.\n", database)
		return nil
	}
	fmt.Fprintf(out, "\nRelations in %q — counts are relations reachable, of TOTAL:\n", database)
	return render.Render(out, render.FormatTable, tableSummaryView(reachable))
}

// roleOmissions names every object the privilege tables dropped for having no access,
// grouped by kind. Returns "" when nothing was omitted.
func roleOmissions(d *catalog.RoleDetail, withGrants bool) string {
	if d.Superuser && !withGrants {
		return ""
	}
	var parts []string
	if _, noDBs := splitDBGrants(d.Databases); len(noDBs) > 0 {
		parts = append(parts, pluralizeCount(int64(len(noDBs)), "database")+" ("+summarizeNames(noDBs)+")")
	}
	_, noSchemas := splitSchemaGrants(d.Schemas)
	if len(noSchemas) > 0 {
		parts = append(parts, pluralizeCount(int64(len(noSchemas)), "schema")+" ("+summarizeNames(noSchemas)+")")
	}
	if !withGrants {
		// A schema already reported as unreachable explains its own relations — naming it
		// twice ("1 schema (app); relations in app") is noise, not precision.
		skip := make(map[string]bool, len(noSchemas))
		for _, s := range noSchemas {
			skip[s] = true
		}
		var rels []string
		for _, s := range d.TableSummary {
			if skip[s.Schema] || s.Select+s.Insert+s.Update+s.Delete+s.Owned > 0 {
				continue
			}
			rels = append(rels, fmt.Sprintf("%s (%s)", s.Schema, pluralizeCount(s.Total, "relation")))
		}
		if len(rels) > 0 {
			parts = append(parts, "relations in "+summarizeNames(rels))
		}
	}
	return strings.Join(parts, "; ")
}

// summarizeNames lists names but caps the list, so "no access" on a 200-schema database
// doesn't become the longest line in the output.
func summarizeNames(names []string) string {
	const max = 6
	if len(names) <= max {
		return strings.Join(names, ", ")
	}
	return fmt.Sprintf("%s, … (%s more)",
		strings.Join(names[:max], ", "), withThousands(int64(len(names)-max)))
}

// splitDBGrants separates the databases the role can reach from those it can't, so the
// common case (access to one of many) costs one table row instead of a screen of them.
func splitDBGrants(grants []catalog.DBGrant) (reachable []catalog.DBGrant, noAccess []string) {
	for _, g := range grants {
		if !g.Owner && !g.Connect && !g.Create && !g.Temp {
			noAccess = append(noAccess, g.Database)
			continue
		}
		reachable = append(reachable, g)
	}
	return reachable, noAccess
}

func splitSchemaGrants(grants []catalog.SchemaGrant) (reachable []catalog.SchemaGrant, noAccess []string) {
	for _, g := range grants {
		if !g.Owner && !g.Usage && !g.Create {
			noAccess = append(noAccess, g.Schema)
			continue
		}
		reachable = append(reachable, g)
	}
	return reachable, noAccess
}

// splitTableSummary separates schemas the role can reach from those it can't. A schema it
// owns nothing and can touch nothing in contributes a name, not a row of zeros.
func splitTableSummary(rows []catalog.SchemaTableGrants) (reachable []catalog.SchemaTableGrants, unreachable []string) {
	for _, s := range rows {
		if s.Select+s.Insert+s.Update+s.Delete+s.Owned == 0 {
			unreachable = append(unreachable, fmt.Sprintf("%s (%s)", s.Schema, pluralizeCount(s.Total, "relation")))
			continue
		}
		reachable = append(reachable, s)
	}
	return reachable, unreachable
}

func pluralizeCount(n int64, noun string) string {
	if n == 1 {
		return withThousands(n) + " " + noun
	}
	return withThousands(n) + " " + noun + "s"
}

// pluralizeKind pluralizes the catalog's object-kind labels.
func pluralizeKind(kind string, n int64) string {
	if n == 1 {
		return kind
	}
	if strings.HasSuffix(kind, "s") || strings.HasSuffix(kind, "x") {
		return kind + "es"
	}
	return kind + "s"
}

// roleAttributes renders the attribute set the way psql's \du does — only what's true,
// plus the two that matter by their absence (no login, NOINHERIT).
func roleAttributes(d *catalog.RoleDetail) []string {
	var attrs []string
	if d.Superuser {
		attrs = append(attrs, "SUPERUSER")
	}
	if d.CreateDB {
		attrs = append(attrs, "create DB")
	}
	if d.CreateRole {
		attrs = append(attrs, "create role")
	}
	if d.Replication {
		attrs = append(attrs, "replication")
	}
	if d.BypassRLS {
		attrs = append(attrs, "bypass RLS")
	}
	if d.Login {
		attrs = append(attrs, "login")
	} else {
		attrs = append(attrs, "NOLOGIN")
	}
	if !d.Inherit {
		attrs = append(attrs, "NOINHERIT")
	}
	if d.ConnLimit >= 0 {
		attrs = append(attrs, fmt.Sprintf("conn limit %d", d.ConnLimit))
	}
	if d.ValidUntil != "" && d.ValidUntil != "infinity" {
		until := d.ValidUntil
		if len(until) >= 10 {
			until = until[:10]
		}
		attrs = append(attrs, "valid until "+until)
	}
	if len(attrs) == 0 {
		return []string{"—"}
	}
	return attrs
}

func rolePasswordStatus(d *catalog.RoleDetail) string {
	switch {
	case d.HasPassword == nil:
		return "unknown (needs superuser to read)"
	case *d.HasPassword:
		return "set"
	default:
		return "none — this role cannot authenticate by password"
	}
}

// roleHeaderWarnings surfaces the configurations that make the rest of the output
// misleading if read at face value.
func roleHeaderWarnings(d *catalog.RoleDetail) []string {
	var w []string
	if !d.Inherit && len(d.MemberOf) > 0 {
		// has_*_privilege() honors rolinherit, so a NOINHERIT role's memberships contribute
		// NOTHING to the privileges below — this role can reach MORE than is shown here, but
		// only after an explicit SET ROLE.
		w = append(w, "⚠ NOINHERIT — this role does not automatically hold its groups' privileges, and\n"+
			"  the privileges below EXCLUDE them. After SET ROLE <group> it can reach more;\n"+
			"  describe that group to see what.")
	}
	if d.Login && d.HasPassword != nil && !*d.HasPassword {
		w = append(w, "⚠ LOGIN with no password — reachable only via a non-password auth method "+
			"(peer, trust,\n  cert, gss); check pg_hba with `pgdx audit`.")
	}
	if !d.Login && len(d.Sessions) > 0 {
		w = append(w, "⚠ NOLOGIN but sessions are open — these reached this role via SET ROLE.")
	}
	if note := catalog.PredefinedRoleWarning(d.Name); note != "" {
		w = append(w, "⚠ predefined role — "+note+"; every member below inherits that.")
	}
	return w
}

// roleGrantsView is one membership direction. The heading carries the direction, so the
// two can sit side by side and still be unambiguous.
type roleGrantsView struct {
	rows    []catalog.RoleGrant
	heading string
}

func (v roleGrantsView) Headers() []string { return []string{v.heading, "ADMIN", ""} }
func (v roleGrantsView) Rows() [][]string {
	if len(v.rows) == 0 {
		return [][]string{{render.Dim("(none)"), "", ""}}
	}
	out := make([][]string, 0, len(v.rows))
	for _, g := range v.rows {
		// Only predefined roles that confer outsized power get a note; ordinary groups
		// would just add a column of dashes.
		note := catalog.PredefinedRoleWarning(g.Role)
		if note != "" {
			note = "⚠ " + note
		}
		out = append(out, []string{g.Role, render.Flag(g.Admin), note})
	}
	return out
}

type roleSettingsView []catalog.RoleSetting

func (v roleSettingsView) Headers() []string { return []string{"SETTING", "VALUE", "SCOPE"} }
func (v roleSettingsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, s := range v {
		scope := render.Dim("all databases")
		if s.Database != "" {
			scope = "database " + s.Database
		}
		out = append(out, []string{s.Name, dashIfEmpty(s.Value), scope})
	}
	return out
}

type ownedGroupsView []catalog.OwnedGroup

func (v ownedGroupsView) Headers() []string { return []string{"OWNS", "COUNT", "EXAMPLES"} }
func (v ownedGroupsView) Aligns() []render.Align {
	return []render.Align{render.AlignLeft, render.AlignRight, render.AlignLeft}
}
func (v ownedGroupsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, g := range v {
		examples := strings.Join(g.Samples, ", ")
		// A sampled list must say how much it isn't showing, or 42 tables reads as 2.
		if g.Count > int64(len(g.Samples)) {
			examples += render.Dim(fmt.Sprintf(", … %s more", withThousands(g.Count-int64(len(g.Samples)))))
		}
		out = append(out, []string{pluralizeKind(g.Kind, g.Count), withThousands(g.Count), dashIfEmpty(examples)})
	}
	return out
}

// privCell is one privilege of one object: its display name, the catalog's spelling when
// that differs, and whether the role holds it.
type privCell struct {
	Name  string
	Alias string // catalog spelling, e.g. pg_database's TEMPORARY for the TEMP column
	Held  bool
}

func dbPrivCells(g catalog.DBGrant) []privCell {
	return []privCell{
		{Name: "CONNECT", Held: g.Connect},
		{Name: "CREATE", Held: g.Create},
		{Name: "TEMP", Alias: "TEMPORARY", Held: g.Temp},
	}
}

func schemaPrivCells(g catalog.SchemaGrant) []privCell {
	return []privCell{
		{Name: "USAGE", Held: g.Usage},
		{Name: "CREATE", Held: g.Create},
	}
}

// viaPublic reports whether PUBLIC also holds this privilege — i.e. whether the role's
// having it says anything about the role.
func viaPublic(c privCell, publicPrivs []string) bool {
	for _, p := range publicPrivs {
		if strings.EqualFold(p, c.Name) || (c.Alias != "" && strings.EqualFold(p, c.Alias)) {
			return true
		}
	}
	return false
}

// publicMark renders one boolean privilege cell. A held privilege is always a green check —
// the thing worth spotting — with a dim trailing "*" when PUBLIC holds it too, so the
// "everyone has this" nuance rides along without dulling the signal. The marker is a
// character rather than only a color because color is off whenever output is piped.
func publicMark(held bool, priv string, publicPrivs []string) string {
	if !held {
		return render.Flag(false)
	}
	if viaPublic(privCell{Name: priv, Alias: publicAlias(priv)}, publicPrivs) {
		return render.Green("✓") + render.Dim("*")
	}
	return render.Flag(true)
}

func publicAlias(priv string) string {
	if priv == "TEMP" {
		return "TEMPORARY"
	}
	return ""
}

// privSignature is the collapse key: two objects with the same signature grant exactly the
// same thing, so listing them separately is repetition rather than information.
func privSignature(owner bool, cells []privCell, publicPrivs []string) string {
	var b strings.Builder
	if owner {
		b.WriteString("owner;")
	}
	for _, c := range cells {
		if !c.Held {
			continue
		}
		b.WriteString(c.Name)
		if viaPublic(c, publicPrivs) {
			b.WriteByte('*')
		}
		b.WriteByte(',')
	}
	return b.String()
}

// privPhrase is the human form of a signature, for the collapsed one-line summary.
func privPhrase(owner bool, cells []privCell, publicPrivs []string) (phrase string, anyPublic bool) {
	var parts []string
	if owner {
		parts = append(parts, "owner")
	}
	for _, c := range cells {
		if !c.Held {
			continue
		}
		if viaPublic(c, publicPrivs) {
			parts = append(parts, render.Green(c.Name)+render.Dim("*"))
			anyPublic = true
			continue
		}
		parts = append(parts, render.Green(c.Name))
	}
	if len(parts) == 0 {
		return "", false
	}
	return strings.Join(parts, ", "), anyPublic
}

// partitionDBGrants splits reachable databases into those where this role holds something
// of its own (a grant PUBLIC doesn't have, or ownership) and those where it only has the
// defaults every role gets. The second group is the noise: on a normal cluster it's every
// database, and listing it row by row says nothing about the role.
func partitionDBGrants(gs []catalog.DBGrant) (specific, generic []catalog.DBGrant) {
	for _, g := range gs {
		if allHeldViaPublic(g.Owner, dbPrivCells(g), g.PublicPrivs) {
			generic = append(generic, g)
			continue
		}
		specific = append(specific, g)
	}
	return specific, generic
}

func partitionSchemaGrants(gs []catalog.SchemaGrant) (specific, generic []catalog.SchemaGrant) {
	for _, g := range gs {
		if allHeldViaPublic(g.Owner, schemaPrivCells(g), g.PublicPrivs) {
			generic = append(generic, g)
			continue
		}
		specific = append(specific, g)
	}
	return specific, generic
}

// genericGroupLine describes a collapsed PUBLIC-defaults group: how many objects, what they
// grant (when they all grant the same thing), and which they are. Returns "" when the group
// is too small to be worth collapsing — one row in a table beats one row of prose.
func genericGroupLine(names []string, noun, phrase string, standalone bool) string {
	if len(names) < 2 {
		return ""
	}
	scope := fmt.Sprintf("all %d %ss", len(names), noun)
	if len(names) == 2 {
		scope = "both " + noun + "s" // "all 2 databases" reads like a typo
	}
	if !standalone {
		scope = fmt.Sprintf("%d other %ss", len(names), noun)
	}
	s := scope
	if phrase != "" {
		s += ": " + phrase
	}
	if standalone {
		return s + " — nothing specific to this role"
	}
	return fmt.Sprintf("  plus %s (%s)", s, summarizeNames(names))
}

// grantBlock is one privilege section: either a collapsed one-line summary, or a table
// with an optional footer collapsing the uninteresting rows.
type grantBlock struct {
	line   string
	footer string
	table  render.Tabular
}

func (b grantBlock) write(out io.Writer) error {
	if err := render.Render(out, render.FormatTable, b.table); err != nil {
		return err
	}
	if b.footer != "" {
		_, err := fmt.Fprintln(out, b.footer)
		return err
	}
	return nil
}

// Per-type adapters so planGrantBlock can work on either grant type.
func dbSignature(g catalog.DBGrant) string {
	return privSignature(g.Owner, dbPrivCells(g), g.PublicPrivs)
}
func dbPhrase(g catalog.DBGrant) string {
	p, _ := privPhrase(g.Owner, dbPrivCells(g), g.PublicPrivs)
	return p
}
func schemaSignature(g catalog.SchemaGrant) string {
	return privSignature(g.Owner, schemaPrivCells(g), g.PublicPrivs)
}
func schemaPhrase(g catalog.SchemaGrant) string {
	p, _ := privPhrase(g.Owner, schemaPrivCells(g), g.PublicPrivs)
	return p
}

// planGrantBlock decides how one privilege section renders. It returns the rows to table,
// the generic rows that could NOT be collapsed (so they still appear), and either a
// standalone line (when everything collapses) or a table footer.
//
// The collapse only fires when the generic group is uniform — objects that grant different
// things must not be summarized as if they were the same.
func planGrantBlock[T any](
	all []T,
	partition func([]T) (specific, generic []T),
	signature func(T) string,
	phrase func(T) string,
	noun string,
) (rows, leftoverGeneric []T, line, footer string) {
	specific, generic := partition(all)

	// Guard the empty case before indexing: a role whose every object is special has no
	// generic group at all.
	uniform := len(generic) > 0
	for i := 1; i < len(generic); i++ {
		if signature(generic[i]) != signature(generic[0]) {
			uniform = false
			break
		}
	}
	if !uniform {
		// Mixed defaults: nothing safe to collapse, so table everything.
		return append(specific, generic...), nil, "", ""
	}

	names := make([]string, 0, len(generic))
	for _, g := range generic {
		names = append(names, objectName(g))
	}
	summary := genericGroupLine(names, noun, phrase(generic[0]), len(specific) == 0)
	if summary == "" {
		// Too small to collapse — leave the generic rows in the table.
		return append(specific, generic...), nil, "", ""
	}
	if len(specific) == 0 {
		return nil, nil, summary, ""
	}
	return specific, nil, "", summary
}

// objectName pulls the display name out of either grant type.
func objectName(v any) string {
	switch g := v.(type) {
	case catalog.DBGrant:
		return g.Database
	case catalog.SchemaGrant:
		return g.Schema
	}
	return ""
}

// allHeldViaPublic reports whether every privilege the role holds here comes from PUBLIC,
// i.e. nothing was granted to this role in particular. Ownership counts as specific.
func allHeldViaPublic(owner bool, cells []privCell, publicPrivs []string) bool {
	if owner {
		return false
	}
	any := false
	for _, c := range cells {
		if !c.Held {
			continue
		}
		if !viaPublic(c, publicPrivs) {
			return false
		}
		any = true
	}
	return any
}

// anyPublicGrants reports whether any privilege actually SHOWN was earned via PUBLIC, so
// the legend prints only when a "*" is on screen.
func anyPublicGrants(dbs []catalog.DBGrant, schemas []catalog.SchemaGrant) bool {
	for _, g := range dbs {
		for _, c := range dbPrivCells(g) {
			if c.Held && viaPublic(c, g.PublicPrivs) {
				return true
			}
		}
	}
	for _, g := range schemas {
		for _, c := range schemaPrivCells(g) {
			if c.Held && viaPublic(c, g.PublicPrivs) {
				return true
			}
		}
	}
	return false
}

type dbGrantsView []catalog.DBGrant

func (v dbGrantsView) Headers() []string {
	return []string{"DATABASE", "OWN", "CONN", "CREATE", "TEMP"}
}
func (v dbGrantsView) Rows() [][]string {
	if len(v) == 0 {
		return [][]string{{render.Dim("(none)"), "", "", "", ""}}
	}
	out := make([][]string, 0, len(v))
	for _, g := range v {
		out = append(out, []string{
			g.Database, render.Flag(g.Owner),
			publicMark(g.Connect, "CONNECT", g.PublicPrivs),
			publicMark(g.Create, "CREATE", g.PublicPrivs),
			publicMark(g.Temp, "TEMP", g.PublicPrivs),
		})
	}
	return out
}

type schemaGrantsView []catalog.SchemaGrant

func (v schemaGrantsView) Headers() []string {
	return []string{"SCHEMA", "OWN", "USAGE", "CREATE", ""}
}
func (v schemaGrantsView) Rows() [][]string {
	if len(v) == 0 {
		return [][]string{{render.Dim("(none)"), "", "", "", ""}}
	}
	out := make([][]string, 0, len(v))
	for _, g := range v {
		// USAGE is the gate: without it, any relation grant inside the schema is unusable,
		// so a schema that shows relation counts but no USAGE is the explanation.
		note := ""
		if !g.Usage {
			note = "⚠ no USAGE — grants here unusable"
		}
		out = append(out, []string{
			g.Schema, render.Flag(g.Owner),
			publicMark(g.Usage, "USAGE", g.PublicPrivs),
			publicMark(g.Create, "CREATE", g.PublicPrivs),
			note,
		})
	}
	return out
}

type tableSummaryView []catalog.SchemaTableGrants

func (v tableSummaryView) Headers() []string {
	return []string{"SCHEMA", "TOTAL", "SELECT", "INSERT", "UPDATE", "DELETE", "OWNED"}
}
func (v tableSummaryView) Aligns() []render.Align {
	return []render.Align{
		render.AlignLeft, render.AlignRight, render.AlignRight, render.AlignRight,
		render.AlignRight, render.AlignRight, render.AlignRight,
	}
}
func (v tableSummaryView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, s := range v {
		out = append(out, []string{
			s.Schema, withThousands(s.Total), countCell(s.Select), countCell(s.Insert),
			countCell(s.Update), countCell(s.Delete), countCell(s.Owned),
		})
	}
	return out
}

// countCell dims a zero so the columns that matter stand out in a wide numeric row.
func countCell(n int64) string {
	if n == 0 {
		return render.Dim("0")
	}
	return withThousands(n)
}

type tableGrantsView []catalog.TableGrant

func (v tableGrantsView) Headers() []string {
	return []string{"SCHEMA", "NAME", "KIND", "OWN", "PRIVILEGES"}
}
func (v tableGrantsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, g := range v {
		out = append(out, []string{
			g.Schema, g.Name, g.Kind, render.Flag(g.Owner), dashIfEmpty(strings.Join(g.Privileges, ", ")),
		})
	}
	return out
}

type defaultACLsView []catalog.DefaultACL

func (v defaultACLsView) Headers() []string {
	return []string{"ON NEW", "IN SCHEMA", "CREATED BY", "GRANTED TO", "PRIVILEGES"}
}
func (v defaultACLsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, a := range v {
		schema := a.Schema
		if schema == "" {
			schema = render.Dim("(every)")
		}
		out = append(out, []string{
			a.ObjectType, schema, a.Grantor, a.Grantee, dashIfEmpty(strings.Join(a.Privileges, ", ")),
		})
	}
	return out
}

type roleSessionsView []catalog.RoleSession

func (v roleSessionsView) Headers() []string {
	return []string{"PID", "DB", "CLIENT", "APP", "STATE", "DURATION", "QUERY"}
}
func (v roleSessionsView) Aligns() []render.Align {
	return []render.Align{
		render.AlignRight, render.AlignLeft, render.AlignLeft, render.AlignLeft,
		render.AlignLeft, render.AlignRight, render.AlignLeft,
	}
}
func (v roleSessionsView) Rows() [][]string {
	out := make([][]string, 0, len(v))
	for _, s := range v {
		out = append(out, []string{
			strconv.FormatInt(int64(s.PID), 10),
			dashIfEmpty(s.Database), dashIfEmpty(s.ClientAddr), dashIfEmpty(s.AppName),
			dashIfEmpty(s.State), secondsHuman(s.DurationSec), compactQuery(s.Query, 50),
		})
	}
	return out
}
