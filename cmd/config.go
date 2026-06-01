package cmd

import (
	"bufio"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/capitaltg/pgdx/internal/pgdxconfig"
	"github.com/capitaltg/pgdx/internal/pgpass"
	"github.com/capitaltg/pgdx/internal/pgservice"
	"github.com/capitaltg/pgdx/internal/render"
)

func newConfigCmd() *cobra.Command {
	c := &cobra.Command{
		Use:   "config",
		Short: "Manage connection contexts (reads/writes ~/.pg_service.conf and ~/.pgpass)",
		Long: "config is a thin, standards-respecting wrapper over Postgres's own files.\n" +
			"Contexts are sections in ~/.pg_service.conf (PGSERVICEFILE); a context written\n" +
			"by pgdx is a normal service entry psql can use. Passwords live in ~/.pgpass and\n" +
			"are only ever written by the explicit `set-password` command.",
	}
	c.AddCommand(
		newGetContextsCmd(),
		newCurrentContextCmd(),
		newUseContextCmd(),
		newGetContextCmd(),
		newSetContextCmd(),
		newDeleteContextCmd(),
		newSetPasswordCmd(),
	)
	return c
}

func loadServices() (*pgservice.File, string, error) {
	path, err := pgservice.DefaultPath()
	if err != nil {
		return nil, "", err
	}
	f, err := pgservice.Load(path)
	if err != nil {
		return nil, "", fmt.Errorf("read %s: %w", path, err)
	}
	return f, path, nil
}

// --- get-contexts ---

func newGetContextsCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get-contexts",
		Short: "List connection contexts",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			f, _, err := loadServices()
			if err != nil {
				return err
			}
			current := os.Getenv("PGSERVICE")
			view := contextsView{}
			for _, name := range f.Services() {
				kv, _ := f.Get(name)
				marker := ""
				if name == current {
					marker = "*"
				}
				view.rows = append(view.rows, contextRow{
					Current: marker, Name: name,
					Host: kv["host"], Port: kv["port"], User: kv["user"], Dbname: kv["dbname"],
				})
			}
			if format == render.FormatJSON {
				return render.Render(cmd.OutOrStdout(), format, view.rows)
			}
			if len(view.rows) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no contexts found (empty or missing pg_service.conf)")
				return nil
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, view)
		},
	}
}

type contextRow struct {
	Current string `json:"current,omitempty"`
	Name    string `json:"name"`
	Host    string `json:"host,omitempty"`
	Port    string `json:"port,omitempty"`
	User    string `json:"user,omitempty"`
	Dbname  string `json:"dbname,omitempty"`
}

type contextsView struct{ rows []contextRow }

func (v contextsView) Headers() []string {
	return []string{"CURRENT", "NAME", "HOST", "PORT", "USER", "DBNAME"}
}
func (v contextsView) Rows() [][]string {
	out := make([][]string, 0, len(v.rows))
	for _, r := range v.rows {
		out = append(out, []string{r.Current, r.Name, r.Host, r.Port, r.User, r.Dbname})
	}
	return out
}

// --- current-context ---

func newCurrentContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "current-context",
		Short: "Show the effective context ($PGSERVICE, else the pgdx default)",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			name, source := effectiveContext()
			if name == "" {
				fmt.Fprintln(cmd.ErrOrStderr(), "no current context ($PGSERVICE unset and no pgdx default; set one with: pgdx config use-context <name>)")
				return nil
			}
			fmt.Fprintln(cmd.OutOrStdout(), name)
			fmt.Fprintf(cmd.ErrOrStderr(), "(source: %s)\n", source)
			return nil
		},
	}
}

