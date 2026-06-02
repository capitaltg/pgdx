package cmd

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/chzyer/readline"
	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/capitaltg/pgdx/internal/catalog"
	"github.com/capitaltg/pgdx/internal/db"
	"github.com/capitaltg/pgdx/internal/pgdxconfig"
)

// shell turns pgdx's one-shot commands into an interactive session over a SINGLE
// read-only connection. One-shot mode opens (and closes) a fresh connection per
// command; that's fine for the occasional invocation but wasteful when triaging — every
// `get`/`describe`/`status` pays the connect cost again. The shell opens one connection
// at startup, stashes it in the package-level sharedConn, and every subcommand reuses it
// through dial(). The session is read-only like the rest of pgdx, and the connection's
// statement_timeout (D6) still caps every query.
//
// Each typed line is dispatched through a FRESH root command, so the grammar inside the
// shell is identical to the CLI ("get tables", "describe public.users", "query 'select
// 1'"), flags and all. Building the root anew per line also re-binds the persistent-flag
// globals to their defaults (StringVar resets on bind), so -o/-d/--timeout from one line
// never leak into the next.
func newShellCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "shell",
		Short: "Start an interactive session that reuses one read-only connection",
		Long: "shell opens a single read-only connection and keeps it open, so a triage\n" +
			"session runs many commands without reconnecting each time. At the prompt you type\n" +
			"the same commands you'd pass on the CLI — get, describe, status, explain, query,\n" +
			"diff, snapshot — with the same flags. The connection is read-only and every query\n" +
			"still runs under the statement_timeout, exactly like one-shot pgdx.\n\n" +
			"The connection target is resolved once at startup (--dsn / $PGSERVICE / pgdx\n" +
			"default context / -d) and shown in the banner, so per-command context notes stay\n" +
			"quiet. Pass -d DBNAME on a single command to point it at another database (it\n" +
			"dials that one separately), or type `use DBNAME` to switch the whole session\n" +
			"(and `use schema NAME` to set the default schema for unqualified names).\n\n" +
			"On a terminal the prompt is a full line editor: Tab completes commands, flags, and\n" +
			"— for describe table/view/index — object names; ↑/↓ recall prior commands (history\n" +
			"is saved between sessions), Ctrl-R searches it, Ctrl/Alt-←/→ move by word, Home/End\n" +
			"jump to the line ends, and Ctrl-W deletes the previous word. Exit with \\q, exit,\n" +
			"quit, or Ctrl-D.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			if sharedConn != nil {
				return usageError{"already inside a pgdx shell"}
			}
			return runShell(cmd)
		},
	}
}

// runShell opens the session connection, installs it as sharedConn for the duration, and
// runs the read-eval-print loop. The connection is always closed and sharedConn cleared
// on the way out, even if a command panics, so a later one-shot Execute() can't trip over
// a stale shared connection.
func runShell(cmd *cobra.Command) error {
	out := cmd.OutOrStdout()
	errOut := cmd.ErrOrStderr()
	in := cmd.InOrStdin()

	// Resolve the default context exactly as a one-shot command would (sets PGSERVICE for
	// this process when no explicit target is given), so the session connects to the same
	// place the CLI would and -d stays visible in the banner.
	applied := applyDefaultContext(flagDSN)

	ctx := context.Background()
	conn, err := db.Connect(ctx, flagDSN, flagTimeout, flagDatabase, sqlLog(cmd))
	if err != nil {
		return err
	}
	sharedConn = conn
	sessionSchema = ""
	// Close whatever sharedConn currently points at — `use <database>` may have swapped
	// it for a connection to a different database (closing the prior one as it went).
	defer func() {
		if sharedConn != nil {
			_ = sharedConn.Close(ctx)
		}
		sharedConn = nil
	}()

	printBanner(errOut, conn, applied)

	// On a real terminal, use a full line editor (history, word motion, Home/End, …). If
	// it can't initialize (rare), fall back to the plain reader rather than failing the
	// session. Non-terminal input (pipes, tests) always takes the plain reader.
	interactive := term.IsTerminal(int(os.Stdin.Fd()))
	if interactive {
		if err := readlineLoop(out, errOut); !errors.Is(err, errNoRawMode) {
			return err
		}
	}
	return plainLoop(out, errOut, in, interactive)
}

