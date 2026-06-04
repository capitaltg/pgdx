package catalog

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
)

// Severity ranks a security finding. The string values are the JSON contract
// (`-o json`) and the table-group headings.
type Severity string

const (
	SeverityCritical Severity = "critical" // exploitable now / silently broken protection
	SeverityWarning  Severity = "warning"  // weakens the security posture; fix when you can
	SeverityInfo     Severity = "info"     // worth knowing; not necessarily wrong
)

// rank orders severities for display (critical first).
func (s Severity) rank() int {
	switch s {
	case SeverityCritical:
		return 0
	case SeverityWarning:
		return 1
	default:
		return 2
	}
}

// Finding categories. Security findings weaken the security posture; reliability
// findings risk data durability, bloat, or wraparound — the operational counterpart.
const (
	CategorySecurity    = "security"
	CategoryReliability = "reliability"
)

// SecurityFinding is one issue surfaced by `pgdx audit`. Check is a stable id
// (e.g. "superuser-roles") so JSON consumers can match on it without parsing prose.
// Category splits security from reliability findings in the output.
type SecurityFinding struct {
	Check       string   `json:"check"`
	Category    string   `json:"category"`
	Severity    Severity `json:"severity"`
	Title       string   `json:"title"`
	Detail      string   `json:"detail"`
	Remediation string   `json:"remediation,omitempty"`
}

// SkippedCheck records a check pgdx could not run — almost always because the
// connected role lacks the privilege to read the relevant catalog (e.g.
// pg_hba_file_rules needs superuser or pg_read_all_settings). Surfacing the skip
// is the point: a security tool that silently omits a check reads as "you're
// fine" when it simply couldn't look.
type SkippedCheck struct {
	Check  string `json:"check"`
	Reason string `json:"reason"`
}

// SecurityAudit is the full result of `pgdx audit`: the findings, what was
// skipped, and how many checks actually completed.
type SecurityAudit struct {
	Database string            `json:"database"`
	Checks   int               `json:"checks"` // checks that completed (excludes skipped)
	Findings []SecurityFinding `json:"findings"`
	Skipped  []SkippedCheck    `json:"skipped,omitempty"`
}

// Counts tallies findings by severity (for the summary line).
func (a *SecurityAudit) Counts() (critical, warning, info int) {
	for _, f := range a.Findings {
		switch f.Severity {
		case SeverityCritical:
			critical++
		case SeverityWarning:
			warning++
		default:
			info++
		}
	}
	return
}

// HasAtLeast reports whether any finding is at or above the given severity — the
// signal `pgdx audit --exit-code` turns into a non-zero exit for CI gating.
func (a *SecurityAudit) HasAtLeast(min Severity) bool {
	for _, f := range a.Findings {
		if f.Severity.rank() <= min.rank() {
			return true
		}
	}
	return false
}

// securityCheck is one audit probe. It returns findings (possibly none), or a
// non-nil *SkippedCheck when it lacked the privilege to run. A returned error is
// a genuine failure and aborts the audit. category tags every finding the probe
// produces, so the output can group security and reliability separately.
type securityCheck struct {
	id       string
	category string
	run      func(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error)
}

// AuditSecurity runs pgdx's hardening checks against the connected database and
// returns the findings sorted by severity. It is read-only. Checks that hit a
// permission error degrade to a SkippedCheck rather than failing the whole audit.
//
// Scope note: pgdx connects as a client, so it audits what the catalogs expose —
// roles, object privileges, RLS, and server GUCs. It cannot read postgresql.conf
// or (without elevated privilege) pg_hba.conf, so the authentication check is
// best-effort and skips cleanly when the role can't see pg_hba_file_rules.
func AuditSecurity(ctx context.Context, q Querier) (*SecurityAudit, error) {
	checks := []securityCheck{
		{"superuser-roles", CategorySecurity, checkSuperuserRoles},
		{"privileged-roles", CategorySecurity, checkPrivilegedRoles},
		{"public-schema", CategorySecurity, checkPublicSchema},
		{"schema-public-create", CategorySecurity, checkSchemaPublicCreate},
		{"rls-disabled", CategorySecurity, checkRLSDisabled},
		{"password-encryption", CategorySecurity, checkPasswordEncryption},
		{"ssl", CategorySecurity, checkSSL},
		{"session-ssl", CategorySecurity, checkSessionSSL},
		{"hba-auth", CategorySecurity, checkHBAAuth},
		{"logging", CategorySecurity, checkLogging},
		{"untrusted-languages", CategorySecurity, checkUntrustedLanguages},
		{"durability-settings", CategoryReliability, checkDurabilitySettings},
		{"autovacuum-config", CategoryReliability, checkAutovacuumConfig},
		{"checkpoint-pressure", CategoryReliability, checkCheckpointPressure},
	}

	a := &SecurityAudit{}
	for _, c := range checks {
		findings, skip, err := c.run(ctx, q)
		if err != nil {
			return nil, fmt.Errorf("audit check %q: %w", c.id, err)
		}
		if skip != nil {
			a.Skipped = append(a.Skipped, *skip)
			continue
		}
		a.Checks++
		for i := range findings {
			findings[i].Category = c.category
		}
		a.Findings = append(a.Findings, findings...)
	}

	// Group security before reliability, and within each, most-severe first.
	sort.SliceStable(a.Findings, func(i, j int) bool {
		if ri, rj := categoryRank(a.Findings[i].Category), categoryRank(a.Findings[j].Category); ri != rj {
			return ri < rj
		}
		return a.Findings[i].Severity.rank() < a.Findings[j].Severity.rank()
	})
	return a, nil
}

