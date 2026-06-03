package cmd

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/db"
	"github.com/capitaltg/pgdx/internal/render"
)

// audit runs pgdx's security hardening checks against the connected database:
// superuser/BYPASSRLS login roles, a world-writable public schema, RLS policies
// that are defined but not enforced, md5 password storage, server SSL being off,
// trust/cleartext pg_hba rules, and installed untrusted procedural languages.
//
// These are hygiene checks, not a compliance certification — pgdx is a client, so
// it audits what the catalogs expose and skips (loudly) what its privileges can't
// reach. Read-only.
func newAuditCmd() *cobra.Command {
	var exitCode bool
	c := &cobra.Command{
		Use:   "audit",
		Short: "Security hardening checks: roles, schema exposure, RLS, SSL, auth, languages",
		Long: "audit runs a set of read-only security hygiene checks against the connected\n" +
			"database and reports findings grouped by severity (critical / warning / info),\n" +
			"each with a one-line remediation:\n\n" +
			"  • login roles with SUPERUSER or BYPASSRLS\n" +
			"  • a public schema that any role can create objects in\n" +
			"  • tables with row-level security POLICIES defined but RLS not enabled\n" +
			"    (the policies silently do nothing)\n" +
			"  • password_encryption stuck on md5 instead of scram-sha-256\n" +
			"  • server SSL/TLS disabled\n" +
			"  • pg_hba.conf rules using trust (no password) or cleartext password auth\n" +
			"  • installed untrusted procedural languages (plpython3u, plperlu, …)\n\n" +
			"These are hygiene checks, not a compliance audit. pgdx connects as a client, so\n" +
			"it cannot read postgresql.conf and needs superuser (or pg_read_all_settings) to\n" +
			"read pg_hba.conf — a check it can't run is reported as SKIPPED rather than\n" +
			"silently passed. Use -o json for the structured findings.\n\n" +
			"--exit-code makes pgdx exit non-zero (1) when any warning or critical finding is\n" +
			"present, so it can gate CI; the default always exits 0 on a successful audit.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, conn, release, err := connectForGet(cmd)
			if err != nil {
				return err
			}
			ctx := context.Background()
			defer release()

			audit, err := catalog.AuditSecurity(ctx, conn)
			if err != nil {
				return err
			}
			audit.Database = conn.Database()

			if format == render.FormatJSON {
				if audit.Findings == nil {
					audit.Findings = []catalog.SecurityFinding{}
				}
				if err := render.Render(cmd.OutOrStdout(), format, audit); err != nil {
					return err
				}
			} else {
				printSecurityAudit(cmd, conn, audit)
			}

			if exitCode && audit.HasAtLeast(catalog.SeverityWarning) {
				return quietExit{1}
			}
			return nil
		},
	}
	c.Flags().BoolVar(&exitCode, "exit-code", false, "exit 1 if any warning/critical finding is present (for CI gating)")
	return c
}

// printSecurityAudit renders the human view: findings grouped by severity to
// stdout (D4 — the findings are the command's data), with the connection header,
// summary tally, and skipped-check notices on stderr.
func printSecurityAudit(cmd *cobra.Command, conn *db.Conn, a *catalog.SecurityAudit) {
	out := cmd.OutOrStdout()
	e := cmd.ErrOrStderr()

	host := conn.Host()
	if host == "" {
		host = "local"
	}
	fmt.Fprintf(e, "Security audit of %q @ %s:%d\n", a.Database, host, conn.Port())

	printAuditFindings(out, a)

	crit, warn, info := a.Counts()
	if len(a.Findings) == 0 {
		fmt.Fprintf(e, "No issues found across %d checks.\n", a.Checks)
	} else {
		fmt.Fprintf(e, "%s across %d checks (%d critical, %d warning, %d info).\n",
			pluralFindings(len(a.Findings)), a.Checks, crit, warn, info)
	}
	for _, s := range a.Skipped {
		fmt.Fprintf(e, "⚠ skipped %s — %s.\n", s.Check, s.Reason)
	}
}

// printAuditFindings writes the severity-grouped findings to w. Split out from
// printSecurityAudit (which adds the connection header and stderr summary) so it
// is testable without a database connection.
func printAuditFindings(w io.Writer, a *catalog.SecurityAudit) {
	if len(a.Findings) == 0 {
		return
	}
	// Findings arrive sorted by severity; emit each group under its heading.
	var lastSev catalog.Severity = ""
	for _, f := range a.Findings {
		if f.Severity != lastSev {
			if lastSev != "" {
				fmt.Fprintln(w)
			}
			fmt.Fprintln(w, strings.ToUpper(string(f.Severity)))
			lastSev = f.Severity
		}
		fmt.Fprintf(w, "  ● %s\n", f.Title)
		fmt.Fprintf(w, "    %s\n", f.Detail)
		if f.Remediation != "" {
			fmt.Fprintf(w, "    → %s\n", f.Remediation)
		}
	}
}

// pluralFindings renders the finding count with correct grammar.
func pluralFindings(n int) string {
	if n == 1 {
		return "1 finding"
	}
	return fmt.Sprintf("%d findings", n)
}
