package catalog

import (
	"strings"
	"testing"
)

// roleOIDQueries are every role-detail query that takes the role OID as $1. They all
// share two invariants worth locking down, so they're checked as a set.
func roleOIDQueries() map[string]string {
	return map[string]string{
		"member_of":     buildRoleMembershipQuery("of"),
		"members":       buildRoleMembershipQuery("in"),
		"settings":      buildRoleSettingsQuery(),
		"owned":         buildOwnedObjectsQuery(),
		"database":      buildDatabaseGrantsQuery(),
		"schema":        buildSchemaGrantsQuery(),
		"table summary": buildTableSummaryQuery(),
		"table grants":  buildTableGrantsQuery(),
		"default privs": buildDefaultACLQuery(),
		"role sessions": buildRoleSessionsQuery(),
	}
}

// The role parameter must be cast to oid at every site. Without the cast Postgres infers
// $1's type from its first use, and has_*_privilege() has a `name` overload — picking it
// makes a later `relowner = $1` fail with "operator does not exist: oid = name".
func TestRoleQueriesCastParamToOID(t *testing.T) {
	for name, sql := range roleOIDQueries() {
		if !strings.Contains(sql, "$1::oid") {
			t.Errorf("%s query does not cast $1 to oid:\n%s", name, sql)
		}
		// Every $1 must carry the cast, not just the first one.
		for _, frag := range strings.Split(sql, "$1")[1:] {
			if !strings.HasPrefix(frag, "::oid") {
				t.Errorf("%s query has an uncast $1:\n%s", name, sql)
				break
			}
		}
	}
}

// The role is never interpolated — it arrives as a parameter, so a role named
// `'; DROP ...` is inert.
func TestRoleQueriesAreParameterized(t *testing.T) {
	all := roleOIDQueries()
	all["attributes"] = buildRoleAttributesQuery()
	for name, sql := range all {
		if !strings.Contains(sql, "$1") {
			t.Errorf("%s query does not reference $1 (is the role interpolated?):\n%s", name, sql)
		}
	}
}

func TestBuildRoleAttributesQuery(t *testing.T) {
	sql := buildRoleAttributesQuery()
	// pg_roles, never pg_authid: the attributes query must not be able to read a hash.
	if !strings.Contains(sql, "pg_catalog.pg_roles") {
		t.Fatalf("attributes should come from pg_roles:\n%s", sql)
	}
	if strings.Contains(sql, "pg_authid") || strings.Contains(sql, "rolpassword") {
		t.Fatalf("attributes query must not touch pg_authid/rolpassword:\n%s", sql)
	}
	// Matched by name here (that's what the user typed); the OID it yields drives the rest.
	if !strings.Contains(sql, "r.rolname = $1") {
		t.Fatalf("attributes query should match rolname = $1:\n%s", sql)
	}
	// Unlike `get roles`, predefined roles are NOT excluded — describing pg_monitor is valid.
	if strings.Contains(sql, "!~ '^pg_'") {
		t.Fatalf("describe role should not exclude predefined pg_* roles:\n%s", sql)
	}
	for _, want := range []string{"rolsuper", "rolbypassrls", "rolinherit", "rolconnlimit", "rolvaliduntil"} {
		if !strings.Contains(sql, want) {
			t.Errorf("attributes query missing %q:\n%s", want, sql)
		}
	}
}

func TestBuildRoleMembershipQuery_Direction(t *testing.T) {
	of := buildRoleMembershipQuery("of")
	if !strings.Contains(of, "WHERE m.member = $1::oid") {
		t.Fatalf(`direction "of" should filter on m.member:\n%s`, of)
	}
	if !strings.Contains(of, "ON o.oid = m.roleid") {
		t.Fatalf(`direction "of" should join the granted role:\n%s`, of)
	}

	in := buildRoleMembershipQuery("in")
	if !strings.Contains(in, "WHERE m.roleid = $1::oid") {
		t.Fatalf(`direction "in" should filter on m.roleid:\n%s`, in)
	}
	if !strings.Contains(in, "ON o.oid = m.member") {
		t.Fatalf(`direction "in" should join the member role:\n%s`, in)
	}

	// PG16 added per-grant inherit/set options; pgdx supports 13+, so only the ADMIN flag
	// (present on every supported version) may be read.
	for _, sql := range []string{of, in} {
		if strings.Contains(sql, "inherit_option") || strings.Contains(sql, "set_option") {
			t.Fatalf("membership query uses a PG16-only column:\n%s", sql)
		}
	}
}