// newUseContextCmd sets pgdx's default context (kubectl-style). The default is
// pgdx-only state; an explicit --dsn or $PGSERVICE always overrides it.
func newUseContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "use-context <name>",
		Short: "Set the default context pgdx uses when $PGSERVICE is not set",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			// Refuse to default to a context that doesn't exist (kubectl does the same).
			f, _, err := loadServices()
			if err != nil {
				return err
			}
			if _, ok := f.Get(name); !ok {
				return fmt.Errorf("context %q not found in pg_service.conf; create it first with: pgdx config set-context %s ...", name, name)
			}
			cfg, err := pgdxconfig.Load()
			if err != nil {
				return err
			}
			cfg.DefaultContext = name
			if err := cfg.Save(); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "default context set to %q\n", name)
			if env := os.Getenv("PGSERVICE"); env != "" && env != name {
				fmt.Fprintf(cmd.ErrOrStderr(), "note: $PGSERVICE=%q is set and overrides the default for now\n", env)
			}
			return nil
		},
	}
}

// effectiveContext resolves which context is active and why. Precedence:
// $PGSERVICE (env) > pgdx default. (An explicit --dsn, handled at connect time,
// outranks both.)
func effectiveContext() (name, source string) {
	if env := os.Getenv("PGSERVICE"); env != "" {
		return env, "$PGSERVICE"
	}
	if cfg, err := pgdxconfig.Load(); err == nil && cfg.DefaultContext != "" {
		return cfg.DefaultContext, "pgdx default (use-context)"
	}
	return "", ""
}

// applyDefaultContext makes pgdx's stored default take effect for commands that
// connect, by setting PGSERVICE for this process — but ONLY when the user gave no
// explicit connection target. Returns the context name applied, or "".
//
// The pgdx default is the LOWEST-precedence input: an explicit --dsn, $PGSERVICE,
// or $PGHOST/$PGHOSTADDR all mean "connect HERE" and must win. Injecting a service
// over an explicit $PGHOST is a blast-radius bug — a remembered default could send
// a command to prod when the user pointed at localhost.
func applyDefaultContext(dsn string) string {
	if explicitConnTarget(dsn, os.Getenv) {
		return ""
	}
	cfg, err := pgdxconfig.Load()
	if err != nil || cfg.DefaultContext == "" {
		return ""
	}
	os.Setenv("PGSERVICE", cfg.DefaultContext)
	return cfg.DefaultContext
}

// explicitConnTarget reports whether the user already specified where to connect,
// via --dsn or a standard libpq connection env var. When true, pgdx must NOT
// override it with the stored default context.
func explicitConnTarget(dsn string, getenv func(string) string) bool {
	if dsn != "" {
		return true
	}
	for _, k := range []string{"PGSERVICE", "PGHOST", "PGHOSTADDR"} {
		if getenv(k) != "" {
			return true
		}
	}
	return false
}

// --- get-context ---

func newGetContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "get-context <name>",
		Short: "Show one context (passwords are never shown)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			format, err := render.ParseFormat(flagOutput)
			if err != nil {
				return usageError{err.Error()}
			}
			f, _, err := loadServices()
			if err != nil {
				return err
			}
			kv, ok := f.Get(args[0])
			if !ok {
				return fmt.Errorf("context %q not found", args[0])
			}
			if format == render.FormatJSON {
				return render.Render(cmd.OutOrStdout(), format, kv)
			}
			return render.Render(cmd.OutOrStdout(), render.FormatTable, kvView(kv))
		},
	}
}

type kvView map[string]string

func (v kvView) Headers() []string { return []string{"KEY", "VALUE"} }
func (v kvView) Rows() [][]string {
	keys := make([]string, 0, len(v))
	for k := range v {
		keys = append(keys, k)
	}
	for i := 1; i < len(keys); i++ {
		for j := i; j > 0 && keys[j-1] > keys[j]; j-- {
			keys[j-1], keys[j] = keys[j], keys[j-1]
		}
	}
	out := make([][]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, []string{k, v[k]})
	}
	return out
}

// --- set-context (writes pg_service.conf) ---

