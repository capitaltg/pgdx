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

// audit runs pgdx's hygiene checks against the connected database in two sections.
// Security: superuser/BYPASSRLS login roles, a world-writable public schema, RLS
// policies defined but not enforced, md5 password storage, server SSL off,
// trust/cleartext pg_hba rules, and untrusted procedural languages. Reliability:
// crash-safety GUCs turned off (fsync, full_page_writes, …), autovacuum disabled,
// and checkpoints being forced by WAL volume.
//
// These are hygiene checks, not a compliance certification — pgdx is a client, so
// it audits what the catalogs expose and skips (loudly) what its privileges can't
// reach. Read-only.
func newAuditCmd() *cobra.Command {
	var exitCode bool
	c := &cobra.Command{
		Use:   "audit",
		Short: "Hygiene checks — security (roles, RLS, SSL, auth) and reliability (durability, autovacuum)",
		Long: "audit runs a set of read-only hygiene checks against the connected database and\n" +
			"reports findings grouped into SECURITY and RELIABILITY sections, each ordered by\n" +
			"severity (critical / warning / info) with a one-line remediation.\n\n" +
			"Security:\n" +
			"  • login roles with SUPERUSER or BYPASSRLS\n" +
			"  • a public schema that any role can create objects in\n" +
			"  • tables with row-level security POLICIES defined but RLS not enabled\n" +
			"    (the policies silently do nothing)\n" +
			"  • password_encryption stuck on md5 instead of scram-sha-256\n" +
			"  • server SSL/TLS disabled\n" +
			"  • pg_hba.conf rules using trust (no password) or cleartext password auth\n" +
			"  • installed untrusted procedural languages (plpython3u, plperlu, …)\n\n" +
			"Reliability:\n" +
			"  • crash-safety GUCs turned off (fsync, full_page_writes, zero_damaged_pages,\n" +
			"    synchronous_commit)\n" +
			"  • autovacuum disabled (or track_counts off, which starves it)\n" +
			"  • checkpoints mostly forced by WAL volume — a sign max_wal_size is too small\n\n" +
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
	fmt.Fprintf(e, "Audit of %q @ %s:%d\n", a.Database, host, conn.Port())

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

// categoryHeading is the section banner for a finding category.
func categoryHeading(category string) string {
	if category == catalog.CategoryReliability {
		return "RELIABILITY"
	}
	return "SECURITY"
}

// printAuditFindings writes the findings to w, grouped by category (SECURITY, then
// RELIABILITY) and, within each, by severity. Split out from printSecurityAudit (which
// adds the connection header and stderr summary) so it is testable without a database
// connection.
func printAuditFindings(w io.Writer, a *catalog.SecurityAudit) {
	if len(a.Findings) == 0 {
		return
	}
	// Findings arrive sorted by (category, severity); emit a banner when the category
	// changes and a sub-heading when the severity changes within it.
	lastCat := ""
	var lastSev catalog.Severity = ""
	first := true
	for _, f := range a.Findings {
		// Normalize so a finding with no explicit category groups under SECURITY.
		cat := categoryHeading(f.Category)
		if cat != lastCat {
			if !first {
				fmt.Fprintln(w)
			}
			fmt.Fprintf(w, "── %s ──\n", cat)
			lastCat = cat
			lastSev = ""
		}
		if f.Severity != lastSev {
			fmt.Fprintln(w, strings.ToUpper(string(f.Severity)))
			lastSev = f.Severity
		}
		fmt.Fprintf(w, "  ● %s\n", f.Title)
		fmt.Fprintf(w, "    %s\n", f.Detail)
		if f.Remediation != "" {
			fmt.Fprintf(w, "    → %s\n", f.Remediation)
		}
		first = false
	}
}

// pluralFindings renders the finding count with correct grammar.
func pluralFindings(n int) string {
	if n == 1 {
		return "1 finding"
	}
	return fmt.Sprintf("%d findings", n)
}
