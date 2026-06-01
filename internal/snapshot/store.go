// Package snapshot persists point-in-time captures of Postgres's cumulative statistics
// (pg_stat_statements and pg_stat_user_tables) to local files, so `pgdx diff` can
// subtract two captures and show what actually changed between them.
//
// Postgres exposes these counters only as totals-since-reset, which are nearly useless
// on their own — "this query has used 4 hours of CPU since last March" says nothing
// about now. The delta between two snapshots is the real signal ("what got slow since
// this morning"), and core Postgres has no built-in way to take it. Snapshots are plain
// JSON files under a state directory; nothing here ever writes to the database.
package snapshot

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/capitaltg/pgdx/internal/catalog"
)

// Snapshot is one capture plus the identity needed to refuse a nonsensical diff (two
// snapshots of different databases).
type Snapshot struct {
	Label      string                 `json:"label"`
	Database   string                 `json:"database"`
	Host       string                 `json:"host"`
	Port       uint16                 `json:"port"`
	TakenAt    time.Time              `json:"taken_at"`
	Statements []catalog.StmtStatRow  `json:"statements"`
	Tables     []catalog.TableStatRow `json:"tables"`
}

// Dir returns the snapshot store directory, honoring $PGDX_STATE_DIR, then
// $XDG_STATE_HOME/pgdx/snapshots, then ~/.local/state/pgdx/snapshots.
func Dir() (string, error) {
	if d := os.Getenv("PGDX_STATE_DIR"); d != "" {
		return d, nil
	}
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		return filepath.Join(x, "pgdx", "snapshots"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "state", "pgdx", "snapshots"), nil
}

// fileSafe turns a label into a filename-safe token (so a label can't escape the store
// directory or collide with the timestamp separator).
func fileSafe(s string) string {
	repl := func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-':
			return r
		default:
			return '_'
		}
	}
	return strings.Map(repl, s)
}

// Save writes a snapshot to the store and returns its file path. The filename encodes
// the capture time (sortable) and the label.
func Save(s *Snapshot) (string, error) {
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	label := fileSafe(s.Label)
	if label == "" {
		label = "snap"
	}
	name := fmt.Sprintf("%s_%s.json", s.TakenAt.UTC().Format("20060102T150405Z"), label)
	path := filepath.Join(dir, name)
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		return "", err
	}
	return path, nil
}

// Entry is a stored snapshot's file metadata (for listing / resolving), without loading
// the (potentially large) payload.
type Entry struct {
	Name    string // bare filename
	Path    string
	ModTime time.Time
}

// List returns stored snapshots, newest first. A missing store directory is not an
// error — it just means nothing has been captured yet.
func List() ([]Entry, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Entry
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		info, ierr := e.Info()
		if ierr != nil {
			continue
		}
		out = append(out, Entry{Name: e.Name(), Path: filepath.Join(dir, e.Name()), ModTime: info.ModTime()})
	}
	// Filename prefix is a sortable UTC timestamp, so name sort == chronological.
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// Load reads a snapshot by exact filename, by filename without the .json suffix, or by a
// unique substring match (so "morning" resolves a labeled capture). A leading "@"
// is accepted and ignored, mirroring how the CLI lets users reference snapshots.
func Load(ref string) (*Snapshot, error) {
	ref = strings.TrimPrefix(ref, "@")
	ents, err := List()
	if err != nil {
		return nil, err
	}
	var match *Entry
	for i := range ents {
		e := ents[i]
		base := strings.TrimSuffix(e.Name, ".json")
		if e.Name == ref || base == ref {
			match = &ents[i]
			break
		}
		if strings.Contains(e.Name, ref) {
			if match != nil {
				return nil, fmt.Errorf("snapshot reference %q is ambiguous (matches multiple); use a full name from `pgdx snapshot --list`", ref)
			}
			match = &ents[i]
		}
	}
	if match == nil {
		return nil, fmt.Errorf("no snapshot matching %q (see `pgdx snapshot --list`)", ref)
	}
	return loadPath(match.Path)
}

func loadPath(path string) (*Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var s Snapshot
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, fmt.Errorf("parse snapshot %s: %w", filepath.Base(path), err)
	}
	return &s, nil
}

// LatestTwo returns the two most recent snapshots as (older, newer) for the no-argument
// `pgdx diff`. It errors when fewer than two exist.
func LatestTwo() (older, newer *Snapshot, err error) {
	ents, err := List()
	if err != nil {
		return nil, nil, err
	}
	if len(ents) < 2 {
		return nil, nil, fmt.Errorf("need at least two snapshots to diff (have %d); capture with `pgdx snapshot`", len(ents))
	}
	newer, err = loadPath(ents[0].Path) // List is newest-first
	if err != nil {
		return nil, nil, err
	}
	older, err = loadPath(ents[1].Path)
	if err != nil {
		return nil, nil, err
	}
	return older, newer, nil
}
