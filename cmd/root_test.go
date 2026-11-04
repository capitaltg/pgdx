package cmd

import (
	"os"
	"testing"
)

// Color is opt-in per invocation. Each condition here is a case where colored output
// would corrupt something downstream, so they're pinned individually.
func TestUseColor(t *testing.T) {
	origOut, origNoColor := flagOutput, os.Getenv("NO_COLOR")
	t.Cleanup(func() {
		flagOutput = origOut
		if origNoColor == "" {
			os.Unsetenv("NO_COLOR")
		} else {
			os.Setenv("NO_COLOR", origNoColor)
		}
	})
	os.Unsetenv("NO_COLOR")

	// -o json must stay machine-readable; escapes would break `| jq`.
	flagOutput = "json"
	if useColor() {
		t.Error("json output must never be colored")
	}

	// NO_COLOR is the cross-tool opt-out (no-color.org) and wins even for table output.
	flagOutput = "table"
	os.Setenv("NO_COLOR", "1")
	if useColor() {
		t.Error("NO_COLOR must disable color")
	}
	// Set-but-empty still counts as set, per the spec.
	os.Setenv("NO_COLOR", "")
	if useColor() {
		t.Error("NO_COLOR set to empty must still disable color")
	}
	os.Unsetenv("NO_COLOR")

	// Under `go test` stdout is a pipe, so the TTY check must come back false — this is
	// what keeps piped and redirected output plain.
	if useColor() {
		t.Error("color must be off when stdout is not a terminal")
	}
}
