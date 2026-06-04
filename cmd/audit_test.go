package cmd

import (
	"bytes"
	"strings"
	"testing"

	"github.com/capitaltg/pgdx/internal/catalog"
)

func TestPluralFindings(t *testing.T) {
	if got := pluralFindings(1); got != "1 finding" {
		t.Errorf("pluralFindings(1) = %q, want \"1 finding\"", got)
	}
	if got := pluralFindings(3); got != "3 findings" {
		t.Errorf("pluralFindings(3) = %q, want \"3 findings\"", got)
	}
}

func TestPrintAuditFindingsEmpty(t *testing.T) {
	var out bytes.Buffer
	printAuditFindings(&out, &catalog.SecurityAudit{})
	if out.Len() != 0 {
		t.Fatalf("clean audit should print no findings body, got:\n%s", out.String())
	}
}

func TestPrintAuditFindingsCategorized(t *testing.T) {
	var out bytes.Buffer
	a := &catalog.SecurityAudit{
		Checks: 14,
		Findings: []catalog.SecurityFinding{
			{Check: "ssl", Category: catalog.CategorySecurity, Severity: catalog.SeverityWarning,
				Title: "SSL disabled", Detail: "no TLS"},
			{Check: "autovacuum-config", Category: catalog.CategoryReliability, Severity: catalog.SeverityCritical,
				Title: "autovacuum is disabled", Detail: "bloat grows", Remediation: "set autovacuum = on"},
		},
	}
	printAuditFindings(&out, a)
	o := out.String()

	for _, want := range []string{"SECURITY", "RELIABILITY", "autovacuum is disabled"} {
		if !strings.Contains(o, want) {
			t.Fatalf("output missing %q:\n%s", want, o)
		}
	}
	// Security section precedes the reliability section.
	if strings.Index(o, "SECURITY") > strings.Index(o, "RELIABILITY") {
		t.Fatalf("SECURITY section must precede RELIABILITY:\n%s", o)
	}
}

func TestPrintAuditFindingsGrouped(t *testing.T) {
	var out bytes.Buffer
	a := &catalog.SecurityAudit{
		Checks: 7,
		Findings: []catalog.SecurityFinding{
			{Check: "rls-disabled", Severity: catalog.SeverityCritical,
				Title: "RLS not enforced", Detail: "policies inert", Remediation: "ENABLE ROW LEVEL SECURITY"},
			{Check: "ssl", Severity: catalog.SeverityWarning,
				Title: "SSL disabled", Detail: "no TLS"},
		},
	}
	printAuditFindings(&out, a)
	o := out.String()

	// Severity headings appear, critical before warning, and the remediation arrow renders.
	for _, want := range []string{"CRITICAL", "WARNING", "RLS not enforced", "→ ENABLE ROW LEVEL SECURITY"} {
		if !strings.Contains(o, want) {
			t.Fatalf("findings output missing %q:\n%s", want, o)
		}
	}
	if strings.Index(o, "CRITICAL") > strings.Index(o, "WARNING") {
		t.Fatalf("CRITICAL group must precede WARNING group:\n%s", o)
	}
	// A finding without remediation should not emit a stray arrow line.
	if strings.Count(o, "→ ") != 1 {
		t.Fatalf("expected exactly one remediation arrow, got:\n%s", o)
	}
}
