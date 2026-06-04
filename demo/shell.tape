# pgdx demo: one-shot CLI commands, then the resident interactive shell.
# Rendered with VHS (https://github.com/charmbracelet/vhs).
# Regenerate with: demo/record.sh  (builds pgdx, spins up a demo DB, runs VHS).

Output shell.gif

Set Shell bash
Set FontSize 16
Set Width 1200
Set Height 760
Set Padding 20
Set Margin 16
Set BorderRadius 10
Set WindowBar Colorful
Set Theme "Dracula"
Set TypingSpeed 55ms

# Off-screen setup: demo binary on PATH, connection via PG* env vars (so the
# commands on screen stay clean), and a minimal prompt.
Hide
Type "export PATH=/vhs:$PATH" Enter
Type "export PGHOST=pgdx-demo-pg PGPORT=5432 PGUSER=demo PGPASSWORD=demo PGDATABASE=shop PGSSLMODE=disable" Enter
Type "PS1='$ '" Enter
Type "clear" Enter
Show

# --- One-shot CLI: kubectl-style verbs, no session needed. ---
Type "pgdx get tables"
Sleep 400ms
Enter
Sleep 2.5s

Type `pgdx explain --analyze --suggest-index "select * from order_items where unit_price > 99.5"`
Sleep 400ms
Enter
Sleep 4.5s

# --- ...or stay resident: the same commands on one read-only connection. ---
Type "pgdx shell   # ...or stay resident — same commands, one connection"
Sleep 600ms
Enter
Sleep 2s

# Tab-completion fills in live object names.
Type "describe table cust"
Sleep 800ms
Tab
Sleep 1.2s
Enter
Sleep 3s

# Ad-hoc read-only query, rendered like everything else.
Type `query "select status, count(*) from orders group by 1 order by 2 desc"`
Sleep 400ms
Enter
Sleep 3s

Type "\q"
Sleep 300ms
Enter
Sleep 1.5s
