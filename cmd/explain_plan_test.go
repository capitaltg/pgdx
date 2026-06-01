package cmd

import (
	"bytes"
	"strings"
	"testing"
)

// TestExplainPlanFromStdin drives the real `explain --plan -` path end to end with no
// database: a plan on stdin should be parsed and diagnosed.
func TestExplainPlanFromStdin(t *testing.T) {
	plan := `[{"Plan":{"Node Type":"Seq Scan","Relation Name":"events",
	"Startup Cost":0.00,"Total Cost":25000.00,"Plan Rows":1000000}}]`

	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader(plan))
	root.SetArgs([]string{"explain", "--plan", "-"})

	if err := root.Execute(); err != nil {
		t.Fatalf("explain --plan - failed: %v (stderr: %s)", err, errBuf.String())
	}
	if !strings.Contains(out.String(), "Full sequential scan") {
		t.Fatalf("expected a full-scan finding, got:\n%s", out.String())
	}
}

func TestExplainPlanRejectsAnalyze(t *testing.T) {
	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetIn(strings.NewReader("[]"))
	root.SetArgs([]string{"explain", "--plan", "-", "--analyze"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected --plan with --analyze to be a usage error")
	}
}

func TestExplainPlanRejectsQueryArg(t *testing.T) {
	root := newRootCmd()
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{"explain", "--plan", "somefile.json", "select 1"})
	if err := root.Execute(); err == nil {
		t.Fatal("expected --plan with a query argument to be a usage error")
	}
}
