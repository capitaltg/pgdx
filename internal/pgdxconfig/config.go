// Package pgdxconfig stores pgdx's OWN small state — currently just the default
// context. This is the one place pgdx keeps state Postgres doesn't model: libpq
// has no "default service" concept, so a kubectl-style default lives here.
//
// It is deliberately additive and psql-safe: pgdx applies the default by behaving
// as if PGSERVICE were set for its own process. It never writes anything psql reads,
// and an explicit --dsn or $PGSERVICE always wins (see cmd resolution).
//
// Location: $XDG_CONFIG_HOME/pgdx/config, else ~/.config/pgdx/config.
package pgdxconfig

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const keyDefaultContext = "default-context"

// Config is pgdx's persisted state.
type Config struct {
	DefaultContext string
}

// Dir returns the pgdx config directory.
func Dir() (string, error) {
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		return filepath.Join(x, "pgdx"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "pgdx"), nil
}

// Path returns the pgdx config file path.
func Path() (string, error) {
	d, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(d, "config"), nil
}

// Load reads the config. A missing file yields a zero Config (not an error).
func Load() (*Config, error) {
	path, err := Path()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Config{}, nil
		}
		return nil, err
	}
	cfg := &Config{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	for sc.Scan() {
		t := strings.TrimSpace(sc.Text())
		if t == "" || strings.HasPrefix(t, "#") {
			continue
		}
		k, v, found := strings.Cut(t, "=")
		if !found {
			continue
		}
		if strings.TrimSpace(k) == keyDefaultContext {
			cfg.DefaultContext = strings.TrimSpace(v)
		}
	}
	return cfg, sc.Err()
}

// Save writes the config atomically, creating the directory if needed.
func (c *Config) Save() error {
	dir, err := Dir()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "config")

	var b strings.Builder
	b.WriteString("# pgdx configuration — managed by `pgdx config`\n")
	if c.DefaultContext != "" {
		fmt.Fprintf(&b, "%s=%s\n", keyDefaultContext, c.DefaultContext)
	}

	tmp, err := os.CreateTemp(dir, ".pgdx-config-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(b.String()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
