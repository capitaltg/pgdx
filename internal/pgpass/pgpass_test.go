package pgpass

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEscapeField(t *testing.T) {
	cases := map[string]string{
		"simple":     "simple",
		"with:colon": `with\:colon`,
		`back\slash`: `back\\slash`,
		`a:b\c`:      `a\:b\\c`,
	}
	for in, want := range cases {
		if got := escapeField(in); got != want {
			t.Fatalf("escapeField(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestUpsertAppendsThenReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".pgpass")

	if err := Upsert(path, Entry{Host: "h", Port: "5432", Database: "shop", User: "alice", Password: "p1"}); err != nil {
		t.Fatal(err)
	}
	// A different user must NOT collide.
	if err := Upsert(path, Entry{Host: "h", Port: "5432", Database: "shop", User: "bob", Password: "p2"}); err != nil {
		t.Fatal(err)
	}
	// Replacing alice's password updates in place, not appends.
	if err := Upsert(path, Entry{Host: "h", Port: "5432", Database: "shop", User: "alice", Password: "p3"}); err != nil {
		t.Fatal(err)
	}

	data, _ := os.ReadFile(path)
	out := string(data)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines (alice updated, bob kept), got %d:\n%s", len(lines), out)
	}
	if !strings.Contains(out, "h:5432:shop:alice:p3") {
		t.Fatalf("alice password not updated:\n%s", out)
	}
	if strings.Contains(out, ":alice:p1") {
		t.Fatalf("stale alice password remains:\n%s", out)
	}
	if !strings.Contains(out, "h:5432:shop:bob:p2") {
		t.Fatalf("bob entry lost:\n%s", out)
	}
}

func TestUpsertMode0600(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".pgpass")
	if err := Upsert(path, Entry{Host: "h", User: "u", Password: "x"}); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("pgpass mode = %o, want 0600 (libpq ignores it otherwise)", fi.Mode().Perm())
	}
}

func TestUpsertDefaultsWildcards(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".pgpass")
	if err := Upsert(path, Entry{User: "u", Password: "x"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "*:*:*:u:x") {
		t.Fatalf("missing fields should default to *:\n%s", string(data))
	}
}

func TestUpsertEscapedPasswordRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".pgpass")
	// Password containing a colon must be escaped, and updating it must still match
	// the same key (proving firstNFields respects escapes).
	if err := Upsert(path, Entry{Host: "h", Port: "5432", Database: "d", User: "u", Password: "pa:ss"}); err != nil {
		t.Fatal(err)
	}
	if err := Upsert(path, Entry{Host: "h", Port: "5432", Database: "d", User: "u", Password: "new"}); err != nil {
		t.Fatal(err)
	}
	data, _ := os.ReadFile(path)
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 1 {
		t.Fatalf("escaped-colon key should match on update, got %d lines:\n%s", len(lines), string(data))
	}
	if !strings.Contains(string(data), "h:5432:d:u:new") {
		t.Fatalf("password not updated:\n%s", string(data))
	}
}