// sessionSchema is the schema set this session via `use schema <name>` (empty = the
// connection's default search_path). It only drives the prompt; the actual search_path
// lives on the server connection. Cleared whenever the database is switched, since a new
// connection starts with its own default search_path.
var sessionSchema string

// shellPrompt renders the prompt for the current session connection, so it always names
// the database in use (updates after `use <database>`) and, when one has been chosen, the
// active schema (`pgdx/db:schema=>` after `use schema <name>`).
func shellPrompt() string {
	if sharedConn == nil {
		return "pgdx=> "
	}
	if sessionSchema != "" {
		return fmt.Sprintf("pgdx/%s:%s=> ", sharedConn.Database(), sessionSchema)
	}
	return fmt.Sprintf("pgdx/%s=> ", sharedConn.Database())
}

// activeCancel is the connection a shell Ctrl-C should cancel. Most commands run on the
// session connection (sharedConn), but write commands (vacuum/analyze) open their own via
// writeConn and register it here for the duration — so Ctrl-C cancels a long ANALYZE or
// VACUUM too, not just queries on the session connection.
var (
	activeCancelMu sync.Mutex
	activeCancel   *db.Conn
)

// setActiveCancel registers (or clears, with nil) the connection a Ctrl-C should cancel.
func setActiveCancel(c *db.Conn) {
	activeCancelMu.Lock()
	activeCancel = c
	activeCancelMu.Unlock()
}

// cancelTarget is the connection to cancel on Ctrl-C: the active write-command connection
// if one is running, otherwise the session connection.
func cancelTarget() *db.Conn {
	activeCancelMu.Lock()
	defer activeCancelMu.Unlock()
	if activeCancel != nil {
		return activeCancel
	}
	return sharedConn
}

// errNoRawMode signals that the interactive line editor couldn't initialize, so runShell
// falls back to the plain (no-editing, no-history) line reader.
var errNoRawMode = errors.New("interactive line editor unavailable")

// readlineLoop runs the interactive REPL with a full line editor (chzyer/readline):
// persistent command history (↑/↓, Ctrl-R), word motion (Ctrl/Alt-←/→), Home/End, and
// word delete — all the editing keys term.Terminal lacked. readline owns the terminal's
// raw mode internally and drops back to cooked mode between lines, so a subcommand's own
// confirm prompt (kill/vacuum) and its output behave exactly as in one-shot pgdx. The
// prompt and line-edit echo are written to errOut (stderr) so stdout stays data-only.
func readlineLoop(out, errOut io.Writer) error {
	rl, err := readline.NewEx(&readline.Config{
		Prompt:          shellPrompt(),
		Stdout:          errOut,
		HistoryFile:     historyFile(),
		HistoryLimit:    1000,
		AutoComplete:    newCompleter(),
		InterruptPrompt: "^C",
		EOFPrompt:       "exit",
	})
	if err != nil {
		return errNoRawMode
	}
	defer rl.Close()

	// While a command runs, Ctrl-C cancels the in-flight query (Postgres out-of-band
	// cancel) instead of killing the shell — so a slow query, explain, or a long
	// ANALYZE/VACUUM doesn't cost you the whole session. cancelTarget() picks the
	// connection actually working (the session connection, or a write command's own). At
	// the prompt, readline's raw mode delivers Ctrl-C as a byte (handled as ErrInterrupt
	// below), so no signal fires there; if one somehow does, cancelling an idle connection
	// is a no-op.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt)
	defer signal.Stop(sigCh)
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		for {
			select {
			case <-sigCh:
				if c := cancelTarget(); c != nil {
					ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
					_ = c.Cancel(ctx)
					cancel()
				}
			case <-stop:
				return
			}
		}
	}()

	for {
		line, err := rl.Readline()
		switch {
		case errors.Is(err, readline.ErrInterrupt):
			// Ctrl-C abandons the current line; on an already-empty line it exits, the
			// convention psql and most REPLs follow.
			if strings.TrimSpace(line) == "" {
				return nil
			}
			continue
		case err == io.EOF: // Ctrl-D
			return nil
		case err != nil:
			return err
		}
		if handleLine(out, errOut, os.Stdin, line) {
			return nil
		}
		rl.SetPrompt(shellPrompt()) // reflect a `use <database>` switch
	}
}

