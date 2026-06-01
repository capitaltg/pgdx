package pgdxconfig

import (
	"os"
	"testing"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	// Missing file → zero config, no error.
	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.DefaultContext != "" {
		t.Fatalf("fresh config should have no default, got %q", cfg.DefaultContext)
	}

	cfg.DefaultContext = "prod"
	if err := cfg.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.DefaultContext != "prod" {
		t.Fatalf("default context = %q, want prod", got.DefaultContext)
	}
}

func TestPathHonorsXDG(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	p, err := Path()
	if err != nil {
		t.Fatal(err)
	}
	if want := dir + "/pgdx/config"; p != want {
		t.Fatalf("Path = %q, want %q", p, want)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
}