// categoryRank orders categories for display (security first, then reliability).
func categoryRank(c string) int {
	if c == CategoryReliability {
		return 1
	}
	return 0
}

// isPermissionDenied reports whether err is a Postgres "insufficient_privilege"
// (SQLSTATE 42501) — the signal to skip a check rather than fail the audit.
func isPermissionDenied(err error) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "42501"
	}
	return false
}

// ---- superuser / dangerous role attributes ----

// superuserRolesQuery lists LOGIN-capable roles carrying cluster-dominating
// attributes (SUPERUSER or BYPASSRLS). Login roles are the attack surface — a
// non-login role can't authenticate directly. Built-in pg_* roles are excluded.
// oid = 10 is the bootstrap superuser (the cluster owner), flagged so output can
// note it's expected rather than implying it's a misconfiguration.
func superuserRolesQuery() string {
	return `SELECT r.rolname, r.rolsuper, r.rolbypassrls, (r.oid = 10) AS bootstrap
FROM pg_catalog.pg_roles r
WHERE r.rolcanlogin AND r.rolname !~ '^pg_'
  AND (r.rolsuper OR r.rolbypassrls)
ORDER BY r.rolsuper DESC, r.rolname`
}

// managedSuperuserRoles maps cloud-provider-owned superuser role names to a human
// label. On managed Postgres the real superuser belongs to the provider — you
// can't log in as it, alter it, or drop it (e.g. AWS RDS's rdsadmin, which is also
// the bootstrap superuser) — so flagging it as a risk is noise. The role *you*
// create on those services is granted a restricted group (rds_superuser /
// cloudsqlsuperuser / azure_pg_admin), not the SUPERUSER attribute, so it never
// trips this check.
var managedSuperuserRoles = map[string]string{
	"rdsadmin":        "AWS RDS-managed",
	"cloudsqladmin":   "GCP Cloud SQL-managed",
	"azuresu":         "Azure-managed",
	"azure_superuser": "Azure-managed",
}

// classifySuperuser annotates a superuser role and reports whether it's an
// "expected" admin account — a cloud provider's managed superuser or the
// cluster's bootstrap role — versus a user-created one. Expected accounts are
// reported at info (you should know they exist); user-created superusers are the
// actionable warning.
func classifySuperuser(name string, bootstrap bool) (label string, expected bool) {
	if provider, ok := managedSuperuserRoles[name]; ok {
		return fmt.Sprintf("%s (%s)", name, provider), true
	}
	if bootstrap {
		return name + " (bootstrap)", true
	}
	return name, false
}

