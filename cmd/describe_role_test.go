package cmd

import (
	"strings"
	"testing"

	"github.com/capitaltg/pgdx/internal/catalog"
)

func TestRoleAttributes(t *testing.T) {
	t.Run("only true attributes, plus login state", func(t *testing.T) {
		got := roleAttributes(&catalog.RoleDetail{
			Superuser: true, CreateDB: true, Login: true, Inherit: true, ConnLimit: -1,
		})
		want := "SUPERUSER, create DB, login"
		if strings.Join(got, ", ") != want {
			t.Fatalf("attributes = %q, want %q", strings.Join(got, ", "), want)
		}
	})

	t.Run("absences that matter are stated", func(t *testing.T) {
		// NOLOGIN and NOINHERIT change how the role behaves, so silence would mislead.
		got := strings.Join(roleAttributes(&catalog.RoleDetail{ConnLimit: -1}), ", ")
		for _, want := range []string{"NOLOGIN", "NOINHERIT"} {
			if !strings.Contains(got, want) {
				t.Errorf("attributes %q should mention %q", got, want)
			}
		}
	})

	t.Run("unlimited connections and infinite validity are not noise", func(t *testing.T) {
		got := strings.Join(roleAttributes(&catalog.RoleDetail{
			Login: true, Inherit: true, ConnLimit: -1, ValidUntil: "infinity",
		}), ", ")
		if strings.Contains(got, "conn limit") {
			t.Errorf("ConnLimit -1 means unlimited and should be omitted, got %q", got)
		}
		if strings.Contains(got, "valid until") {
			t.Errorf("infinite validity should be omitted, got %q", got)
		}
	})

	t.Run("a real limit and expiry are shown, date trimmed", func(t *testing.T) {
		got := strings.Join(roleAttributes(&catalog.RoleDetail{
			Login: true, Inherit: true, ConnLimit: 20, ValidUntil: "2027-01-01 00:00:00+00",
		}), ", ")
		for _, want := range []string{"conn limit 20", "valid until 2027-01-01"} {
			if !strings.Contains(got, want) {
				t.Errorf("attributes %q should contain %q", got, want)
			}
		}
	})
}

func TestRolePasswordStatus(t *testing.T) {
	set, unset := true, false
	cases := []struct {
		name string
		in   *bool
		want string
	}{
		// nil is "we couldn't look", NOT "there is no password" — conflating them would
		// report every role as passwordless to a non-superuser.
		{"unreadable", nil, "unknown"},
		{"set", &set, "set"},
		{"absent", &unset, "none"},
	}
	for _, tc := range cases {
		got := rolePasswordStatus(&catalog.RoleDetail{HasPassword: tc.in})
		if !strings.Contains(got, tc.want) {
			t.Errorf("%s: password status = %q, want it to contain %q", tc.name, got, tc.want)
		}
	}
}

func TestRoleHeaderWarnings(t *testing.T) {
	noPass := false
	pass := true

	// has_*_privilege() honors rolinherit, so a NOINHERIT role's memberships contribute
	// nothing to the reported privileges. The warning must say they're EXCLUDED — claiming
	// the opposite would have the reader over-estimate what the role can reach.
	t.Run("NOINHERIT says privileges are excluded, not included", func(t *testing.T) {
		got := strings.Join(roleHeaderWarnings(&catalog.RoleDetail{
			MemberOf: []catalog.RoleGrant{{Role: "readonly"}},
		}), " ")
		if !strings.Contains(got, "EXCLUDE") {
			t.Fatalf("NOINHERIT warning should say the privileges below exclude memberships, got %q", got)
		}
		if !strings.Contains(got, "SET ROLE") {
			t.Errorf("NOINHERIT warning should name the SET ROLE escape hatch, got %q", got)
		}
	})

	t.Run("NOINHERIT only warns when there is a membership to not inherit", func(t *testing.T) {
		without := roleHeaderWarnings(&catalog.RoleDetail{})
		if strings.Contains(strings.Join(without, " "), "NOINHERIT") {
			t.Fatalf("NOINHERIT with no memberships is harmless, got %v", without)
		}
	})

	t.Run("LOGIN with no password", func(t *testing.T) {
		got := strings.Join(roleHeaderWarnings(&catalog.RoleDetail{
			Login: true, Inherit: true, HasPassword: &noPass,
		}), " ")
		if !strings.Contains(got, "no password") {
			t.Fatalf("a passwordless login role should be flagged, got %q", got)
		}
	})

	t.Run("unknown password status does not warn", func(t *testing.T) {
		// HasPassword nil means unreadable; warning here would cry wolf at every
		// non-superuser invocation.
		got := strings.Join(roleHeaderWarnings(&catalog.RoleDetail{Login: true, Inherit: true}), " ")
		if strings.Contains(got, "no password") {
			t.Fatalf("unknown password status must not be reported as absent, got %q", got)
		}
	})

	t.Run("NOLOGIN with live sessions means SET ROLE", func(t *testing.T) {
		got := strings.Join(roleHeaderWarnings(&catalog.RoleDetail{
			Inherit: true, HasPassword: &pass, Sessions: []catalog.RoleSession{{PID: 1}},
		}), " ")
		if !strings.Contains(got, "SET ROLE") {
			t.Fatalf("sessions on a NOLOGIN role should be explained, got %q", got)
		}
	})

	t.Run("a clean role warns about nothing", func(t *testing.T) {
		got := roleHeaderWarnings(&catalog.RoleDetail{Login: true, Inherit: true, HasPassword: &pass})
		if len(got) != 0 {
			t.Fatalf("ordinary role should produce no warnings, got %v", got)
		}
	})
}

