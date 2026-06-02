package cmd

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/capitaltg/pgdx/internal/catalog"
)

// Tab-completion for the shell, in two tiers:
//
//   Tier 1 — the command grammar: verbs, subcommands, and flags. Built by walking the
//   live cobra command tree (newRootCmd), so it can never drift from the real commands —
//   add a subcommand and completion picks it up for free.
//
//   Tier 2 — object names: `describe table|view|index <TAB>` and `vacuum <TAB>` list
//   actual relation names from the session's read-only connection (sharedConn),
//   schema-qualified and, for the public schema, bare. Names are cached briefly so
//   repeated TABs don't re-query.
//
// SQL inside `query`/`explain` is deliberately NOT completed — those verbs simply offer
// no children, so TAB there does nothing (psql is the place for SQL completion).

// newCompleter builds the readline completer from a fresh root command. The tree is
// static (the command grammar doesn't change at runtime); the only dynamic parts are the
// object-name leaves, which query the catalog when triggered.
func newCompleter() readline.AutoCompleter {
	items := commandItems(newRootCmd())
	// `use` is a shell built-in (not a cobra command), so add it explicitly: after `use `
	// offer database names plus the `schema` keyword; after `use schema ` offer schema
	// names.
	items = append(items, readline.PcItem("use",
		readline.PcItemDynamic(func(string) []string { return databaseNames() }),
		readline.PcItem("schema",
			readline.PcItemDynamic(func(string) []string { return schemaNames() })),
	))
	return readline.NewPrefixCompleter(items...)
}

// commandItems returns a completer node per visible subcommand of cmd, recursively.
func commandItems(cmd *cobra.Command) []readline.PrefixCompleterInterface {
	var items []readline.PrefixCompleterInterface
	for _, sub := range cmd.Commands() {
		// Skip cobra's generated helpers — they only add noise to the prompt.
		if sub.Hidden || sub.Name() == "help" || sub.Name() == "completion" {
			continue
		}
		items = append(items, pcItemForCommand(sub))
	}
	return items
}

// pcItemForCommand builds one command's completer node: a dynamic object-name child (for
// describe table/view/index), its subcommands, and its flags.
func pcItemForCommand(cmd *cobra.Command) readline.PrefixCompleterInterface {
	var children []readline.PrefixCompleterInterface
	if dyn := objectNameCompleter(cmd); dyn != nil {
		children = append(children, dyn)
	}
	children = append(children, commandItems(cmd)...)
	children = append(children, flagItems(cmd)...)
	return readline.PcItem(cmd.Name(), children...)
}

// flagItems returns completer nodes for a command's long flags plus the root's global
// persistent flags (--dsn, -o/--output, -d/--database, --sql, --timeout), de-duped.
func flagItems(cmd *cobra.Command) []readline.PrefixCompleterInterface {
	seen := map[string]bool{}
	var items []readline.PrefixCompleterInterface
	add := func(fs *pflag.FlagSet) {
		fs.VisitAll(func(f *pflag.Flag) {
			name := "--" + f.Name
			if seen[name] {
				return
			}
			seen[name] = true
			items = append(items, readline.PcItem(name))
		})
	}
	add(cmd.LocalFlags())
	add(cmd.Root().PersistentFlags())
	return items
}

// objectNameCompleter returns a dynamic completer for the commands that take a single
// object name — describe table/view/index and vacuum — or nil for any other command.
func objectNameCompleter(cmd *cobra.Command) readline.PrefixCompleterInterface {
	var kinds []string
	switch {
	case cmd.Name() == "vacuum":
		kinds = []string{"r", "p", "m"} // VACUUM targets tables and materialized views
	case cmd.Parent() != nil && cmd.Parent().Name() == "describe":
		switch cmd.Name() {
		case "table":
			kinds = []string{"r", "p"} // ordinary + partitioned tables
		case "view":
			kinds = []string{"v", "m"} // views + materialized views
		case "index":
			kinds = []string{"i", "I"} // indexes + partitioned indexes
		}
	}
	if len(kinds) == 0 {
		return nil
	}
	return readline.PcItemDynamic(func(string) []string { return objectNames(kinds...) })
}

// objectNames lists relation names of the given relkinds from the session connection,
// formatted as completion candidates: schema-qualified always, plus the bare name for
// public-schema objects (the common case, where `describe table users` works unqualified).
// Results are cached for a few seconds so holding TAB doesn't hammer the catalog. Any
// error (no connection, lost privileges) yields no candidates rather than a failure.
func objectNames(kinds ...string) []string {
	conn := sharedConn
	if conn == nil {
		return nil
	}
	key := strings.Join(kinds, ",")
	if names, ok := completionNames.get(key); ok {
		return names
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rels, err := catalog.ListRelationNames(ctx, conn, kinds...)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(rels))
	for _, r := range rels {
		names = append(names, r.Schema+"."+r.Name)
		if r.Schema == "public" {
			names = append(names, r.Name)
		}
	}
	completionNames.put(key, names)
	return names
}

// databaseNames lists the server's connectable databases, for `use <TAB>` completion.
// Cached like object names; errors yield no candidates.
func databaseNames() []string {
	conn := sharedConn
	if conn == nil {
		return nil
	}
	const key = "\x00databases" // NUL-prefixed so it can't collide with a relkind key
	if names, ok := completionNames.get(key); ok {
		return names
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	names, err := catalog.ListDatabaseNames(ctx, conn)
	if err != nil {
		return nil
	}
	completionNames.put(key, names)
	return names
}

// schemaNames lists user schema names for `use schema <TAB>` completion (cached like the
// rest). Errors yield no candidates.
func schemaNames() []string {
	conn := sharedConn
	if conn == nil {
		return nil
	}
	const key = "\x00schemas" // NUL-prefixed so it can't collide with a relkind key
	if names, ok := completionNames.get(key); ok {
		return names
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	names, err := catalog.ListSchemaNames(ctx, conn)
	if err != nil {
		return nil
	}
	completionNames.put(key, names)
	return names
}

// completionNames is a small TTL cache of object-name candidates, keyed by relkind set.
// A short TTL keeps completion responsive while still reflecting schema changes within a
// session (rare, since pgdx is read-only — they'd come from outside).
var completionNames = &nameCache{data: map[string]nameCacheEntry{}}

const nameCacheTTL = 10 * time.Second

type nameCache struct {
	mu   sync.Mutex
	data map[string]nameCacheEntry
}

type nameCacheEntry struct {
	names []string
	at    time.Time
}

func (c *nameCache) get(key string) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.data[key]
	if !ok || time.Since(e.at) > nameCacheTTL {
		return nil, false
	}
	return e.names, true
}

func (c *nameCache) put(key string, names []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data[key] = nameCacheEntry{names: names, at: time.Now()}
}

// reset drops all cached names — called after `use <database>` switches the connection,
// since object names are database-specific.
func (c *nameCache) reset() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = map[string]nameCacheEntry{}
}
