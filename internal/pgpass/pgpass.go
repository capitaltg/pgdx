// Package pgpass writes ~/.pgpass entries the way libpq expects, so psql and pgdx
// share one password store.
//
// Format (one entry per line): hostname:port:database:username:password
// A literal ':' or '\' inside any field is backslash-escaped. libpq ignores the
// file unless its mode is 0600, so we always write 0600.
package pgpass

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultPath returns PGPASSFILE if set, else ~/.pgpass.
func DefaultPath() (string, error) {
	if p := os.Getenv("PGPASSFILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pgpass"), nil
}

// Entry is a single pgpass record. Any field may be "*" to match anything.
type Entry struct {
	Host, Port, Database, User, Password string
}

// Upsert writes (or replaces) the password for the host:port:database:user key in
// the file at path. Matching is on the first four fields; the password is replaced
// if a matching line exists, otherwise a new line is appended. The file is written
// atomically with mode 0600.
func Upsert(path string, e Entry) error {
	if e.Host == "" {
		e.Host = "*"
	}
	if e.Port == "" {
		e.Port = "*"
	}
	if e.Database == "" {
		e.Database = "*"
	}
	if e.User == "" {
		e.User = "*"
	}

	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return err
	}

	keyPrefix := strings.Join([]string{
		escapeField(e.Host), escapeField(e.Port), escapeField(e.Database), escapeField(e.User),
	}, ":")
	newLine := keyPrefix + ":" + escapeField(e.Password)

	var out bytes.Buffer
	replaced := false
	if len(existing) > 0 {
		sc := bufio.NewScanner(bytes.NewReader(existing))
		for sc.Scan() {
			l := sc.Text()
			if matchesKey(l, keyPrefix) {
				out.WriteString(newLine + "\n")
				replaced = true
			} else {
				out.WriteString(l + "\n")
			}
		}
		if err := sc.Err(); err != nil {
			return err
		}
	}
	if !replaced {
		out.WriteString(newLine + "\n")
	}

	return atomicWrite0600(path, out.Bytes())
}

// matchesKey reports whether a pgpass line has the same first four (escaped) fields
// as keyPrefix (host:port:database:user).
func matchesKey(lineText, keyPrefix string) bool {
	t := strings.TrimSpace(lineText)
	if t == "" || strings.HasPrefix(t, "#") {
		return false
	}
	// Compare the first four fields, respecting escapes, without unescaping.
	got := firstNFields(lineText, 4)
	return got == keyPrefix
}

// firstNFields returns the first n colon-separated fields (escapes respected) joined
// by ':'. A backslash escapes the next character.
func firstNFields(s string, n int) string {
	var fields []string
	var cur strings.Builder
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteByte('\\')
			cur.WriteRune(r)
			esc = false
		case r == '\\':
			esc = true
		case r == ':':
			fields = append(fields, cur.String())
			cur.Reset()
			if len(fields) == n {
				return strings.Join(fields, ":")
			}
		default:
			cur.WriteRune(r)
		}
	}
	fields = append(fields, cur.String())
	if len(fields) > n {
		fields = fields[:n]
	}
	return strings.Join(fields, ":")
}

// escapeField backslash-escapes ':' and '\' so the value survives the colon-delimited
// format exactly as libpq parses it.
func escapeField(s string) string {
	var b strings.Builder
	for _, r := range s {
		if r == '\\' || r == ':' {
			b.WriteByte('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func atomicWrite0600(path string, data []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pgdx-pgpass-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, 0o600); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
