package catalog

// Role detail. `get roles` answers "who exists"; DescribeRole answers the questions a
// single role actually raises: what can it reach, what does it own (i.e. what breaks if
// we drop it), and what is connected as it right now.
//
// Privileges are reported EFFECTIVE — has_*_privilege() resolves inherited grants and
// grants to PUBLIC, which is what "can this role read that table?" really means. Direct
// ACL entries are a different (and usually less useful) question; the renderer labels
// which one it's showing so the two are never confused.
//
// Every section is its own query so one unreadable catalog degrades that section rather
// than failing the command (D8). Anything skipped for lack of privilege lands in Notes.
//
// The role is always passed as an OID, cast explicitly ($1::oid). Both matter: the OID
// keeps the queries correct across a role rename, and without the cast Postgres infers
// the parameter's type from its first use — has_*_privilege() has a `name` overload, so
// it would pick that and then fail comparing `relowner = $1` (oid = name).

import (
	"context"
	"fmt"
	"strings"
)

// RoleDetail is everything the catalogs expose about one role.
type RoleDetail struct {
	Name        string `json:"name"`
	OID         uint32 `json:"oid"`
	Superuser   bool   `json:"superuser"`
	CreateDB    bool   `json:"create_db"`
	CreateRole  bool   `json:"create_role"`
	Login       bool   `json:"login"`
	Replication bool   `json:"replication"`
	BypassRLS   bool   `json:"bypass_rls"`
	Inherit     bool   `json:"inherit"` // NOINHERIT means memberships need an explicit SET ROLE
	ConnLimit   int64  `json:"conn_limit"`
	ValidUntil  string `json:"valid_until,omitempty"`

	// HasPassword is nil when the current role may not read pg_authid (that needs
	// superuser) — a Notes entry says so. Only presence is read, never the hash.
	HasPassword *bool `json:"has_password"`

	MemberOf  []RoleGrant   `json:"member_of,omitempty"`
	Members   []RoleGrant   `json:"members,omitempty"`
	Settings  []RoleSetting `json:"settings,omitempty"`
	Owns      []OwnedGroup  `json:"owns,omitempty"`
	Databases []DBGrant     `json:"database_privileges,omitempty"`
	Schemas   []SchemaGrant `json:"schema_privileges,omitempty"`

	// TableSummary is the per-schema rollup shown by default; TableGrants is the
	// per-relation enumeration, populated only when the caller asks for it.
	TableSummary []SchemaTableGrants `json:"table_privileges,omitempty"`
	TableGrants  []TableGrant        `json:"table_grants,omitempty"`

	DefaultACLs []DefaultACL  `json:"default_privileges,omitempty"`
	Sessions    []RoleSession `json:"sessions,omitempty"`

	// MaskedQueries reports that session query text was hidden because the current
	// role lacks pg_monitor — the same caveat `get activity` carries.
	MaskedQueries bool     `json:"masked_queries,omitempty"`
	Notes         []string `json:"notes,omitempty"`
}

// RoleGrant is one membership edge, in either direction.
type RoleGrant struct {
	Role  string `json:"role"`
	Admin bool   `json:"admin_option"` // WITH ADMIN OPTION: may grant this membership onward
}

// RoleSetting is one role-scoped GUC from ALTER ROLE ... SET. Database is "" when the
// setting applies in every database.
type RoleSetting struct {
	Database string `json:"database,omitempty"`
	Name     string `json:"name"`
	Value    string `json:"value"`
}

// OwnedGroup counts the objects this role owns, by kind, with a few example names.
// These are what REASSIGN OWNED / DROP OWNED have to clear before DROP ROLE succeeds.
type OwnedGroup struct {
	Kind    string   `json:"kind"`
	Count   int64    `json:"count"`
	Samples []string `json:"samples,omitempty"`
}

// DBGrant is one database's effective privileges for the role. PublicPrivs is what PUBLIC
// holds there — i.e. what EVERY role holds regardless of any grant to this one. Postgres
// grants CONNECT and TEMPORARY to PUBLIC on every database by default, so without this the
// CONNECT column reads "✓" for every role on every database and says nothing about the role.
type DBGrant struct {
	Database    string   `json:"database"`
	Owner       bool     `json:"owner"`
	Connect     bool     `json:"connect"`
	Create      bool     `json:"create"`
	Temp        bool     `json:"temp"`
	PublicPrivs []string `json:"public_privileges,omitempty"`
}