// historyFile returns the path where the shell persists command history, creating the
// pgdx config directory if needed. An empty string (the directory can't be resolved or
// created) tells readline to keep history in memory for the session only.
func historyFile() string {
	dir, err := pgdxconfig.Dir()
	if err != nil {
		return ""
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return ""
	}
	return filepath.Join(dir, "shell_history")
}

// plainLoop runs the REPL with a basic line reader: no editing or history, used when
// stdin isn't a terminal (piped input, tests) or the line editor is unavailable. On a
// terminal ReadString returns a line at a time (canonical mode), so it won't read ahead
// and steal input from a subcommand's own confirm prompt.
func plainLoop(out, errOut io.Writer, in io.Reader, showPrompt bool) error {
	reader := bufio.NewReader(in)
	for {
		if showPrompt {
			fmt.Fprint(errOut, shellPrompt())
		}
		line, err := reader.ReadString('\n')
		if line == "" && err != nil {
			if showPrompt && err == io.EOF {
				fmt.Fprintln(errOut) // close the prompt line on Ctrl-D
			}
			if err == io.EOF {
				return nil
			}
			return err
		}
		atEOF := err == io.EOF
		if handleLine(out, errOut, in, line) {
			return nil
		}
		if atEOF {
			return nil
		}
	}
}

// handleLine processes one input line: skips blanks and # comments, handles the exit and
// help built-ins, then tokenizes and dispatches the rest. It returns true when the
// session should end (exit/quit/\q).
func handleLine(out, errOut io.Writer, in io.Reader, line string) (quit bool) {
	line = strings.TrimSpace(line)
	if line == "" || strings.HasPrefix(line, "#") {
		return false
	}
	switch line {
	case "exit", "quit", `\q`:
		return true
	case "help", `\?`:
		printShellHelp(errOut)
		return false
	}
	args, err := splitArgs(line)
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return false
	}
	if len(args) == 0 {
		return false
	}
	// `use <database>` is a shell built-in, not a cobra command: it repoints the whole
	// session at another database (one-shot pgdx has no session to switch, so it would be
	// meaningless on the CLI).
	if args[0] == "use" {
		if len(args) >= 2 && args[1] == "schema" {
			switchSchema(errOut, args[2:])
		} else {
			switchDatabase(errOut, args[1:])
		}
		return false
	}
	dispatch(out, errOut, in, args)
	return false
}

// switchDatabase implements `use <database>`: it opens a connection to another database
// on the same server (reusing the session's host/user/auth/timeout) and swaps it in only
// if that succeeds, so a typo'd or unreachable database leaves the current session
// untouched. The object-name completion cache is dropped, since it's database-specific.
func switchDatabase(errOut io.Writer, args []string) {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(errOut, "usage: use <database>")
		return
	}
	if sharedConn == nil { // only reachable outside a session; defensive
		fmt.Fprintln(errOut, "error: no active connection")
		return
	}
	if args[0] == sharedConn.Database() {
		fmt.Fprintf(errOut, "already connected to %q\n", args[0])
		return
	}
	ctx := context.Background()
	newConn, err := sharedConn.ConnectDatabase(ctx, args[0])
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return
	}
	_ = sharedConn.Close(ctx)
	sharedConn = newConn
	sessionSchema = "" // the new connection starts with its own default search_path
	completionNames.reset()
	fmt.Fprintf(errOut, "now connected to %q on %s:%d (read-only)\n",
		newConn.Database(), newConn.Host(), newConn.Port())
}