func TestPluralizeKind(t *testing.T) {
	cases := []struct{ kind, want string }{
		{"table", "tables"},
		{"view", "views"},
		{"schema", "schemas"},
		{"tablespace", "tablespaces"},
		{"materialized view", "materialized views"},
	}
	for _, tc := range cases {
		if got := pluralizeKind(tc.kind, 2); got != tc.want {
			t.Errorf("pluralizeKind(%q, 2) = %q, want %q", tc.kind, got, tc.want)
		}
		if got := pluralizeKind(tc.kind, 1); got != tc.kind {
			t.Errorf("pluralizeKind(%q, 1) = %q, want it unchanged", tc.kind, got)
		}
	}
}

func TestSplitTableSummary(t *testing.T) {
	reachable, unreachable := splitTableSummary([]catalog.SchemaTableGrants{
		{Schema: "public", Total: 4, Select: 4, Insert: 2},
		{Schema: "app", Total: 1},
		{Schema: "empty", Total: 0},
	})
	if len(reachable) != 1 || reachable[0].Schema != "public" {
		t.Fatalf("reachable = %v, want just public", reachable)
	}
	// Named with its size, so "no access" doesn't hide how much is behind it.
	if len(unreachable) != 2 || unreachable[0] != "app (1 relation)" {
		t.Fatalf("unreachable = %v, want app named with its relation count", unreachable)
	}
	if unreachable[1] != "empty (0 relations)" {
		t.Errorf("unreachable[1] = %q, want a pluralized zero", unreachable[1])
	}
}

func TestSummarizeNames_Caps(t *testing.T) {
	many := []string{"a", "b", "c", "d", "e", "f", "g", "h", "i"}
	got := summarizeNames(many)
	if !strings.Contains(got, "3 more") {
		t.Errorf("a long list should be capped with a remainder count, got %q", got)
	}
	if short := summarizeNames([]string{"a", "b"}); short != "a, b" {
		t.Errorf("a short list should be listed in full, got %q", short)
	}
}

func TestRoleSessionsView_TruncatesQuery(t *testing.T) {
	long := strings.Repeat("SELECT 1 ", 30)
	rows := roleSessionsView{
		{PID: 42, Database: "shop", State: "active", DurationSec: 90, Query: long},
		{PID: 43, DurationSec: -1},
	}.Rows()

	const queryCol = 6
	if len([]rune(rows[0][queryCol])) > 51 { // 50 + the ellipsis
		t.Errorf("QUERY should be capped, got %d runes", len([]rune(rows[0][queryCol])))
	}
	if rows[0][5] != "1m30s" {
		t.Errorf("DURATION = %q, want \"1m30s\"", rows[0][5])
	}
	// A masked row has no duration; "—" beats a fabricated 0.
	if rows[1][5] != "—" {
		t.Errorf("unknown DURATION = %q, want \"—\"", rows[1][5])
	}
}

func TestRoleGrantsView(t *testing.T) {
	rows := roleGrantsView{heading: "MEMBER OF", rows: []catalog.RoleGrant{
		{Role: "readonly"},
		{Role: "ops", Admin: true},
		{Role: "pg_read_all_data"},
	}}.Rows()

	if rows[0][0] != "readonly" || rows[0][2] != "" {
		t.Errorf("an ordinary membership needs no note, got %v", rows[0])
	}
	// Flags differ between the admin and non-admin rows (glyph, or color when on a TTY).
	if rows[0][1] == rows[1][1] {
		t.Errorf("ADMIN should render differently for true/false, both = %q", rows[0][1])
	}
	// A dangerous predefined role is the whole reason to look at memberships.
	if !strings.Contains(rows[2][2], "GRANT") {
		t.Errorf("pg_read_all_data should carry the GRANT-bypass warning, got %q", rows[2][2])
	}
}

