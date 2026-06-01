package cmd

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/capitaltg/pgdx/internal/catalog"
)

func TestRootBlockers(t *testing.T) {
	t.Run("head of a chain is the one not itself waiting", func(t *testing.T) {
		// 102 waits on 101; 101 waits on 100; 100 waits on nobody => 100 is the root.
		waits := []catalog.LockWait{
			{PID: 102, BlockedBy: "101"},
			{PID: 101, BlockedBy: "100"},
		}
		roots := rootBlockers(waits)
		if len(roots) != 1 || roots[0] != 100 {
			t.Fatalf("roots = %v, want [100]", roots)
		}
	})
	t.Run("multiple distinct roots, sorted and de-duped", func(t *testing.T) {
		waits := []catalog.LockWait{
			{PID: 5, BlockedBy: "9,3"},
			{PID: 6, BlockedBy: "3"},
		}
		roots := rootBlockers(waits)
		if len(roots) != 2 || roots[0] != 3 || roots[1] != 9 {
			t.Fatalf("roots = %v, want [3 9]", roots)
		}
	})
	t.Run("no blockers", func(t *testing.T) {
		if roots := rootBlockers(nil); len(roots) != 0 {
			t.Fatalf("want none, got %v", roots)
		}
	})
}

func TestSynthesizeStartHere(t *testing.T) {
	t.Run("locks win over everything and name the root blocker", func(t *testing.T) {
		got := synthesizeStartHere(statusSignals{
			blockedCount: 2, rootBlockers: []int32{8821}, connCrit: true,
		})
		if !strings.Contains(got, "pid 8821") || !strings.Contains(got, "root blocker") {
			t.Fatalf("expected a lock-root pointer, got %q", got)
		}
	})
	t.Run("cross-links the root blocker that is also the oldest transaction", func(t *testing.T) {
		got := synthesizeStartHere(statusSignals{
			blockedCount: 1, rootBlockers: []int32{8821},
			oldestXactPID: 8821, oldestXactSec: 720, oldestXactState: "idle in transaction",
		})
		if !strings.Contains(got, "also the oldest transaction") {
			t.Fatalf("expected the cross-link, got %q", got)
		}
	})
	t.Run("connections crit when no locks", func(t *testing.T) {
		got := synthesizeStartHere(statusSignals{connCrit: true})
		if !strings.Contains(got, "Connections are near the cap") {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("transaction crit ranks above wraparound", func(t *testing.T) {
		got := synthesizeStartHere(statusSignals{
			txCrit: true, oldestXactPID: 42, oldestXactSec: 800, oldestXactState: "active",
			wraparoundCrit: true, wraparoundRel: "public.t",
		})
		if !strings.Contains(got, "pid 42") || strings.Contains(got, "wraparound") {
			t.Fatalf("transaction crit should win over wraparound, got %q", got)
		}
	})
	t.Run("healthy when nothing is set", func(t *testing.T) {
		got := synthesizeStartHere(statusSignals{})
		if !strings.Contains(got, "Nothing urgent") {
			t.Fatalf("got %q", got)
		}
	})
}

func TestCapRows(t *testing.T) {
	t.Run("under the cap passes through unchanged", func(t *testing.T) {
		in := []string{"a", "b", "c"}
		if got := capRows(in); len(got) != 3 {
			t.Fatalf("got %d rows, want 3", len(got))
		}
	})
	t.Run("over the cap is trimmed with a +N more pointer", func(t *testing.T) {
		in := make([]string, detailRows+5)
		for i := range in {
			in[i] = fmt.Sprintf("row %d", i)
		}
		got := capRows(in)
		if len(got) != detailRows+1 {
			t.Fatalf("got %d rows, want %d", len(got), detailRows+1)
		}
		last := got[len(got)-1]
		if !strings.Contains(last, "+5 more") {
			t.Fatalf("expected an overflow pointer, got %q", last)
		}
	})
}

func TestTopCounts(t *testing.T) {
	got := topCounts(map[string]int64{"idle": 140, "active": 12, "idle in transaction": 36})
	// Highest count first.
	if len(got) != 3 || !strings.HasPrefix(got[0], "idle:") || !strings.HasPrefix(got[2], "active:") {
		t.Fatalf("expected count-desc order, got %v", got)
	}
}

func TestPrintStatusDetail(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	printStatus(cmd, statusReport{
		Database: "shop", Host: "db", Port: 5432,
		Checks: []statusCheck{
			{Name: "Locks", Severity: sevCrit, Message: "1 session(s) blocked",
				Detail: []string{"pid 8830 waiting on orders (RowExclusiveLock) — blocked by 8821"}},
			{Name: "Bloat", Severity: sevInfo, Message: "top: public.orders ~1.2 GB reclaimable"},
		},
	})
	out := buf.String()
	if !strings.Contains(out, "pid 8830 waiting on orders") {
		t.Fatalf("detail row not rendered:\n%s", out)
	}
	// The detail line is indented past the symbol/name column, not flush-left.
	for _, line := range strings.Split(out, "\n") {
		if strings.Contains(line, "pid 8830") && !strings.HasPrefix(line, "    ") {
			t.Fatalf("detail row should be indented, got %q", line)
		}
	}
}
