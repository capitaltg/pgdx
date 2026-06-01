// Package pgservice reads and writes Postgres connection service files
// (~/.pg_service.conf) without breaking compatibility with libpq/psql.
//
// The file is an INI-like format:
//
//	# a comment
//	[prod]
//	host=prod.example.com
//	port=5432
//	user=analyst
//	dbname=shop
//
// Round-trip safety is the whole point (design doc, v0.2 config feature): we parse
// into an ordered line model, edit only the section asked for, and re-emit every
// other section, comment, blank line, and key order verbatim. A pgdx-written file
// is a normal Postgres file that psql can read.
package pgservice

import (
	"bufio"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

type lineKind int

const (
	kindOther   lineKind = iota // comment, blank, or anything we don't model — preserved verbatim
	kindSection                 // [name]
	kindKV                      // key=value
)

type line struct {
	kind    lineKind
	raw     string // verbatim text for kindOther
	section string // section name for kindSection
	key     string // for kindKV
	value   string // for kindKV
}

// File is a parsed pg_service.conf preserving original ordering and formatting.
type File struct {
	lines []line
}

// DefaultPath returns PGSERVICEFILE if set, else ~/.pg_service.conf.
func DefaultPath() (string, error) {
	if p := os.Getenv("PGSERVICEFILE"); p != "" {
		return p, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".pg_service.conf"), nil
}

// Load parses the file at path. A missing file is not an error — it returns an
// empty File ready to write.
func Load(path string) (*File, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &File{}, nil
		}
		return nil, err
	}
	return Parse(data)
}

// Parse builds a File from raw bytes.
func Parse(data []byte) (*File, error) {
	f := &File{}
	sc := bufio.NewScanner(bytes.NewReader(data))
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		raw := sc.Text()
		trimmed := strings.TrimSpace(raw)
		switch {
		case trimmed == "" || strings.HasPrefix(trimmed, "#"):
			f.lines = append(f.lines, line{kind: kindOther, raw: raw})
		case strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]"):
			name := strings.TrimSpace(trimmed[1 : len(trimmed)-1])
			f.lines = append(f.lines, line{kind: kindSection, section: name})
		case strings.Contains(trimmed, "="):
			eq := strings.IndexByte(trimmed, '=')
			key := strings.TrimSpace(trimmed[:eq])
			val := strings.TrimSpace(trimmed[eq+1:])
			f.lines = append(f.lines, line{kind: kindKV, key: key, value: val})
		default:
			// Not something we understand; keep it verbatim so we never corrupt the file.
			f.lines = append(f.lines, line{kind: kindOther, raw: raw})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return f, nil
}

// Services returns section names in file order.
func (f *File) Services() []string {
	var out []string
	for _, l := range f.lines {
		if l.kind == kindSection {
			out = append(out, l.section)
		}
	}
	return out
}

// Get returns the key/value pairs of a section (keys lower-cased) and whether it exists.
func (f *File) Get(name string) (map[string]string, bool) {
	start, end, ok := f.sectionSpan(name)
	if !ok {
		return nil, false
	}
	kv := map[string]string{}
	for _, l := range f.lines[start:end] {
		if l.kind == kindKV {
			kv[strings.ToLower(l.key)] = l.value
		}
	}
	return kv, true
}

// sectionSpan returns [start,end) line indices for the body of a section (the
// lines after its header up to the next section header or EOF), and the header
// index is start-1.
func (f *File) sectionSpan(name string) (start, end int, ok bool) {
	header := -1
	for i, l := range f.lines {
		if l.kind == kindSection && strings.EqualFold(l.section, name) {
			header = i
			break
		}
	}
	if header < 0 {
		return 0, 0, false
	}
	end = len(f.lines)
	for i := header + 1; i < len(f.lines); i++ {
		if f.lines[i].kind == kindSection {
			end = i
			break
		}
	}
	return header + 1, end, true
}

// Set merges kv into a section, creating the section if absent. Existing keys not
// in kv are left untouched (merge, not replace). Keys are matched case-insensitively.
func (f *File) Set(name string, kv map[string]string) {
	start, end, ok := f.sectionSpan(name)
	if !ok {
		// Append a new section. Separate from prior content with a blank line for
		// readability (a kv/section line has empty .raw, so check the kind too).
		if len(f.lines) > 0 && !isBlankLine(f.lines[len(f.lines)-1]) {
			f.lines = append(f.lines, line{kind: kindOther, raw: ""})
		}
		f.lines = append(f.lines, line{kind: kindSection, section: name})
		for k, v := range orderedKV(kv) {
			f.lines = append(f.lines, line{kind: kindKV, key: v.key, value: v.val})
			_ = k
		}
		return
	}

	body := append([]line(nil), f.lines[start:end]...)
	for _, kvp := range orderedKV(kv) {
		found := false
		for i := range body {
			if body[i].kind == kindKV && strings.EqualFold(body[i].key, kvp.key) {
				body[i].value = kvp.val
				found = true
				break
			}
		}
		if !found {
			body = append(body, line{kind: kindKV, key: kvp.key, value: kvp.val})
		}
	}
	rebuilt := append([]line(nil), f.lines[:start]...)
	rebuilt = append(rebuilt, body...)
	rebuilt = append(rebuilt, f.lines[end:]...)
	f.lines = rebuilt
}

// Delete removes a section header and its body. Returns whether it existed.
func (f *File) Delete(name string) bool {
	start, end, ok := f.sectionSpan(name)
	if !ok {
		return false
	}
	// start-1 is the header line; remove header through end of body.
	rebuilt := append([]line(nil), f.lines[:start-1]...)
	rebuilt = append(rebuilt, f.lines[end:]...)
	f.lines = rebuilt
	return true
}

// Bytes renders the file back to its on-disk form.
func (f *File) Bytes() []byte {
	var b strings.Builder
	for _, l := range f.lines {
		switch l.kind {
		case kindSection:
			b.WriteString("[" + l.section + "]\n")
		case kindKV:
			b.WriteString(l.key + "=" + l.value + "\n")
		default:
			b.WriteString(l.raw + "\n")
		}
	}
	return []byte(b.String())
}

// Save atomically writes the file to path: write a temp file in the same directory,
// then rename over the target. Preserves the existing file mode, or 0644 for a new
// file (the service file is not secret; passwords live in ~/.pgpass).
func (f *File) Save(path string) error {
	return atomicWrite(path, f.Bytes(), 0o644)
}

// --- helpers ---

func isBlankLine(l line) bool {
	return l.kind == kindOther && strings.TrimSpace(l.raw) == ""
}

type kvPair struct{ key, val string }

// orderedKV returns kv pairs in a stable order (sorted by key) so output is
// deterministic regardless of map iteration order.
func orderedKV(kv map[string]string) []kvPair {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	// simple insertion sort to avoid importing sort for a tiny slice
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	out := make([]kvPair, 0, len(keys))
	for _, k := range keys {
		out = append(out, kvPair{key: k, val: kv[k]})
	}
	return out
}

// ValidSSLModes are the libpq sslmode values accepted by set-context.
var ValidSSLModes = map[string]bool{
	"disable": true, "allow": true, "prefer": true,
	"require": true, "verify-ca": true, "verify-full": true,
}

// ValidatePort ensures a port is numeric and in range (so psql stays happy).
func ValidatePort(p string) error {
	if p == "" {
		return nil
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("invalid port %q (want 1-65535)", p)
	}
	return nil
}

func atomicWrite(path string, data []byte, mode os.FileMode) error {
	if fi, err := os.Stat(path); err == nil {
		mode = fi.Mode().Perm()
	}
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".pgdx-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op if rename succeeded
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