func TestBuildOwnedObjectsQuery(t *testing.T) {
	sql := buildOwnedObjectsQuery()
	// Indexes and TOAST tables follow their parent's ownership; counting them would just
	// inflate the totals.
	if strings.Contains(sql, "'i'") || strings.Contains(sql, "'t'") {
		t.Fatalf("owned-objects query should skip indexes/TOAST relations:\n%s", sql)
	}
	// The kinds that actually block DROP ROLE.
	for _, want := range []string{"pg_class", "pg_namespace", "pg_proc", "pg_database", "pg_tablespace"} {
		if !strings.Contains(sql, want) {
			t.Errorf("owned-objects query missing %q:\n%s", want, sql)
		}
	}
	if !strings.Contains(sql, "[1:5]") {
		t.Fatalf("owned-objects query should cap samples at %d:\n%s", ownedSamples, sql)
	}
}

// has_table_privilege's any-of form takes ONE comma-separated string. Passing the
// privileges as separate arguments looks for a 9-arity overload that doesn't exist, and
// the whole section fails.
func TestBuildTableGrantsQuery_UsesSingleCommaList(t *testing.T) {
	sql := buildTableGrantsQuery()
	if !strings.Contains(sql, "'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER'") {
		t.Fatalf("privilege filter should be one comma-separated string:\n%s", sql)
	}
	if strings.Contains(sql, "'SELECT', 'INSERT'") {
		t.Fatalf("privileges must not be passed as separate arguments:\n%s", sql)
	}
	// One over the limit, so a truncated result is detectable rather than silent.
	if !strings.Contains(sql, "LIMIT 2001") {
		t.Fatalf("query should fetch limit+1 rows to detect truncation:\n%s", sql)
	}
}

func TestBuildTableSummaryQuery_CountsPerSchema(t *testing.T) {
	sql := buildTableSummaryQuery()
	for _, priv := range []string{"'SELECT'", "'INSERT'", "'UPDATE'", "'DELETE'"} {
		if !strings.Contains(sql, "has_table_privilege($1::oid, c.oid, "+priv+")") {
			t.Errorf("summary query missing a %s check:\n%s", priv, sql)
		}
	}
	if !strings.Contains(sql, "GROUP BY n.nspname") {
		t.Fatalf("summary must roll up per schema (output bounded by schemas, not tables):\n%s", sql)
	}
}

func TestBuildDefaultACLQuery_BothDirections(t *testing.T) {
	sql := buildDefaultACLQuery()
	// Defaults set BY the role and defaults granting TO it are both relevant; the second is
	// the usual "why can't the app role see the new table?" answer.
	if !strings.Contains(sql, "d.defaclrole = $1::oid OR a.grantee = $1::oid") {
		t.Fatalf("default-ACL query should match grantor OR grantee:\n%s", sql)
	}
	// grantee 0 is PUBLIC; pg_get_userbyid(0) would render "unknown (OID=0)".
	if !strings.Contains(sql, "a.grantee = 0 THEN 'PUBLIC'") {
		t.Fatalf("default-ACL query should render grantee 0 as PUBLIC:\n%s", sql)
	}
}