// SchemaGrant mirrors DBGrant for schemas. The `public` schema carries USAGE for PUBLIC by
// default (and, before PG15, CREATE too), which is the same trap one level down.
type SchemaGrant struct {
	Schema      string   `json:"schema"`
	Owner       bool     `json:"owner"`
	Usage       bool     `json:"usage"`
	Create      bool     `json:"create"`
	PublicPrivs []string `json:"public_privileges,omitempty"`
}

// SchemaTableGrants rolls up per-relation access to one schema: how many of its
// relations this role can read, write, and owns.
type SchemaTableGrants struct {
	Schema string `json:"schema"`
	Total  int64  `json:"total"`
	Select int64  `json:"select"`
	Insert int64  `json:"insert"`
	Update int64  `json:"update"`
	Delete int64  `json:"delete"`
	Owned  int64  `json:"owned"`
}

// TableGrant is one relation this role holds at least one privilege on.
type TableGrant struct {
	Schema     string   `json:"schema"`
	Name       string   `json:"name"`
	Kind       string   `json:"kind"`
	Owner      bool     `json:"owner"`
	Privileges []string `json:"privileges"`
}

// DefaultACL is one ALTER DEFAULT PRIVILEGES entry touching this role — either set BY it
// (so objects it creates carry these grants) or granting TO it (so objects others create
// become reachable). The second kind is the usual answer to "why can't the app role see
// the table that was just created?".
type DefaultACL struct {
	Schema     string   `json:"schema,omitempty"` // "" = every schema
	ObjectType string   `json:"object_type"`      // tables | sequences | functions | types | schemas
	Grantor    string   `json:"grantor"`
	Grantee    string   `json:"grantee"` // "PUBLIC" for a grant to PUBLIC
	Privileges []string `json:"privileges"`
}

// RoleSession is one connection currently open as this role. Postgres keeps no
// last-login history, so "when did this role last connect?" is unanswerable from the
// catalogs — this is the live picture only.
type RoleSession struct {
	PID         int32   `json:"pid"`
	Database    string  `json:"database"`
	ClientAddr  string  `json:"client_addr"`
	AppName     string  `json:"application_name,omitempty"`
	State       string  `json:"state"`
	DurationSec float64 `json:"duration_sec"` // seconds in the current state; -1 = unknown
	Query       string  `json:"query,omitempty"`
}

// DescribeRole gathers everything above for one role. withTableGrants adds the
// per-relation privilege enumeration, which costs a privilege check per relation — fine
// for a normal schema, deliberately opt-in for a very large one.
func DescribeRole(ctx context.Context, q Querier, name string, withTableGrants bool) (*RoleDetail, error) {
	d, err := describeRoleAttributes(ctx, q, name)
	if err != nil {
		return nil, err
	}

	// Each section is independent: record the failure as a note and keep going, so a
	// locked-down catalog costs one section rather than the whole command.
	section := func(label string, fn func() error) {
		if err := fn(); err != nil {
			d.Notes = append(d.Notes, fmt.Sprintf("%s unavailable: %v", label, err))
		}
	}
	section("password status", func() error { return d.loadPassword(ctx, q) })
	section("memberships", func() error { return d.loadMemberships(ctx, q) })
	section("role settings", func() error { return d.loadSettings(ctx, q) })
	section("owned objects", func() error { return d.loadOwned(ctx, q) })
	section("database privileges", func() error { return d.loadDatabaseGrants(ctx, q) })
	section("schema privileges", func() error { return d.loadSchemaGrants(ctx, q) })
	section("table privileges", func() error { return d.loadTableSummary(ctx, q) })
	if withTableGrants {
		section("per-table privileges", func() error { return d.loadTableGrants(ctx, q) })
	}
	section("default privileges", func() error { return d.loadDefaultACLs(ctx, q) })
	section("sessions", func() error { return d.loadSessions(ctx, q) })
	return d, nil
}