func checkSuperuserRoles(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	rows, err := q.Query(ctx, superuserRolesQuery())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var userSupers, expectedSupers, bypass []string
	for rows.Next() {
		var name string
		var isSuper, isBypass, bootstrap bool
		if err := rows.Scan(&name, &isSuper, &isBypass, &bootstrap); err != nil {
			return nil, nil, err
		}
		if isSuper {
			label, expected := classifySuperuser(name, bootstrap)
			if expected {
				expectedSupers = append(expectedSupers, label)
			} else {
				userSupers = append(userSupers, label)
			}
		} else if isBypass { // BYPASSRLS without SUPERUSER — superusers bypass RLS anyway
			bypass = append(bypass, name)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var out []SecurityFinding
	if len(userSupers) > 0 {
		out = append(out, SecurityFinding{
			Check:    "superuser-roles",
			Severity: SeverityWarning,
			Title:    fmt.Sprintf("%d login role%s with SUPERUSER", len(userSupers), plural(len(userSupers))),
			Detail: "Login-capable superusers: " + strings.Join(userSupers, ", ") +
				". A superuser bypasses every permission check; a compromised one owns the cluster.",
			Remediation: "Reserve SUPERUSER for the bootstrap/maintenance role. Grant application roles only the privileges they need (and pg_monitor/pg_read_all_stats for diagnostics).",
		})
	}
	if len(expectedSupers) > 0 {
		out = append(out, SecurityFinding{
			Check:    "expected-superusers",
			Severity: SeverityInfo,
			Title:    fmt.Sprintf("%d expected superuser account%s", len(expectedSupers), plural(len(expectedSupers))),
			Detail: "Built-in or provider-managed superusers: " + strings.Join(expectedSupers, ", ") +
				". These are the cluster's maintenance / cloud-managed admin accounts — expected, and on a managed service not something you can change.",
		})
	}
	if len(bypass) > 0 {
		out = append(out, SecurityFinding{
			Check:    "bypassrls-roles",
			Severity: SeverityWarning,
			Title:    fmt.Sprintf("%d login role%s with BYPASSRLS", len(bypass), plural(len(bypass))),
			Detail: "Roles that bypass row-level security: " + strings.Join(bypass, ", ") +
				". Row-level security policies do not apply to these roles.",
			Remediation: "Remove BYPASSRLS unless the role genuinely needs to see all rows: ALTER ROLE <name> NOBYPASSRLS.",
		})
	}
	return out, nil, nil
}

// ---- "superuser-lite" predefined-role membership ----

// rceRoles are the predefined roles (PostgreSQL 11+) that grant near-superuser
// power over the host: COPY ... PROGRAM runs arbitrary shell commands as the
// postgres OS user, and the file roles read/write arbitrary server files.
var rceRoles = map[string]bool{
	"pg_execute_server_program": true,
	"pg_read_server_files":      true,
	"pg_write_server_files":     true,
}

// dataRoles are the predefined roles (PostgreSQL 14+) that read or write every
// table's data, bypassing per-object GRANTs — serious, but data-scoped rather
// than host-level, so they rank a notch below the RCE roles.
var dataRoles = map[string]bool{
	"pg_read_all_data":  true,
	"pg_write_all_data": true,
}

// privilegedRolesQuery lists login roles (excluding superusers, who already have
// everything) that are members — directly OR transitively, via pg_has_role with
// MEMBER — of the dangerous predefined roles. The JOIN only references predefined
// roles that actually exist in this cluster, so it stays valid on versions
// predating pg_read_all_data without a version gate.
func privilegedRolesQuery() string {
	return `SELECT r.rolname, g.rolname
FROM pg_catalog.pg_roles r
JOIN pg_catalog.pg_roles g
  ON g.rolname IN ('pg_execute_server_program','pg_read_server_files','pg_write_server_files',
                   'pg_read_all_data','pg_write_all_data')
 AND pg_catalog.pg_has_role(r.oid, g.oid, 'MEMBER')
WHERE r.rolcanlogin AND r.rolname !~ '^pg_' AND NOT r.rolsuper
ORDER BY r.rolname, g.rolname`
}

// classifyPrivilegedRoles turns a role→predefined-roles map into the two finding
// lists. A role with any RCE-class membership is reported only there (if you can
// run shell commands, "can also read all data" is noise); roles with just
// data-access membership form the warning list.
func classifyPrivilegedRoles(memberships map[string][]string) (rce, data []string) {
	// Stable order so output and tests are deterministic.
	names := make([]string, 0, len(memberships))
	for name := range memberships {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		var hasRCE, hasData []string
		for _, role := range memberships[name] {
			switch {
			case rceRoles[role]:
				hasRCE = append(hasRCE, role)
			case dataRoles[role]:
				hasData = append(hasData, role)
			}
		}
		if len(hasRCE) > 0 {
			rce = append(rce, fmt.Sprintf("%s (%s)", name, strings.Join(hasRCE, ", ")))
		} else if len(hasData) > 0 {
			data = append(data, fmt.Sprintf("%s (%s)", name, strings.Join(hasData, ", ")))
		}
	}
	return rce, data
}

func checkPrivilegedRoles(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	rows, err := q.Query(ctx, privilegedRolesQuery())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	memberships := map[string][]string{}
	for rows.Next() {
		var role, memberOf string
		if err := rows.Scan(&role, &memberOf); err != nil {
			return nil, nil, err
		}
		memberships[role] = append(memberships[role], memberOf)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	rce, data := classifyPrivilegedRoles(memberships)
	var out []SecurityFinding
	if len(rce) > 0 {
		out = append(out, SecurityFinding{
			Check:    "privileged-roles",
			Severity: SeverityCritical,
			Title:    fmt.Sprintf("%d login role%s with server program/file access", len(rce), plural(len(rce))),
			Detail: "These roles are not superusers but hold a predefined role that grants host-level power (shell execution via COPY ... PROGRAM, or reading/writing arbitrary server files): " +
				strings.Join(rce, "; ") + ". This is effectively superuser-equivalent.",
			Remediation: "Revoke the membership unless it is genuinely required: REVOKE pg_execute_server_program (etc.) FROM <role>.",
		})
	}
	if len(data) > 0 {
		out = append(out, SecurityFinding{
			Check:    "data-access-roles",
			Severity: SeverityWarning,
			Title:    fmt.Sprintf("%d login role%s can read/write all table data", len(data), plural(len(data))),
			Detail: "These roles hold a predefined role that bypasses per-table GRANTs across the whole database: " +
				strings.Join(data, "; ") + ".",
			Remediation: "Revoke the membership and grant only the specific tables the role needs: REVOKE pg_read_all_data (etc.) FROM <role>.",
		})
	}
	return out, nil, nil
}

// ---- public schema exposure ----

// publicSchemaQuery reports whether the public schema grants CREATE to PUBLIC
// (any role can create objects there) and whether its ACL is at the default
// (NULL) — needed because aclexplode(NULL) yields no rows, so a default ACL must
// be interpreted against the server version (see publicSchemaFinding).
func publicSchemaQuery() string {
	return `SELECT
  n.nspacl IS NULL AS acl_default,
  COALESCE(bool_or(a.privilege_type = 'CREATE'), false) AS public_create
FROM pg_catalog.pg_namespace n
LEFT JOIN LATERAL aclexplode(n.nspacl) a ON a.grantee = 0
WHERE n.nspname = 'public'
GROUP BY n.nspacl`
}

// publicSchemaFinding decides whether the public schema is world-writable.
//
//   - publicCreate true: we positively observed a CREATE grant to PUBLIC.
//   - aclDefault true on PostgreSQL < 15: initdb's historical default granted
//     CREATE on public to PUBLIC, so a default ACL is itself the risk. PostgreSQL
//     15+ dropped that default, so a default ACL there is fine.
//
// versionNum is server_version_num (e.g. 150000 for 15.0); 0 means unknown, in
// which case we only trust the positively-observed grant.
func publicSchemaFinding(aclDefault, publicCreate bool, versionNum int) *SecurityFinding {
	defaultIsOpen := aclDefault && versionNum > 0 && versionNum < 150000
	if !publicCreate && !defaultIsOpen {
		return nil
	}
	detail := "Any role that can connect can create objects (tables, functions) in the public schema."
	if defaultIsOpen && !publicCreate {
		detail += " This server is on the pre-15 default, which grants CREATE on public to PUBLIC."
	}
	return &SecurityFinding{
		Check:       "public-schema",
		Severity:    SeverityWarning,
		Title:       "public schema is writable by PUBLIC",
		Detail:      detail,
		Remediation: "REVOKE CREATE ON SCHEMA public FROM PUBLIC; (PostgreSQL 15+ does this by default).",
	}
}

func checkPublicSchema(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	version, err := ServerVersionNum(ctx, q)
	if err != nil {
		return nil, nil, err
	}
	rows, err := q.Query(ctx, publicSchemaQuery())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var aclDefault, publicCreate bool
	var found bool
	if rows.Next() {
		found = true
		if err := rows.Scan(&aclDefault, &publicCreate); err != nil {
			return nil, nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if !found { // no public schema (it was dropped) — nothing to flag
		return nil, nil, nil
	}
	if f := publicSchemaFinding(aclDefault, publicCreate, version); f != nil {
		return []SecurityFinding{*f}, nil, nil
	}
	return nil, nil, nil
}

// schemaPublicCreateQuery finds non-system, non-public schemas that grant CREATE
// to PUBLIC. Unlike the public schema, a user-created schema's default ACL (NULL)
// gives PUBLIC nothing, so a positive aclexplode grant is the whole signal — no
// version gating needed. USAGE-to-PUBLIC is deliberately NOT flagged: it's common
// and mostly harmless (you still need table grants to read anything), whereas
// CREATE lets any role plant objects in an application schema.
func schemaPublicCreateQuery() string {
	return `SELECT n.nspname
FROM pg_catalog.pg_namespace n
WHERE n.nspname !~ '^pg_'
  AND n.nspname <> 'information_schema'
  AND n.nspname <> 'public'
  AND EXISTS (
        SELECT 1 FROM aclexplode(n.nspacl) a
        WHERE a.grantee = 0 AND a.privilege_type = 'CREATE')
ORDER BY n.nspname`
}

func checkSchemaPublicCreate(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	rows, err := q.Query(ctx, schemaPublicCreateQuery())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var schemas []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, nil, err
		}
		schemas = append(schemas, name)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(schemas) == 0 {
		return nil, nil, nil
	}
	return []SecurityFinding{{
		Check:    "schema-public-create",
		Severity: SeverityWarning,
		Title:    fmt.Sprintf("%d schema%s grant CREATE to PUBLIC", len(schemas), plural(len(schemas))),
		Detail: "Any role that can connect can create objects in: " + strings.Join(schemas, ", ") +
			". Application schemas should rarely be writable by PUBLIC (every role in the cluster).",
		Remediation: "REVOKE CREATE ON SCHEMA <name> FROM PUBLIC; for each schema above.",
	}}, nil, nil
}

// ---- RLS policies defined but not enforced ----

// rlsDisabledQuery finds tables that have row-level security policies defined but
// RLS itself disabled (relrowsecurity = false). This is the dangerous case: the
// policies exist, look like protection in a schema dump, and do nothing. (A table
// with no policies at all is not flagged — RLS is opt-in and absent ≠ misconfig.)
func rlsDisabledQuery() string {
	return `SELECT n.nspname, c.relname,
       (SELECT count(*) FROM pg_catalog.pg_policy p WHERE p.polrelid = c.oid) AS policies
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE c.relkind IN ('r','p')
  AND NOT c.relrowsecurity
  AND EXISTS (SELECT 1 FROM pg_catalog.pg_policy p WHERE p.polrelid = c.oid)
ORDER BY n.nspname, c.relname`
}

func checkRLSDisabled(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	rows, err := q.Query(ctx, rlsDisabledQuery())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var tables []string
	for rows.Next() {
		var schema, name string
		var policies int
		if err := rows.Scan(&schema, &name, &policies); err != nil {
			return nil, nil, err
		}
		tables = append(tables, fmt.Sprintf("%s.%s", schema, name))
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(tables) == 0 {
		return nil, nil, nil
	}
	return []SecurityFinding{{
		Check:    "rls-disabled",
		Severity: SeverityCritical,
		Title:    fmt.Sprintf("RLS policies not enforced on %d table%s", len(tables), plural(len(tables))),
		Detail: "Row-level security policies are defined but RLS is disabled on: " + strings.Join(tables, ", ") +
			". The policies are inert — every row is visible regardless of them.",
		Remediation: "ALTER TABLE <schema>.<table> ENABLE ROW LEVEL SECURITY; for each table above.",
	}}, nil, nil
}

// ---- password encryption ----

func checkPasswordEncryption(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	val, err := queryOneString(ctx, q, "SELECT current_setting('password_encryption')")
	if err != nil {
		return nil, nil, err
	}
	if !strings.EqualFold(val, "md5") {
		return nil, nil, nil
	}
	return []SecurityFinding{{
		Check:       "password-encryption",
		Severity:    SeverityWarning,
		Title:       "password_encryption is md5",
		Detail:      "New and changed passwords are stored as md5 hashes, which are weak and deprecated. scram-sha-256 has been the recommended default since PostgreSQL 14.",
		Remediation: "Set password_encryption = 'scram-sha-256' and have users reset their passwords so the new hash takes effect.",
	}}, nil, nil
}

// ---- server-side SSL ----

func checkSSL(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	val, err := queryOneString(ctx, q, "SELECT current_setting('ssl')")
	if err != nil {
		return nil, nil, err
	}
	if strings.EqualFold(val, "on") {
		return nil, nil, nil
	}
	return []SecurityFinding{{
		Check:       "ssl",
		Severity:    SeverityWarning,
		Title:       "server SSL/TLS is disabled",
		Detail:      "The server has ssl = off, so client connections cannot be encrypted in transit. (Which connections are required to use SSL is governed by pg_hba.conf — see the hba-auth check.)",
		Remediation: "Enable TLS: set ssl = on with a server certificate, then require it for remote connections via hostssl rules in pg_hba.conf.",
	}}, nil, nil
}

// ---- current session encryption ----

// sessionSSLQuery reports whether pgdx's OWN connection is encrypted, plus enough
// about the peer to judge how much that matters: whether it's a local unix socket
// (client_addr IS NULL) and whether it's loopback. Reads only the current backend
// (pg_backend_pid), so it needs no elevated privilege.
func sessionSSLQuery() string {
	return `SELECT s.ssl,
       a.client_addr IS NULL AS local_socket,
       COALESCE(a.client_addr <<= inet '127.0.0.0/8' OR a.client_addr = inet '::1', false) AS loopback,
       COALESCE(host(a.client_addr), '') AS client_addr
FROM pg_catalog.pg_stat_ssl s
JOIN pg_catalog.pg_stat_activity a ON a.pid = s.pid
WHERE s.pid = pg_catalog.pg_backend_pid()`
}

// sessionSSLFinding judges the current connection's encryption. An unencrypted
// unix-socket connection is normal (no finding). Unencrypted loopback is low-risk
// (info). Unencrypted to a real remote host is the one that matters — the user
// may believe the server's SSL support means they're protected when this session
// isn't (warning).
func sessionSSLFinding(ssl, localSocket, loopback bool, clientAddr string) *SecurityFinding {
	if ssl || localSocket {
		return nil
	}
	if loopback {
		return &SecurityFinding{
			Check:    "session-ssl",
			Severity: SeverityInfo,
			Title:    "this pgdx connection is not encrypted (loopback)",
			Detail:   fmt.Sprintf("The current session to %s is not using SSL/TLS. Over loopback the exposure is limited, but the same client config against a remote host would also be unencrypted.", clientAddr),
		}
	}
	return &SecurityFinding{
		Check:       "session-ssl",
		Severity:    SeverityWarning,
		Title:       "this pgdx connection is not encrypted",
		Detail:      fmt.Sprintf("The current session to %s is not using SSL/TLS — credentials and data are sent in the clear over the network.", clientAddr),
		Remediation: "Connect with sslmode=require (or verify-full) in your DSN/connection string; ensure the server has ssl = on.",
	}
}

func checkSessionSSL(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	rows, err := q.Query(ctx, sessionSSLQuery())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var ssl, localSocket, loopback bool
	var clientAddr string
	var found bool
	if rows.Next() {
		found = true
		if err := rows.Scan(&ssl, &localSocket, &loopback, &clientAddr); err != nil {
			return nil, nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if !found { // no pg_stat_ssl row for this backend — nothing to judge
		return nil, nil, nil
	}
	if f := sessionSSLFinding(ssl, localSocket, loopback, clientAddr); f != nil {
		return []SecurityFinding{*f}, nil, nil
	}
	return nil, nil, nil
}

// ---- audit-trail logging ----

// checkLogging flags connection-audit logging being off. It's detection hygiene,
// not a vulnerability — and disabling it is a defensible (verbose) trade-off, or
// handled by an external auditor — so it's INFO, not a warning. log_lock_waits is
// deliberately out of scope here: that's observability/performance, covered by
// `pgdx status` / `get locks`, not a security signal.
func checkLogging(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	rows, err := q.Query(ctx, "SELECT current_setting('log_connections'), current_setting('log_disconnections')")
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var logConn, logDisconn string
	if rows.Next() {
		if err := rows.Scan(&logConn, &logDisconn); err != nil {
			return nil, nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var off []string
	if !isLogOn(logConn) {
		off = append(off, "log_connections")
	}
	if !isLogOn(logDisconn) {
		off = append(off, "log_disconnections")
	}
	if len(off) == 0 {
		return nil, nil, nil
	}
	return []SecurityFinding{{
		Check:    "logging",
		Severity: SeverityInfo,
		Title:    "connection audit logging is off",
		Detail: "These logging settings are off: " + strings.Join(off, ", ") +
			". Without them there's no record of who connected and when — a gap during incident response (unless you rely on pgaudit or your provider's connection logs).",
		Remediation: "Set log_connections = on and log_disconnections = on (or use pgaudit) if you need a connection audit trail.",
	}}, nil, nil
}

// isLogOn reports whether a boolean GUC string is enabled. log_connections became
// an enum in PostgreSQL 18 (any non-empty, non-"off" value enables some logging),
// so anything other than off/false/"" counts as on.
func isLogOn(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "false", "0", "no":
		return false
	default:
		return true
	}
}

// ---- reliability: durability GUCs ----

// reliabBoolOff reports whether a boolean GUC string reads as off/false.
func reliabBoolOff(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "false", "0", "no", "":
		return true
	default:
		return false
	}
}

// checkDurabilitySettings flags GUCs that trade away crash safety. fsync and
// full_page_writes off can corrupt the cluster on an OS/hardware crash;
// zero_damaged_pages on makes Postgres silently discard corrupt pages (data loss).
// synchronous_commit off is a legitimate latency/durability trade-off (a crash can
// lose the last few committed transactions), so it's INFO, not a warning. These read
// from current_setting and need no special privilege.
func checkDurabilitySettings(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	rows, err := q.Query(ctx, `SELECT current_setting('fsync'),
       current_setting('full_page_writes'),
       current_setting('zero_damaged_pages'),
       current_setting('synchronous_commit')`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var fsync, fpw, zdp, syncCommit string
	if rows.Next() {
		if err := rows.Scan(&fsync, &fpw, &zdp, &syncCommit); err != nil {
			return nil, nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return durabilityFindings(fsync, fpw, zdp, syncCommit), nil, nil
}

// durabilityFindings is the pure decision logic for checkDurabilitySettings.
func durabilityFindings(fsync, fpw, zdp, syncCommit string) []SecurityFinding {
	var out []SecurityFinding
	if reliabBoolOff(fsync) {
		out = append(out, SecurityFinding{
			Check: "durability-settings", Severity: SeverityCritical,
			Title:       "fsync is off",
			Detail:      "fsync = off means Postgres does not force writes to disk. An OS crash or power loss can leave the database irrecoverably corrupted.",
			Remediation: "Set fsync = on. Leave it off only on a database whose data you can afford to lose entirely (e.g. a throwaway load-test instance).",
		})
	}
	if reliabBoolOff(fpw) {
		out = append(out, SecurityFinding{
			Check: "durability-settings", Severity: SeverityCritical,
			Title:       "full_page_writes is off",
			Detail:      "full_page_writes = off risks torn pages: a crash during a page write can leave a half-written block that recovery cannot repair, corrupting data.",
			Remediation: "Set full_page_writes = on unless your storage guarantees atomic page writes.",
		})
	}
	if !reliabBoolOff(zdp) { // zero_damaged_pages ON is the dangerous state
		out = append(out, SecurityFinding{
			Check: "durability-settings", Severity: SeverityWarning,
			Title:       "zero_damaged_pages is on",
			Detail:      "zero_damaged_pages = on makes Postgres zero out (discard) pages it finds corrupt instead of erroring — silent data loss. It's a recovery-only escape hatch.",
			Remediation: "Set zero_damaged_pages = off in normal operation; enable it only briefly while salvaging a damaged table.",
		})
	}
	if strings.EqualFold(strings.TrimSpace(syncCommit), "off") {
		out = append(out, SecurityFinding{
			Check: "durability-settings", Severity: SeverityInfo,
			Title:       "synchronous_commit is off",
			Detail:      "synchronous_commit = off lets COMMIT return before the WAL is durably flushed, so a crash can lose the last fraction of a second of committed transactions. This is a deliberate throughput trade-off — fine if you accept it.",
			Remediation: "Set synchronous_commit = on if every committed transaction must survive a crash.",
		})
	}
	return out
}

// checkAutovacuumConfig flags autovacuum being disabled cluster-wide, and track_counts
// being off (which starves autovacuum of the dead-tuple stats it triggers on). With
// autovacuum off, dead tuples accumulate unchecked and XID wraparound creeps toward a
// forced shutdown — so it's CRITICAL. Reads from current_setting; no special privilege.
func checkAutovacuumConfig(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	rows, err := q.Query(ctx, `SELECT current_setting('autovacuum'), current_setting('track_counts')`)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	var autovac, trackCounts string
	if rows.Next() {
		if err := rows.Scan(&autovac, &trackCounts); err != nil {
			return nil, nil, err
		}
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	return autovacuumFindings(autovac, trackCounts), nil, nil
}

// autovacuumFindings is the pure decision logic for checkAutovacuumConfig.
func autovacuumFindings(autovac, trackCounts string) []SecurityFinding {
	var out []SecurityFinding
	if reliabBoolOff(autovac) {
		out = append(out, SecurityFinding{
			Check: "autovacuum-config", Severity: SeverityCritical,
			Title:       "autovacuum is disabled",
			Detail:      "autovacuum = off means dead tuples are never reclaimed automatically and tables are never auto-analyzed — bloat grows unchecked and XID age climbs toward a forced anti-wraparound shutdown.",
			Remediation: "Set autovacuum = on. If it was disabled for a one-off bulk load, re-enable it and VACUUM (ANALYZE) the affected tables.",
		})
	}
	if reliabBoolOff(trackCounts) {
		out = append(out, SecurityFinding{
			Check: "autovacuum-config", Severity: SeverityWarning,
			Title:       "track_counts is off",
			Detail:      "track_counts = off stops Postgres from counting row changes, so autovacuum can't tell when a table needs vacuuming or analyzing — it effectively stops triggering.",
			Remediation: "Set track_counts = on (the default) so autovacuum can see dead-tuple activity.",
		})
	}
	return out
}

// reliabCheckpoint* mirror the status checkpoint thresholds: a high share of forced
// checkpoints over a meaningful sample suggests max_wal_size is too small.
const (
	reliabCheckpointForcedPct = 50.0
	reliabCheckpointMinSample = 10
)

// checkCheckpointPressure connects a runtime symptom to the knob: when most checkpoints
// are being forced by WAL volume (rather than firing on checkpoint_timeout), max_wal_size
// is usually too small, which hurts write throughput. Evidence-based — it only fires when
// the counters show it — so it's a WARNING with the actual ratio in the detail.
func checkCheckpointPressure(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	cs, err := CheckpointActivity(ctx, q)
	if err != nil {
		if isPermissionDenied(err) {
			return nil, &SkippedCheck{Check: "checkpoint-pressure", Reason: "reading checkpoint stats needs access to pg_stat_bgwriter/pg_stat_checkpointer"}, nil
		}
		return nil, nil, err
	}
	if f := checkpointPressureFinding(cs); f != nil {
		return []SecurityFinding{*f}, nil, nil
	}
	return nil, nil, nil
}

// checkpointPressureFinding is the pure decision logic: a finding only when a meaningful
// sample of checkpoints is mostly forced. Returns nil when there's too little data or the
// forced ratio is healthy.
func checkpointPressureFinding(cs CheckpointStats) *SecurityFinding {
	total := cs.Timed + cs.Requested
	if total < reliabCheckpointMinSample {
		return nil // too few checkpoints to judge
	}
	forcedPct := 100 * float64(cs.Requested) / float64(total)
	if forcedPct < reliabCheckpointForcedPct {
		return nil
	}
	return &SecurityFinding{
		Check: "checkpoint-pressure", Severity: SeverityWarning,
		Title: "checkpoints are mostly forced by WAL volume",
		Detail: fmt.Sprintf("%.0f%% of %d checkpoints since the last stats reset were forced (triggered by hitting max_wal_size) rather than firing on the timer. Frequent forced checkpoints increase write amplification and I/O.",
			forcedPct, total),
		Remediation: "Raise max_wal_size so checkpoints fire on checkpoint_timeout instead of WAL pressure (see `pgdx get settings max_wal_size`).",
	}
}

// ---- pg_hba authentication methods ----

// hbaAuthQuery lists active pg_hba.conf rules whose auth method is insecure.
// Reading pg_hba_file_rules requires superuser or pg_read_all_settings; without
// it the query errors with 42501 and the check is skipped. array_to_string keeps
// the text[] database/user columns scannable as plain strings.
func hbaAuthQuery() string {
	return `SELECT type,
       array_to_string(database, ','),
       array_to_string(user_name, ','),
       COALESCE(address, ''),
       auth_method
FROM pg_catalog.pg_hba_file_rules
WHERE error IS NULL
  AND auth_method IN ('trust', 'password', 'md5')
ORDER BY auth_method, type`
}

// hbaAuthCategory classifies an insecure pg_hba auth method.
type hbaAuthCategory string

const (
	hbaSecure    hbaAuthCategory = ""          // not flagged (scram-sha-256, cert, gss, …)
	hbaTrust     hbaAuthCategory = "trust"     // no credential check at all — critical
	hbaCleartext hbaAuthCategory = "cleartext" // 'password' sends the password in the clear — warning
	hbaWeakHash  hbaAuthCategory = "md5"       // md5 challenge-response — weak/deprecated — warning
)

// classifyHBAAuth maps a pg_hba auth method to its risk category. md5 is a notch
// safer than cleartext 'password' (it isn't sent in the clear) but is weak and
// deprecated in favor of scram-sha-256, so it's flagged at warning like the
// password_encryption check.
func classifyHBAAuth(method string) hbaAuthCategory {
	switch method {
	case "trust":
		return hbaTrust
	case "password":
		return hbaCleartext
	case "md5":
		return hbaWeakHash
	default:
		return hbaSecure
	}
}

func checkHBAAuth(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	buckets, err := scanHBAAuth(ctx, q)
	if err != nil {
		// pgx v5 executes lazily, so "permission denied" can surface from Query OR from
		// iterating rows — either way it means the role can't read pg_hba_file_rules.
		if isPermissionDenied(err) {
			return nil, &SkippedCheck{
				Check:  "hba-auth",
				Reason: "reading pg_hba_file_rules needs superuser or membership in pg_read_all_settings",
			}, nil
		}
		return nil, nil, err
	}

	var out []SecurityFinding
	if rules := buckets[hbaTrust]; len(rules) > 0 {
		out = append(out, SecurityFinding{
			Check:       "hba-trust",
			Severity:    SeverityCritical,
			Title:       fmt.Sprintf("trust authentication in %d pg_hba rule%s", len(rules), plural(len(rules))),
			Detail:      "These rules accept connections with NO password check: " + strings.Join(rules, "; ") + ".",
			Remediation: "Replace trust with scram-sha-256 (or a vetted method) for all but tightly-scoped local rules, then reload the config.",
		})
	}
	if rules := buckets[hbaCleartext]; len(rules) > 0 {
		out = append(out, SecurityFinding{
			Check:       "hba-password",
			Severity:    SeverityWarning,
			Title:       fmt.Sprintf("cleartext password auth in %d pg_hba rule%s", len(rules), plural(len(rules))),
			Detail:      "The 'password' method transmits credentials unencrypted: " + strings.Join(rules, "; ") + ".",
			Remediation: "Use scram-sha-256 instead of password, and require TLS (hostssl) so credentials are never sent in the clear.",
		})
	}
	if rules := buckets[hbaWeakHash]; len(rules) > 0 {
		out = append(out, SecurityFinding{
			Check:       "hba-md5",
			Severity:    SeverityWarning,
			Title:       fmt.Sprintf("md5 authentication in %d pg_hba rule%s", len(rules), plural(len(rules))),
			Detail:      "These rules use md5, a weak and deprecated password method: " + strings.Join(rules, "; ") + ".",
			Remediation: "Migrate to scram-sha-256 (set password_encryption, have users reset passwords, then change the hba method).",
		})
	}
	return out, nil, nil
}

// ---- untrusted procedural languages ----

// scanHBAAuth runs hbaAuthQuery and buckets the matching rules by risk category.
// It is a separate function so the caller can funnel a permission error from
// either Query or row iteration through a single skip path.
func scanHBAAuth(ctx context.Context, q Querier) (map[hbaAuthCategory][]string, error) {
	rows, err := q.Query(ctx, hbaAuthQuery())
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	buckets := map[hbaAuthCategory][]string{}
	for rows.Next() {
		var connType, database, user, address, method string
		if err := rows.Scan(&connType, &database, &user, &address, &method); err != nil {
			return nil, err
		}
		cat := classifyHBAAuth(method)
		if cat == hbaSecure {
			continue
		}
		where := address
		if where == "" {
			where = connType // local socket rules have no address
		}
		entry := fmt.Sprintf("%s (db=%s user=%s)", where, dashIfBlank(database), dashIfBlank(user))
		buckets[cat] = append(buckets[cat], entry)
	}
	return buckets, rows.Err()
}

// untrustedLanguagesQuery lists installed untrusted procedural languages
// (lanpltrusted = false), e.g. plpython3u / plperlu. A non-trusted PL can run
// arbitrary code as the Postgres OS user, so its mere presence is worth surfacing.
func untrustedLanguagesQuery() string {
	return `SELECT lanname
FROM pg_catalog.pg_language
WHERE lanispl AND NOT lanpltrusted
ORDER BY lanname`
}

func checkUntrustedLanguages(ctx context.Context, q Querier) ([]SecurityFinding, *SkippedCheck, error) {
	rows, err := q.Query(ctx, untrustedLanguagesQuery())
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var langs []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, nil, err
		}
		langs = append(langs, name)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	if len(langs) == 0 {
		return nil, nil, nil
	}
	return []SecurityFinding{{
		Check:    "untrusted-languages",
		Severity: SeverityWarning,
		Title:    fmt.Sprintf("%d untrusted procedural language%s installed", len(langs), plural(len(langs))),
		Detail: "Installed untrusted languages: " + strings.Join(langs, ", ") +
			". Functions in these languages run as the Postgres OS user with no sandbox.",
		Remediation: "Drop any you don't use (DROP LANGUAGE <name>), and restrict CREATE FUNCTION on the rest to trusted roles.",
	}}, nil, nil
}

// queryOneString runs a single-column, single-row query and returns the value.
func queryOneString(ctx context.Context, q Querier, sql string) (string, error) {
	rows, err := q.Query(ctx, sql)
	if err != nil {
		return "", err
	}
	defer rows.Close()
	var val string
	if rows.Next() {
		if err := rows.Scan(&val); err != nil {
			return "", err
		}
	}
	return val, rows.Err()
}

// dashIfBlank renders an empty pg_hba field (an "all" match) as a readable token.
func dashIfBlank(s string) string {
	if s == "" {
		return "all"
	}
	return s
}

// plural returns "s" for any count other than 1, for grammatical messages.
func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
