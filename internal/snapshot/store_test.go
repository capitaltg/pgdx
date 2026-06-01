package snapshot

import (
	"testing"
	"time"

	"github.com/capitaltg/pgdx/internal/catalog"
)

func TestSaveListLoad(t *testing.T) {
	t.Setenv("PGDX_STATE_DIR", t.TempDir())

	// Two captures a second apart so their timestamp-prefixed names sort deterministically.
	older := &Snapshot{Label: "before", Database: "shop", TakenAt: time.Unix(1_700_000_000, 0),
		Statements: []catalog.StmtStatRow{{QueryID: qid(1), Query: "select 1", Calls: 1}}}
	newer := &Snapshot{Label: "after", Database: "shop", TakenAt: time.Unix(1_700_000_060, 0)}

	if _, err := Save(older); err != nil {
		t.Fatalf("save older: %v", err)
	}
	if _, err := Save(newer); err != nil {
		t.Fatalf("save newer: %v", err)
	}

	ents, err := List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(ents) != 2 {
		t.Fatalf("want 2 entries, got %d", len(ents))
	}

	// Load by label substring.
	got, err := Load("before")
	if err != nil {
		t.Fatalf("load by label: %v", err)
	}
	if got.Database != "shop" || len(got.Statements) != 1 {
		t.Fatalf("round-trip lost data: %+v", got)
	}

	// LatestTwo returns (older, newer) by capture time.
	o, n, err := LatestTwo()
	if err != nil {
		t.Fatalf("latest two: %v", err)
	}
	if o.Label != "before" || n.Label != "after" {
		t.Fatalf("LatestTwo order wrong: older=%q newer=%q", o.Label, n.Label)
	}
}

func TestLoadMissing(t *testing.T) {
	t.Setenv("PGDX_STATE_DIR", t.TempDir())
	if _, err := Load("nope"); err == nil {
		t.Fatal("expected an error loading a non-existent snapshot")
	}
	if _, _, err := LatestTwo(); err == nil {
		t.Fatal("expected an error when fewer than two snapshots exist")
	}
}

func TestFileSafe(t *testing.T) {
	if got := fileSafe("before/deploy 1"); got != "before_deploy_1" {
		t.Fatalf("fileSafe sanitization wrong: %q", got)
	}
}
