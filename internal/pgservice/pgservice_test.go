package pgservice

import (
	"strings"
	"testing"
)

const sample = `# pgdx service file
# managed by hand and by pgdx

[prod]
host=prod.example.com
port=5432
user=analyst
dbname=shop

[local]
host=localhost
user=postgres
dbname=postgres
`

func TestParseAndGet(t *testing.T) {
	f, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Services(); strings.Join(got, ",") != "prod,local" {
		t.Fatalf("Services = %v, want [prod local]", got)
	}
	kv, ok := f.Get("prod")
	if !ok {
		t.Fatal("prod not found")
	}
	if kv["host"] != "prod.example.com" || kv["dbname"] != "shop" {
		t.Fatalf("prod kv wrong: %v", kv)
	}
	if _, ok := f.Get("nope"); ok {
		t.Fatal("nonexistent section reported as found")
	}
}

func TestRoundTripPreservesEverything(t *testing.T) {
	// The compatibility promise: parse then re-emit must be byte-identical, so we
	// never silently drop a comment, blank line, or reorder keys.
	f, err := Parse([]byte(sample))
	if err != nil {
		t.Fatal(err)
	}
	if got := string(f.Bytes()); got != sample {
		t.Fatalf("round-trip changed the file.\n--- got ---\n%s\n--- want ---\n%s", got, sample)
	}
}

func TestSetUpdatesExistingKeyOnly(t *testing.T) {
	f, _ := Parse([]byte(sample))
	f.Set("prod", map[string]string{"host": "new-host.example.com"})

	kv, _ := f.Get("prod")
	if kv["host"] != "new-host.example.com" {
		t.Fatalf("host not updated: %v", kv)
	}
	// Other prod keys untouched; other sections untouched; comments preserved.
	out := string(f.Bytes())
	for _, want := range []string{
		"# pgdx service file", "user=analyst", "dbname=shop",
		"[local]", "host=localhost",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Set clobbered content, missing %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "prod.example.com") {
		t.Fatalf("old host value still present:\n%s", out)
	}
}

func TestSetAddsMissingKeyToSection(t *testing.T) {
	f, _ := Parse([]byte(sample))
	f.Set("prod", map[string]string{"sslmode": "require"})
	kv, _ := f.Get("prod")
	if kv["sslmode"] != "require" {
		t.Fatalf("sslmode not added: %v", kv)
	}
	// Must land inside [prod], before [local].
	out := string(f.Bytes())
	if strings.Index(out, "sslmode=require") > strings.Index(out, "[local]") {
		t.Fatalf("new key leaked past its section:\n%s", out)
	}
}

func TestSetCreatesNewSection(t *testing.T) {
	f, _ := Parse([]byte(sample))
	f.Set("staging", map[string]string{"host": "stage.example.com", "dbname": "shop"})
	kv, ok := f.Get("staging")
	if !ok || kv["host"] != "stage.example.com" {
		t.Fatalf("staging not created: %v ok=%v", kv, ok)
	}
	// Existing sections still intact.
	if _, ok := f.Get("prod"); !ok {
		t.Fatal("creating staging dropped prod")
	}
}

func TestDelete(t *testing.T) {
	f, _ := Parse([]byte(sample))
	if !f.Delete("prod") {
		t.Fatal("Delete(prod) returned false")
	}
	if _, ok := f.Get("prod"); ok {
		t.Fatal("prod still present after delete")
	}
	if _, ok := f.Get("local"); !ok {
		t.Fatal("delete removed the wrong section")
	}
	if f.Delete("prod") {
		t.Fatal("deleting a missing section should return false")
	}
}

func TestValidation(t *testing.T) {
	if err := ValidatePort("5432"); err != nil {
		t.Fatalf("5432 should be valid: %v", err)
	}
	for _, bad := range []string{"0", "70000", "abc", "-1"} {
		if err := ValidatePort(bad); err == nil {
			t.Fatalf("port %q should be invalid", bad)
		}
	}
	if !ValidSSLModes["require"] || ValidSSLModes["bogus"] {
		t.Fatal("sslmode validation table wrong")
	}
}

func TestLoadMissingFileIsEmpty(t *testing.T) {
	f, err := Load("/nonexistent/pgdx/pg_service.conf")
	if err != nil {
		t.Fatalf("missing file should not error: %v", err)
	}
	if len(f.Services()) != 0 {
		t.Fatal("missing file should yield no services")
	}
}
