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

func TestCheckCache(t *testing.T) {
	t.Run("healthy ratio is OK", func(t *testing.T) {
		c := checkCache(&catalog.CurrentDB{BlksHit: 999000, BlksRead: 1000})
		if c.Severity != sevOK || !strings.Contains(c.Message, "99.9%") {
			t.Fatalf("got severity=%q msg=%q", c.Severity, c.Message)
		}
	})
	t.Run("low ratio is informational, not critical", func(t *testing.T) {
		c := checkCache(&catalog.CurrentDB{BlksHit: 50000, BlksRead: 50000})
		if c.Severity != sevInfo || !strings.Contains(c.Message, "shared_buffers") {
			t.Fatalf("got severity=%q msg=%q", c.Severity, c.Message)
		}
	})
	t.Run("too little I/O to judge", func(t *testing.T) {
		c := checkCache(&catalog.CurrentDB{BlksHit: 5, BlksRead: 1})
		if c.Severity != sevInfo || !strings.Contains(c.Message, "too little I/O") {
			t.Fatalf("got severity=%q msg=%q", c.Severity, c.Message)
		}
	})
	t.Run("nil stats degrade to unavailable", func(t *testing.T) {
		if c := checkCache(nil); c.Severity != sevInfo || !strings.Contains(c.Message, "unavailable") {
			t.Fatalf("got severity=%q msg=%q", c.Severity, c.Message)
		}
	})
}

func TestCheckTempFiles(t *testing.T) {
	t.Run("no spill is OK", func(t *testing.T) {
		if c := checkTempFiles(&catalog.CurrentDB{}); c.Severity != sevOK {
			t.Fatalf("got severity=%q msg=%q", c.Severity, c.Message)
		}
	})
	t.Run("spill is informational with the byte figure", func(t *testing.T) {
		c := checkTempFiles(&catalog.CurrentDB{TempBytes: 5 << 20})
		if c.Severity != sevInfo || !strings.Contains(c.Message, "work_mem") {
			t.Fatalf("got severity=%q msg=%q", c.Severity, c.Message)
		}
	})
}

func TestShortVersion(t *testing.T) {
	cases := map[string]string{
		"16.2":                           "16.2",
		"16.2 (Ubuntu 16.2-1.pgdg22.04)": "16.2",
		"":                               "",
	}
	for in, want := range cases {
		if got := shortVersion(in); got != want {
			t.Fatalf("shortVersion(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestFormatWAL(t *testing.T) {
	gb := int64(1) << 30
	t.Run("annotates with both governors when set", func(t *testing.T) {
		got := formatWAL(&catalog.WALInfo{Bytes: 2 * gb, Segments: 35, MaxWALBytes: 6 * gb, WALKeepBytes: 2 * gb})
		for _, want := range []string{"35 segment(s)", "max_wal_size", "wal_keep_size"} {
			if !strings.Contains(got, want) {
				t.Fatalf("formatWAL missing %q: %q", want, got)
			}
		}
	})
	t.Run("omits wal_keep_size when unset", func(t *testing.T) {
		got := formatWAL(&catalog.WALInfo{Bytes: gb, Segments: 16, MaxWALBytes: gb, WALKeepBytes: 0})
		if strings.Contains(got, "wal_keep_size") {
			t.Fatalf("formatWAL should omit unset wal_keep_size: %q", got)
		}
		if !strings.Contains(got, "max_wal_size") {
			t.Fatalf("formatWAL should still show max_wal_size: %q", got)
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

func TestPrintStatusScopedSections(t *testing.T) {
	var buf bytes.Buffer
	cmd := &cobra.Command{}
	cmd.SetOut(&buf)
	printStatus(cmd, statusReport{
		Database: "shop", Host: "db", Port: 5432,
		Checks: []statusCheck{
			{Name: "Connections", Scope: scopeCluster, Severity: sevOK, Message: "ok"},
			{Name: "Cache", Scope: scopeDatabase, Severity: sevOK, Message: "100%"},
		},
	})
	o := buf.String()
	for _, want := range []string{"Cluster-wide (the whole server)", `This database ("shop")`} {
		if !strings.Contains(o, want) {
			t.Fatalf("missing section heading %q:\n%s", want, o)
		}
	}
	// Cluster section precedes the database section.
	if strings.Index(o, "Cluster-wide") > strings.Index(o, "This database") {
		t.Fatalf("cluster section must come first:\n%s", o)
	}
	// A cluster check renders under the cluster heading, before the database heading.
	if !(strings.Index(o, "Connections") < strings.Index(o, "This database")) {
		t.Fatalf("Connections should be in the cluster section:\n%s", o)
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