// buildRoleAttributesQuery reads one role's attributes. pg_roles (not pg_authid) so no
// password hash is ever selected; pg_* predefined roles are NOT excluded here — asking
// about pg_monitor is legitimate, and membership in pg_read_all_data is exactly the kind
// of thing this command exists to surface.
func buildRoleAttributesQuery() string {
	return `SELECT r.oid, r.rolname, r.rolsuper, r.rolcreatedb, r.rolcreaterole, r.rolcanlogin,
       r.rolreplication, r.rolbypassrls, r.rolinherit, r.rolconnlimit,
       COALESCE(r.rolvaliduntil::text, '')
FROM pg_catalog.pg_roles r
WHERE r.rolname = $1`
}

func describeRoleAttributes(ctx context.Context, q Querier, name string) (*RoleDetail, error) {
	rows, err := q.Query(ctx, buildRoleAttributesQuery(), name)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var d RoleDetail
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("role %q not found (list them with `pgdx get roles`)", name)
	}
	if err := rows.Scan(&d.OID, &d.Name, &d.Superuser, &d.CreateDB, &d.CreateRole, &d.Login,
		&d.Replication, &d.BypassRLS, &d.Inherit, &d.ConnLimit, &d.ValidUntil); err != nil {
		return nil, err
	}
	return &d, rows.Err()
}

// loadPassword records whether a password is set. Reading it needs SELECT on pg_authid
// (superuser), so the privilege is checked first rather than letting the query fail:
// pg_authid is a table, and Postgres checks table permissions at executor start, so a
// CASE guard in SQL would not prevent the error the way it does for a guarded function.
func (d *RoleDetail) loadPassword(ctx context.Context, q Querier) error {
	ok, err := scanOne[bool](ctx, q,
		`SELECT pg_catalog.has_table_privilege(current_user, 'pg_catalog.pg_authid', 'SELECT')`)
	if err != nil {
		return err
	}
	if !ok {
		d.Notes = append(d.Notes,
			"password status hidden — reading pg_authid requires superuser (pgdx never reads the hash itself)")
		return nil
	}
	set, err := scanOne[bool](ctx, q,
		`SELECT rolpassword IS NOT NULL FROM pg_catalog.pg_authid WHERE oid = $1::oid`, d.OID)
	if err != nil {
		return err
	}
	d.HasPassword = &set
	return nil
}

// buildRoleMembershipQuery returns the memberships of a role. Direction "of" lists the
// roles this one belongs to; "in" lists the roles that belong to it.
//
// Only admin_option is reported: PG16 added per-grant inherit/set options to
// pg_auth_members, but pgdx supports 13+, so the portable answer is the grant's ADMIN
// flag plus the role's own INHERIT attribute (rendered in the header).
func buildRoleMembershipQuery(direction string) string {
	self, other := "member", "roleid"
	if direction == "in" {
		self, other = "roleid", "member"
	}
	return `SELECT o.rolname, m.admin_option
FROM pg_catalog.pg_auth_members m
JOIN pg_catalog.pg_roles o ON o.oid = m.` + other + `
WHERE m.` + self + ` = $1::oid
ORDER BY o.rolname`
}

func (d *RoleDetail) loadMemberships(ctx context.Context, q Querier) error {
	load := func(direction string) ([]RoleGrant, error) {
		rows, err := q.Query(ctx, buildRoleMembershipQuery(direction), d.OID)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []RoleGrant
		for rows.Next() {
			var g RoleGrant
			if err := rows.Scan(&g.Role, &g.Admin); err != nil {
				return nil, err
			}
			out = append(out, g)
		}
		return out, rows.Err()
	}
	var err error
	if d.MemberOf, err = load("of"); err != nil {
		return err
	}
	d.Members, err = load("in")
	return err
}