func TestRoleGrantsView_EmptyIsExplicit(t *testing.T) {
	// Paired side by side, an empty direction must still occupy a row — a blank column
	// would read as "not queried" rather than "no memberships".
	rows := roleGrantsView{heading: "MEMBERS"}.Rows()
	if len(rows) != 1 || !strings.Contains(rows[0][0], "none") {
		t.Fatalf("empty view should render one explicit row, got %v", rows)
	}
}

func TestOwnedGroupsView_ReportsUnshownCount(t *testing.T) {
	rows := ownedGroupsView{
		{Kind: "table", Count: 42, Samples: []string{"public.a", "public.b"}},
		{Kind: "schema", Count: 1, Samples: []string{"reports"}},
	}.Rows()

	// A sampled list must say how much it isn't showing, or 42 tables reads as 2.
	if !strings.Contains(rows[0][2], "40 more") {
		t.Errorf("EXAMPLES = %q, want it to note the 40 unshown tables", rows[0][2])
	}
	if rows[0][0] != "tables" {
		t.Errorf("kind should pluralize with the count, got %q", rows[0][0])
	}
	if rows[1][0] != "schema" {
		t.Errorf("a count of 1 should stay singular, got %q", rows[1][0])
	}
	if strings.Contains(rows[1][2], "more") {
		t.Errorf("a fully-shown group should not claim more, got %q", rows[1][2])
	}
}

func TestSplitDBGrants(t *testing.T) {
	reachable, noAccess := splitDBGrants([]catalog.DBGrant{
		{Database: "shop", Connect: true},
		{Database: "locked"},
		{Database: "mine", Owner: true},
	})
	if len(reachable) != 2 {
		t.Fatalf("only reachable databases belong in the table, got %v", reachable)
	}
	// Omitted rows must still be named — silence would read as "no such database".
	if len(noAccess) != 1 || noAccess[0] != "locked" {
		t.Fatalf("noAccess = %v, want [locked]", noAccess)
	}
}

func TestSplitSchemaGrants(t *testing.T) {
	// CREATE without USAGE still counts as reachable: it's a real (if broken) grant, and
	// the view flags the missing USAGE rather than hiding the row.
	reachable, noAccess := splitSchemaGrants([]catalog.SchemaGrant{
		{Schema: "public", Usage: true},
		{Schema: "app"},
		{Schema: "odd", Create: true},
	})
	if len(reachable) != 2 || len(noAccess) != 1 || noAccess[0] != "app" {
		t.Fatalf("reachable = %v, noAccess = %v", reachable, noAccess)
	}
	rows := schemaGrantsView(reachable).Rows()
	if !strings.Contains(rows[1][4], "no USAGE") {
		t.Errorf("a schema with CREATE but no USAGE should be flagged, got %q", rows[1][4])
	}
}

func TestRoleOmissions_DoesNotNameASchemaTwice(t *testing.T) {
	// A schema with no USAGE explains its own relations; reporting both reads as two
	// separate problems.
	d := &catalog.RoleDetail{
		Schemas:      []catalog.SchemaGrant{{Schema: "app"}, {Schema: "public", Usage: true}},
		TableSummary: []catalog.SchemaTableGrants{{Schema: "app", Total: 1}, {Schema: "public", Total: 2, Select: 2}},
	}
	got := roleOmissions(d, false)
	if strings.Count(got, "app") != 1 {
		t.Fatalf("omissions should name app once, got %q", got)
	}
	if !strings.Contains(got, "1 schema (app)") {
		t.Errorf("omissions = %q, want the schema named", got)
	}
}

func TestRoleOmissions_SuperuserHasNone(t *testing.T) {
	d := &catalog.RoleDetail{Superuser: true, Schemas: []catalog.SchemaGrant{{Schema: "app"}}}
	if got := roleOmissions(d, false); got != "" {
		t.Fatalf("a superuser omits nothing (ACLs don't apply), got %q", got)
	}
}

func TestDefaultACLsView_RendersEverySchema(t *testing.T) {
	rows := defaultACLsView{
		{Schema: "", ObjectType: "tables", Grantor: "demo", Grantee: "PUBLIC", Privileges: []string{"SELECT"}},
	}.Rows()
	// An unscoped default applies everywhere; a blank cell would read as a schema named "".
	if !strings.Contains(rows[0][1], "every") {
		t.Errorf("IN SCHEMA = %q, want it to say every", rows[0][1])
	}
}