func TestBuildRoleSessionsQuery(t *testing.T) {
	sql := buildRoleSessionsQuery()
	// usesysid, not usename: a renamed role still matches its own backends.
	if !strings.Contains(sql, "WHERE usesysid = $1::oid") {
		t.Fatalf("sessions query should filter on usesysid:\n%s", sql)
	}
	// clock_timestamp(), not now(): now() is the transaction timestamp and can sit
	// microseconds after query_start for pgdx's own row, yielding a negative duration.
	if !strings.Contains(sql, "clock_timestamp()") {
		t.Fatalf("sessions duration should use clock_timestamp():\n%s", sql)
	}
	if strings.Contains(sql, "now()") {
		t.Fatalf("sessions duration must not use now():\n%s", sql)
	}
	// Duration is time in the CURRENT state, so an idle pool connection isn't reported as
	// a long-running query.
	if !strings.Contains(sql, "CASE WHEN state = 'active' THEN query_start ELSE state_change END") {
		t.Fatalf("duration should measure time in the current state:\n%s", sql)
	}
}

func TestPredefinedRoleWarning(t *testing.T) {
	for _, tc := range []struct {
		role    string
		wantHit bool
	}{
		{"pg_execute_server_program", true},
		{"pg_read_server_files", true},
		{"pg_read_all_data", true},
		{"pg_write_all_data", true},
		{"pg_monitor", false},        // diagnostic-only — pgdx recommends it
		{"pg_read_all_stats", false}, // ditto
		{"app_user", false},          // an ordinary role
	} {
		got := PredefinedRoleWarning(tc.role)
		if (got != "") != tc.wantHit {
			t.Errorf("PredefinedRoleWarning(%q) = %q, want hit=%v", tc.role, got, tc.wantHit)
		}
	}
}

func TestListRoleNamesQueryOrdersPredefinedLast(t *testing.T) {
	// Not a builder, so assert on the behaviour we care about via the shared helper the
	// completer calls: predefined roles must sort after the ones an operator manages.
	sql := listRoleNamesSQL
	if !strings.Contains(sql, "ORDER BY (rolname ~ '^pg_'), rolname") {
		t.Fatalf("role-name completion should sort pg_* roles last:\n%s", sql)
	}
}

// Background workers (the logical replication launcher, for one) report the bootstrap
// superuser's usesysid but are not sessions anyone opened — counting them would inflate
// "sessions open now" on every superuser.
func TestBuildRoleSessionsQuery_ClientBackendsOnly(t *testing.T) {
	if !strings.Contains(buildRoleSessionsQuery(), "backend_type = 'client backend'") {
		t.Fatalf("sessions query should exclude background workers:\n%s", buildRoleSessionsQuery())
	}
}

// Postgres grants CONNECT+TEMPORARY to PUBLIC on every database and USAGE on schema
// public, so has_*_privilege() returns true for every role. Reporting that as the role's
// own privilege makes the column useless — these queries must also read what PUBLIC holds.
func TestPublicPrivsExpr(t *testing.T) {
	expr := publicPrivsExpr("d.datacl", "d", "d.datdba")
	// grantee 0 is the PUBLIC pseudo-role.
	if !strings.Contains(expr, "a.grantee = 0") {
		t.Errorf("should select the PUBLIC grantee:\n%s", expr)
	}
	// A NULL acl means "built-in defaults", NOT "no privileges" — acldefault() materializes
	// them, so the common untouched-ACL case is reported accurately instead of as empty.
	if !strings.Contains(expr, "acldefault") || !strings.Contains(expr, "COALESCE(d.datacl") {
		t.Errorf("a NULL acl must fall back to acldefault:\n%s", expr)
	}
}

func TestGrantQueriesReadPublicBaseline(t *testing.T) {
	for name, sql := range map[string]string{
		"databases": buildDatabaseGrantsQuery(),
		"schemas":   buildSchemaGrantsQuery(),
	} {
		if !strings.Contains(sql, "a.grantee = 0") {
			t.Errorf("%s query must read the PUBLIC baseline:\n%s", name, sql)
		}
	}
	// The right acldefault object-type code per catalog ('d' database, 'n' namespace).
	if !strings.Contains(buildDatabaseGrantsQuery(), "acldefault('d'") {
		t.Error("database query should use acldefault('d', ...)")
	}
	if !strings.Contains(buildSchemaGrantsQuery(), "acldefault('n'") {
		t.Error("schema query should use acldefault('n', ...)")
	}
}