// buildRoleSettingsQuery reads role-scoped GUCs (ALTER ROLE ... SET, optionally
// ALTER ROLE ... IN DATABASE ... SET). These override postgresql.conf for this role only
// and are invisible in `get settings`, which reports the current session — a role-scoped
// statement_timeout or search_path is a classic "works for me" cause.
func buildRoleSettingsQuery() string {
	return `SELECT COALESCE(d.datname, ''), c.cfg
FROM pg_catalog.pg_db_role_setting s
LEFT JOIN pg_catalog.pg_database d ON d.oid = s.setdatabase
CROSS JOIN LATERAL unnest(s.setconfig) AS c(cfg)
WHERE s.setrole = $1::oid
ORDER BY 1, 2`
}

func (d *RoleDetail) loadSettings(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, buildRoleSettingsQuery(), d.OID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var database, cfg string
		if err := rows.Scan(&database, &cfg); err != nil {
			return err
		}
		s := RoleSetting{Database: database, Name: cfg}
		if i := strings.IndexByte(cfg, '='); i >= 0 {
			s.Name, s.Value = cfg[:i], cfg[i+1:]
		}
		d.Settings = append(d.Settings, s)
	}
	return rows.Err()
}

// ownedSamples caps how many example names each owned-object kind carries.
const ownedSamples = 5

// buildOwnedObjectsQuery counts what a role owns, by kind, with a few sample names.
// Indexes and TOAST tables are omitted: their ownership follows the parent table, so
// listing them would just inflate the counts. Databases and tablespaces are included
// because they are cluster-wide and block DROP ROLE just as relations do.
func buildOwnedObjectsQuery() string {
	return `SELECT kind, count(*)::bigint, (array_agg(name ORDER BY name))[1:` + fmt.Sprint(ownedSamples) + `]
FROM (
  SELECT CASE c.relkind
           WHEN 'r' THEN 'table'
           WHEN 'p' THEN 'partitioned table'
           WHEN 'v' THEN 'view'
           WHEN 'm' THEN 'materialized view'
           WHEN 'S' THEN 'sequence'
           WHEN 'f' THEN 'foreign table'
         END AS kind,
         n.nspname || '.' || c.relname AS name
  FROM pg_catalog.pg_class c
  JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  WHERE c.relowner = $1::oid
    AND c.relkind IN ('r', 'p', 'v', 'm', 'S', 'f')
    AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
  UNION ALL
  SELECT 'schema', n.nspname
  FROM pg_catalog.pg_namespace n
  WHERE n.nspowner = $1::oid
    AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
  UNION ALL
  SELECT CASE p.prokind WHEN 'p' THEN 'procedure' ELSE 'function' END,
         n.nspname || '.' || p.proname
  FROM pg_catalog.pg_proc p
  JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
  WHERE p.proowner = $1::oid
    AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
  UNION ALL
  SELECT 'database', d.datname
  FROM pg_catalog.pg_database d
  WHERE d.datdba = $1::oid
  UNION ALL
  SELECT 'tablespace', t.spcname
  FROM pg_catalog.pg_tablespace t
  WHERE t.spcowner = $1::oid
) o
GROUP BY kind
ORDER BY count(*) DESC, kind`
}

func (d *RoleDetail) loadOwned(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, buildOwnedObjectsQuery(), d.OID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var g OwnedGroup
		if err := rows.Scan(&g.Kind, &g.Count, &g.Samples); err != nil {
			return err
		}
		d.Owns = append(d.Owns, g)
	}
	return rows.Err()
}

// publicPrivsExpr returns a subquery yielding the privileges PUBLIC holds on an object,
// which is the baseline every role gets for free. Two details make it correct:
//
//   - grantee 0 is the PUBLIC pseudo-role.
//   - a NULL acl column does NOT mean "no privileges", it means "built-in defaults" — for a
//     database that's CONNECT+TEMPORARY to PUBLIC. acldefault() materializes those, so the
//     overwhelmingly common case (nobody has touched the ACL) is reported accurately rather
//     than as an empty set.
func publicPrivsExpr(aclCol, kind, ownerCol string) string {
	return `ARRAY(SELECT a.privilege_type
             FROM pg_catalog.aclexplode(
                    COALESCE(` + aclCol + `, pg_catalog.acldefault('` + kind + `', ` + ownerCol + `))) a
             WHERE a.grantee = 0)`
}