// switchSchema implements `use schema <name>`: it sets the session search_path to a
// single schema so unqualified names in `query` resolve there. Unlike a database switch
// this needs no reconnect — search_path is a session setting on the existing connection.
// The schema is validated first so a typo is rejected rather than silently ignored.
func switchSchema(errOut io.Writer, args []string) {
	if len(args) != 1 || args[0] == "" {
		fmt.Fprintln(errOut, "usage: use schema <name>")
		return
	}
	if sharedConn == nil { // only reachable outside a session; defensive
		fmt.Fprintln(errOut, "error: no active connection")
		return
	}
	ctx := context.Background()
	ok, err := catalog.SchemaExists(ctx, sharedConn, args[0])
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return
	}
	if !ok {
		fmt.Fprintf(errOut, "error: schema %q not found (see: get schemas)\n", args[0])
		return
	}
	applied, err := catalog.SetSearchPath(ctx, sharedConn, args[0])
	if err != nil {
		fmt.Fprintln(errOut, "error:", err)
		return
	}
	sessionSchema = args[0]
	fmt.Fprintf(errOut, "search_path is now %s\n", applied)
}

// dispatch runs one typed line through a fresh root command. Errors are printed to stderr
// (mirroring Execute) but never end the session — the next prompt follows. The exit code
// of a failed command is irrelevant inside the shell, so it's dropped.
func dispatch(out, errOut io.Writer, in io.Reader, args []string) {
	root := newRootCmd()
	root.SetOut(out)
	root.SetErr(errOut)
	root.SetIn(in)
	root.SetArgs(args)
	if err := root.Execute(); err != nil {
		fmt.Fprintln(errOut, "error:", err)
	}
}

// printBanner names the connection target once at startup so per-command notes can stay
// quiet for the rest of the session (noteContext is a no-op while sharedConn is set).
func printBanner(w io.Writer, conn *db.Conn, appliedContext string) {
	fmt.Fprintf(w, "pgdx shell — read-only by default, on %s:%d/%s\n",
		conn.Host(), conn.Port(), conn.Database())
	switch {
	case appliedContext != "" && flagDatabase != "":
		fmt.Fprintf(w, "using default context %q (database %q via -d)\n", appliedContext, flagDatabase)
	case appliedContext != "":
		fmt.Fprintf(w, "using default context %q\n", appliedContext)
	case flagDatabase != "":
		fmt.Fprintf(w, "connecting to database %q (via -d)\n", flagDatabase)
	}
	fmt.Fprintln(w, `Type a pgdx command (e.g. "get tables"), "help" for the command list, or \q to quit.`)
}

// printShellHelp shows the available commands by dispatching the root's own help, so the
// list never drifts from the real command tree. (shell appears in it too; re-running it
// inside a session is refused by the sharedConn guard.)
func printShellHelp(w io.Writer) {
	dispatch(w, w, nil, []string{"help"})
	fmt.Fprintln(w, "\nShell built-ins: use <database> (switch database), use schema <name> (set default schema), help, exit / quit (\\q).")
}

// splitArgs tokenizes a typed line into command arguments with shell-like quoting, so a
// SQL string survives as one argument: query "select * from t where x = 'a'". Outside
// quotes, whitespace separates tokens and a backslash escapes the next character. Double
// quotes group text and honor backslash escapes; single quotes group text literally (no
// escapes), which is what keeps SQL string literals intact. An unterminated quote is an
// error rather than a silently-merged token.
func splitArgs(line string) ([]string, error) {
	var args []string
	var cur strings.Builder
	var quote rune // 0, '\'' or '"'
	started := false

	flush := func() {
		if started {
			args = append(args, cur.String())
			cur.Reset()
			started = false
		}
	}

	runes := []rune(line)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		switch {
		case quote == '\'':
			if c == '\'' {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case quote == '"':
			if c == '"' {
				quote = 0
			} else if c == '\\' && i+1 < len(runes) {
				i++
				cur.WriteRune(runes[i])
			} else {
				cur.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote = c
			started = true
		case c == '\\' && i+1 < len(runes):
			i++
			cur.WriteRune(runes[i])
			started = true
		case c == ' ' || c == '\t':
			flush()
		default:
			cur.WriteRune(c)
			started = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("unterminated %c quote", quote)
	}
	flush()
	return args, nil
}