func TestPublicMark(t *testing.T) {
	// A privilege PUBLIC also holds is not this role's — it must read differently, or the
	// CONNECT column is a solid line of check marks for every role in every cluster.
	viaPublic := publicMark(true, "CONNECT", []string{"CONNECT", "TEMPORARY"})
	specific := publicMark(true, "CONNECT", nil)
	absent := publicMark(false, "CONNECT", nil)

	if viaPublic == specific {
		t.Fatal("a PUBLIC-derived privilege must render differently from a role-specific one")
	}
	// The distinction has to survive color being off (piped output), so it can't be color
	// alone — hence the trailing marker.
	if !strings.Contains(viaPublic, "*") {
		t.Errorf("PUBLIC-derived cell = %q, want a visible marker", viaPublic)
	}
	if strings.Contains(specific, "*") {
		t.Errorf("role-specific cell = %q, want no marker", specific)
	}
	if viaPublic == absent || specific == absent {
		t.Error("held and not-held must differ")
	}

	// pg_database spells it TEMPORARY; the column is TEMP. Missing this alias would report
	// the default TEMP grant as role-specific.
	if got := publicMark(true, "TEMP", []string{"TEMPORARY"}); !strings.Contains(got, "*") {
		t.Errorf("TEMP should match a TEMPORARY entry in the PUBLIC set, got %q", got)
	}
	// A privilege PUBLIC holds but this role does not is still just "no".
	if got := publicMark(false, "CREATE", []string{"CREATE"}); got != absent {
		t.Errorf("not-held = %q, want the plain no-flag", got)
	}
}

func TestAnyPublicGrants(t *testing.T) {
	// The legend prints only when a "*" is actually on screen.
	if anyPublicGrants(nil, nil) {
		t.Error("no grants means no legend")
	}
	if !anyPublicGrants([]catalog.DBGrant{{Connect: true, PublicPrivs: []string{"CONNECT"}}}, nil) {
		t.Error("a held, PUBLIC-derived database grant should trigger the legend")
	}
	if !anyPublicGrants(nil, []catalog.SchemaGrant{{Usage: true, PublicPrivs: []string{"USAGE"}}}) {
		t.Error("a held, PUBLIC-derived schema grant should trigger the legend")
	}
	if anyPublicGrants([]catalog.DBGrant{{Database: "x", Connect: true}}, []catalog.SchemaGrant{{Schema: "y"}}) {
		t.Error("a fully locked-down cluster needs no legend")
	}
	// PUBLIC holding a privilege the role does NOT hold marks nothing, so it must not
	// summon a legend explaining a marker that never appears.
	if anyPublicGrants([]catalog.DBGrant{{Database: "x", PublicPrivs: []string{"CONNECT"}}}, nil) {
		t.Error("an unheld privilege marks no cell, so it needs no legend")
	}
}