// buildDatabaseGrantsQuery reports effective database-level privileges cluster-wide.
// has_database_privilege() is an ACL check, not a connection, so it answers for
// databases this session isn't attached to. Template databases are excluded.
func buildDatabaseGrantsQuery() string {
	return `SELECT d.datname, d.datdba = $1::oid,
       pg_catalog.has_database_privilege($1::oid, d.oid, 'CONNECT'),
       pg_catalog.has_database_privilege($1::oid, d.oid, 'CREATE'),
       pg_catalog.has_database_privilege($1::oid, d.oid, 'TEMPORARY'),
       ` + publicPrivsExpr("d.datacl", "d", "d.datdba") + `
FROM pg_catalog.pg_database d
WHERE NOT d.datistemplate
ORDER BY d.datname`
}

func (d *RoleDetail) loadDatabaseGrants(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, buildDatabaseGrantsQuery(), d.OID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var g DBGrant
		if err := rows.Scan(&g.Database, &g.Owner, &g.Connect, &g.Create, &g.Temp, &g.PublicPrivs); err != nil {
			return err
		}
		d.Databases = append(d.Databases, g)
	}
	return rows.Err()
}

// buildSchemaGrantsQuery reports effective schema privileges in the CONNECTED database
// (schemas are per-database). USAGE is the gate that makes table grants usable at all —
// a role with SELECT on a table but no USAGE on its schema still can't read it.
func buildSchemaGrantsQuery() string {
	return `SELECT n.nspname, n.nspowner = $1::oid,
       pg_catalog.has_schema_privilege($1::oid, n.oid, 'USAGE'),
       pg_catalog.has_schema_privilege($1::oid, n.oid, 'CREATE'),
       ` + publicPrivsExpr("n.nspacl", "n", "n.nspowner") + `
FROM pg_catalog.pg_namespace n
WHERE n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
ORDER BY n.nspname`
}

func (d *RoleDetail) loadSchemaGrants(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, buildSchemaGrantsQuery(), d.OID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var g SchemaGrant
		if err := rows.Scan(&g.Schema, &g.Owner, &g.Usage, &g.Create, &g.PublicPrivs); err != nil {
			return err
		}
		d.Schemas = append(d.Schemas, g)
	}
	return rows.Err()
}

// grantedRelkinds are the relation kinds that carry table-style privileges.
const grantedRelkinds = `('r', 'p', 'v', 'm', 'f')`

// buildTableSummaryQuery rolls per-relation privilege checks up to one row per schema —
// "SELECT on 42 of 50 relations" — which is the readable answer on any real schema and
// keeps the default output bounded by schema count rather than table count.
func buildTableSummaryQuery() string {
	return `SELECT n.nspname, count(*)::bigint,
       count(*) FILTER (WHERE pg_catalog.has_table_privilege($1::oid, c.oid, 'SELECT'))::bigint,
       count(*) FILTER (WHERE pg_catalog.has_table_privilege($1::oid, c.oid, 'INSERT'))::bigint,
       count(*) FILTER (WHERE pg_catalog.has_table_privilege($1::oid, c.oid, 'UPDATE'))::bigint,
       count(*) FILTER (WHERE pg_catalog.has_table_privilege($1::oid, c.oid, 'DELETE'))::bigint,
       count(*) FILTER (WHERE c.relowner = $1::oid)::bigint
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ` + grantedRelkinds + `
  AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
GROUP BY n.nspname
ORDER BY n.nspname`
}

func (d *RoleDetail) loadTableSummary(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, buildTableSummaryQuery(), d.OID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s SchemaTableGrants
		if err := rows.Scan(&s.Schema, &s.Total, &s.Select, &s.Insert, &s.Update, &s.Delete, &s.Owned); err != nil {
			return err
		}
		d.TableSummary = append(d.TableSummary, s)
	}
	return rows.Err()
}

// tableGrantLimit bounds the per-relation enumeration. A truncated list is reported as a
// note rather than silently cut.
const tableGrantLimit = 2000