func newSetContextCmd() *cobra.Command {
	var host, port, user, dbname, sslmode string
	var force bool
	c := &cobra.Command{
		Use:   "set-context <name>",
		Short: "Create or modify a context in pg_service.conf",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			kv := map[string]string{}
			addIf := func(k, v string) {
				if v != "" {
					kv[k] = v
				}
			}
			addIf("host", host)
			addIf("port", port)
			addIf("user", user)
			addIf("dbname", dbname)
			addIf("sslmode", sslmode)
			if len(kv) == 0 {
				return usageError{"nothing to set: pass at least one of --host/--port/--user/--dbname/--sslmode"}
			}
			if err := pgservice.ValidatePort(port); err != nil {
				return usageError{err.Error()}
			}
			if sslmode != "" && !pgservice.ValidSSLModes[sslmode] {
				return usageError{fmt.Sprintf("invalid sslmode %q (want one of disable, allow, prefer, require, verify-ca, verify-full)", sslmode)}
			}

			f, path, err := loadServices()
			if err != nil {
				return err
			}
			if _, exists := f.Get(name); exists && !force {
				return fmt.Errorf("context %q already exists; pass --force to modify it", name)
			}
			f.Set(name, kv)
			if err := f.Save(path); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote context %q to %s\n", name, path)
			fmt.Fprintf(cmd.ErrOrStderr(), "set a password with: pgdx config set-password %s\n", name)
			return nil
		},
	}
	c.Flags().StringVar(&host, "host", "", "server host")
	c.Flags().StringVar(&port, "port", "", "server port")
	c.Flags().StringVar(&user, "user", "", "user name")
	c.Flags().StringVar(&dbname, "dbname", "", "database name")
	c.Flags().StringVar(&sslmode, "sslmode", "", "sslmode (disable|allow|prefer|require|verify-ca|verify-full)")
	c.Flags().BoolVar(&force, "force", false, "modify the context if it already exists")
	return c
}

// --- delete-context (writes pg_service.conf) ---

func newDeleteContextCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "delete-context <name>",
		Short: "Remove a context from pg_service.conf",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			f, path, err := loadServices()
			if err != nil {
				return err
			}
			if !f.Delete(args[0]) {
				return fmt.Errorf("context %q not found", args[0])
			}
			if err := f.Save(path); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "deleted context %q from %s\n", args[0], path)
			fmt.Fprintf(cmd.ErrOrStderr(), "note: any matching ~/.pgpass line was left untouched\n")
			return nil
		},
	}
}

// --- set-password (writes ~/.pgpass) ---

func newSetPasswordCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set-password <name>",
		Short: "Store the password for a context in ~/.pgpass (prompted, never via flag)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			name := args[0]
			f, _, err := loadServices()
			if err != nil {
				return err
			}
			kv, ok := f.Get(name)
			if !ok {
				return fmt.Errorf("context %q not found; create it first with: pgdx config set-context %s ...", name, name)
			}

			pw, err := promptPassword(cmd, fmt.Sprintf("Password for context %q (user %s): ", name, kv["user"]))
			if err != nil {
				return err
			}
			if pw == "" {
				return fmt.Errorf("empty password; nothing written")
			}

			path, err := pgpass.DefaultPath()
			if err != nil {
				return err
			}
			entry := pgpass.Entry{
				Host: kv["host"], Port: kv["port"], Database: kv["dbname"], User: kv["user"], Password: pw,
			}
			if err := pgpass.Upsert(path, entry); err != nil {
				return err
			}
			fmt.Fprintf(cmd.ErrOrStderr(), "wrote password for context %q to %s (mode 0600)\n", name, path)
			return nil
		},
	}
}

// promptPassword reads a password without echoing when stdin is a terminal, and
// falls back to a plain line read when piped. Never reads from argv (no shell history leak).
func promptPassword(cmd *cobra.Command, prompt string) (string, error) {
	errOut := cmd.ErrOrStderr()
	fmt.Fprint(errOut, prompt)
	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		b, err := term.ReadPassword(fd)
		fmt.Fprintln(errOut)
		if err != nil {
			return "", err
		}
		return strings.TrimRight(string(b), "\r\n"), nil
	}
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && line == "" {
		return "", err
	}
	return strings.TrimRight(line, "\r\n"), nil
}