// The collapse is the answer to "every database says CONNECT ✓ — say it once". It must fire
// when the defaults are uniform, and must NOT fire when objects differ.
func TestPlanGrantBlock_Databases(t *testing.T) {
	pub := []string{"CONNECT", "TEMPORARY"}
	plan := func(gs []catalog.DBGrant) (rows []catalog.DBGrant, line, footer string) {
		r, _, l, f := planGrantBlock(gs, partitionDBGrants, dbSignature, dbPhrase, "database")
		return r, l, f
	}

	t.Run("all uniform PUBLIC defaults collapse to one line, no table", func(t *testing.T) {
		rows, line, footer := plan([]catalog.DBGrant{
			{Database: "a", Connect: true, Temp: true, PublicPrivs: pub},
			{Database: "b", Connect: true, Temp: true, PublicPrivs: pub},
			{Database: "c", Connect: true, Temp: true, PublicPrivs: pub},
		})
		if len(rows) != 0 || footer != "" {
			t.Fatalf("expected no table, got rows=%v footer=%q", rows, footer)
		}
		if !strings.Contains(line, "all 3 databases") {
			t.Errorf("line = %q, want it to name the count once", line)
		}
		// The point of the collapse: say that none of it is about this role.
		if !strings.Contains(line, "nothing specific to this role") {
			t.Errorf("line = %q, want it to say nothing is role-specific", line)
		}
	})

	t.Run("two reads as \"both\", not \"all 2\"", func(t *testing.T) {
		_, line, _ := plan([]catalog.DBGrant{
			{Database: "a", Connect: true, PublicPrivs: pub},
			{Database: "b", Connect: true, PublicPrivs: pub},
		})
		if !strings.Contains(line, "both databases") {
			t.Errorf("line = %q, want \"both databases\"", line)
		}
	})

	t.Run("an exception is tabled, the rest collapse into a footer", func(t *testing.T) {
		rows, line, footer := plan([]catalog.DBGrant{
			{Database: "a", Connect: true, Temp: true, PublicPrivs: pub},
			{Database: "b", Connect: true, Temp: true, PublicPrivs: pub},
			{Database: "shop", Connect: true}, // granted specifically — the interesting row
		})
		if line != "" {
			t.Fatalf("a set with an exception should not collapse wholesale, got %q", line)
		}
		if len(rows) != 1 || rows[0].Database != "shop" {
			t.Fatalf("only the role-specific database belongs in the table, got %v", rows)
		}
		if !strings.Contains(footer, "2 other databases") || !strings.Contains(footer, "a, b") {
			t.Errorf("footer = %q, want the remainder counted AND named", footer)
		}
	})

	t.Run("differing defaults are not summarized as if identical", func(t *testing.T) {
		rows, line, footer := plan([]catalog.DBGrant{
			{Database: "a", Connect: true, Temp: true, PublicPrivs: pub},
			{Database: "b", Connect: true, PublicPrivs: []string{"CONNECT"}},
		})
		if line != "" || footer != "" {
			t.Fatalf("objects granting different things must not collapse: line=%q footer=%q", line, footer)
		}
		if len(rows) != 2 {
			t.Fatalf("both rows should be tabled, got %v", rows)
		}
	})

	t.Run("a single generic object stays a table row", func(t *testing.T) {
		// Collapsing one object into a sentence is longer than the row it replaces.
		rows, line, footer := plan([]catalog.DBGrant{{Database: "a", Connect: true, PublicPrivs: pub}})
		if line != "" || footer != "" {
			t.Fatalf("one object should not collapse: line=%q footer=%q", line, footer)
		}
		if len(rows) != 1 {
			t.Fatalf("expected the single row, got %v", rows)
		}
	})

	t.Run("no generic group at all means a plain table", func(t *testing.T) {
		// Guards the empty-slice indexing the uniformity check would otherwise do.
		rows, line, footer := plan([]catalog.DBGrant{
			{Database: "a", Connect: true},
			{Database: "b", Owner: true, Connect: true},
		})
		if line != "" || footer != "" || len(rows) != 2 {
			t.Fatalf("all-specific set should table plainly: rows=%v line=%q footer=%q", rows, line, footer)
		}
	})
}

func TestPlanGrantBlock_Schemas(t *testing.T) {
	// Same machinery, the other type — schema `public` carries USAGE for PUBLIC by default.
	rows, _, line, footer := planGrantBlock([]catalog.SchemaGrant{
		{Schema: "public", Usage: true, PublicPrivs: []string{"USAGE"}},
		{Schema: "other", Usage: true, PublicPrivs: []string{"USAGE"}},
	}, partitionSchemaGrants, schemaSignature, schemaPhrase, "schema")
	if len(rows) != 0 || footer != "" {
		t.Fatalf("uniform schemas should collapse, got rows=%v footer=%q", rows, footer)
	}
	if !strings.Contains(line, "both schemas") {
		t.Errorf("line = %q, want it to collapse to one line", line)
	}
}

func TestAllHeldViaPublic(t *testing.T) {
	pub := []string{"CONNECT", "TEMPORARY"}
	// Ownership is always specific, even when the listed privileges all come from PUBLIC.
	if allHeldViaPublic(true, dbPrivCells(catalog.DBGrant{Connect: true}), pub) {
		t.Error("an owner is never generic")
	}
	if !allHeldViaPublic(false, dbPrivCells(catalog.DBGrant{Connect: true, Temp: true}), pub) {
		t.Error("CONNECT+TEMP entirely from PUBLIC is generic")
	}
	// One privilege PUBLIC lacks makes the whole object role-specific.
	if allHeldViaPublic(false, dbPrivCells(catalog.DBGrant{Connect: true, Create: true}), pub) {
		t.Error("a privilege PUBLIC lacks makes the object specific")
	}
	// Holding nothing is "no access", handled elsewhere — not "generic".
	if allHeldViaPublic(false, dbPrivCells(catalog.DBGrant{}), pub) {
		t.Error("holding nothing is not a generic grant")
	}
}