// buildTableGrantsQuery enumerates every relation the role holds at least one privilege
// on. The WHERE clause uses has_table_privilege's comma-list form — ONE string listing
// the privileges, true if ANY is held — so relations the role can't touch never reach the
// output. (Passing them as separate arguments instead looks for a 9-arity overload that
// doesn't exist.)
func buildTableGrantsQuery() string {
	const privs = `'SELECT, INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER'`
	return `SELECT n.nspname, c.relname,
       CASE c.relkind
         WHEN 'r' THEN 'table' WHEN 'p' THEN 'partitioned table' WHEN 'v' THEN 'view'
         WHEN 'm' THEN 'materialized view' WHEN 'f' THEN 'foreign table'
         ELSE c.relkind::text END,
       c.relowner = $1::oid,
       array_remove(ARRAY[
         CASE WHEN pg_catalog.has_table_privilege($1::oid, c.oid, 'SELECT') THEN 'SELECT' END,
         CASE WHEN pg_catalog.has_table_privilege($1::oid, c.oid, 'INSERT') THEN 'INSERT' END,
         CASE WHEN pg_catalog.has_table_privilege($1::oid, c.oid, 'UPDATE') THEN 'UPDATE' END,
         CASE WHEN pg_catalog.has_table_privilege($1::oid, c.oid, 'DELETE') THEN 'DELETE' END,
         CASE WHEN pg_catalog.has_table_privilege($1::oid, c.oid, 'TRUNCATE') THEN 'TRUNCATE' END,
         CASE WHEN pg_catalog.has_table_privilege($1::oid, c.oid, 'REFERENCES') THEN 'REFERENCES' END,
         CASE WHEN pg_catalog.has_table_privilege($1::oid, c.oid, 'TRIGGER') THEN 'TRIGGER' END
       ], NULL)
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ` + grantedRelkinds + `
  AND n.nspname !~ '^pg_' AND n.nspname <> 'information_schema'
  AND pg_catalog.has_table_privilege($1::oid, c.oid, ` + privs + `)
ORDER BY n.nspname, c.relname
LIMIT ` + fmt.Sprint(tableGrantLimit+1)
}

func (d *RoleDetail) loadTableGrants(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, buildTableGrantsQuery(), d.OID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var g TableGrant
		if err := rows.Scan(&g.Schema, &g.Name, &g.Kind, &g.Owner, &g.Privileges); err != nil {
			return err
		}
		d.TableGrants = append(d.TableGrants, g)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if len(d.TableGrants) > tableGrantLimit {
		d.TableGrants = d.TableGrants[:tableGrantLimit]
		d.Notes = append(d.Notes, fmt.Sprintf(
			"per-table privileges truncated at %d relations — narrow the question with `pgdx get tables --schema`",
			tableGrantLimit))
	}
	return nil
}

// buildDefaultACLQuery reads ALTER DEFAULT PRIVILEGES entries touching this role, in
// both directions: set BY it (defaclrole) and granting TO it (grantee). grantee 0 is
// PUBLIC, which pg_get_userbyid would render as "unknown (OID=0)".
func buildDefaultACLQuery() string {
	return `SELECT COALESCE(n.nspname, ''),
       CASE d.defaclobjtype
         WHEN 'r' THEN 'tables' WHEN 'S' THEN 'sequences' WHEN 'f' THEN 'functions'
         WHEN 'T' THEN 'types' WHEN 'n' THEN 'schemas'
         ELSE d.defaclobjtype::text END,
       pg_catalog.pg_get_userbyid(d.defaclrole),
       CASE WHEN a.grantee = 0 THEN 'PUBLIC' ELSE pg_catalog.pg_get_userbyid(a.grantee) END,
       array_agg(a.privilege_type ORDER BY a.privilege_type)
FROM pg_catalog.pg_default_acl d
LEFT JOIN pg_catalog.pg_namespace n ON n.oid = d.defaclnamespace
CROSS JOIN LATERAL pg_catalog.aclexplode(d.defaclacl) a
WHERE d.defaclrole = $1::oid OR a.grantee = $1::oid
GROUP BY 1, 2, 3, 4
ORDER BY 1, 2, 3, 4`
}

func (d *RoleDetail) loadDefaultACLs(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, buildDefaultACLQuery(), d.OID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var a DefaultACL
		if err := rows.Scan(&a.Schema, &a.ObjectType, &a.Grantor, &a.Grantee, &a.Privileges); err != nil {
			return err
		}
		d.DefaultACLs = append(d.DefaultACLs, a)
	}
	return rows.Err()
}

// buildRoleSessionsQuery lists the connections currently open as this role. DURATION is
// time in the CURRENT state (query runtime when active, time parked otherwise) — the
// same honest metric `get activity` uses. usesysid, not usename, so a renamed role still
// matches, and only client backends — a background worker like the logical replication
// launcher reports the bootstrap superuser's usesysid but is not a session anyone opened.
// pgdx's own backend IS included: if you are describing the role you connected as, hiding
// it would misreport the count.
//
// clock_timestamp(), not now(): now() is the transaction timestamp, which for pgdx's own
// row is the same instant as its query_start and can land microseconds on either side of
// it — yielding a negative duration that renders as "unknown". `get activity` doesn't hit
// this because it excludes its own backend; this query includes it deliberately.
func buildRoleSessionsQuery() string {
	const durExpr = "(clock_timestamp() - CASE WHEN state = 'active' THEN query_start ELSE state_change END)"
	return `SELECT pid, COALESCE(datname, ''), COALESCE(host(client_addr), 'local'),
       COALESCE(application_name, ''), COALESCE(state, ''),
       COALESCE(EXTRACT(epoch FROM ` + durExpr + `), -1)::float8,
       COALESCE(query, '')
FROM pg_catalog.pg_stat_activity
WHERE usesysid = $1::oid
  AND backend_type = 'client backend'
ORDER BY COALESCE(EXTRACT(epoch FROM ` + durExpr + `), 0) DESC, pid`
}

// maskedQuery is what pg_stat_activity.query holds for a backend the current role may not
// inspect — Postgres substitutes this literal rather than returning NULL.
const maskedQuery = "<insufficient privilege>"

func (d *RoleDetail) loadSessions(ctx context.Context, q Querier) error {
	rows, err := q.Query(ctx, buildRoleSessionsQuery(), d.OID)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s RoleSession
		if err := rows.Scan(&s.PID, &s.Database, &s.ClientAddr, &s.AppName, &s.State,
			&s.DurationSec, &s.Query); err != nil {
			return err
		}
		d.Sessions = append(d.Sessions, s)
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Without pg_monitor, pg_stat_activity masks other roles' rows: state/duration come back
	// NULL and query is replaced with the literal maskedQuery sentinel. Lacking the
	// privilege isn't enough to warn — describing your OWN role shows full text regardless —
	// so require an actually-masked row too (D8).
	masked := false
	for _, s := range d.Sessions {
		if s.Query == maskedQuery || s.Query == "" {
			masked = true
			break
		}
	}
	if masked {
		ok, err := HasMonitorPrivilege(ctx, q)
		if err != nil {
			return err
		}
		d.MaskedQueries = !ok
	}
	return nil
}

// scanOne runs a query expected to yield a single scalar and returns it (the zero value
// if there are no rows).
func scanOne[T any](ctx context.Context, q Querier, sql string, args ...any) (T, error) {
	var v T
	rows, err := q.Query(ctx, sql, args...)
	if err != nil {
		return v, err
	}
	defer rows.Close()
	if rows.Next() {
		if err := rows.Scan(&v); err != nil {
			return v, err
		}
	}
	return v, rows.Err()
}

// PredefinedRoleWarning returns a short reason why membership in a predefined pg_* role
// matters, or "" when it carries no outsized power. The sets are the ones `pgdx audit`
// flags, so a membership called out here is the same finding audit would report.
func PredefinedRoleWarning(role string) string {
	switch {
	case rceRoles[role]:
		return "host-level power: shell/file access"
	case dataRoles[role]:
		return "bypasses per-table GRANTs"
	}
	return ""
}

const listRoleNamesSQL = `SELECT rolname
FROM pg_catalog.pg_roles
ORDER BY (rolname ~ '^pg_'), rolname`

// ListRoleNames lists role names for shell completion. Predefined pg_* roles come last
// so the roles an operator actually manages surface first.
func ListRoleNames(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.Query(ctx, listRoleNamesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
