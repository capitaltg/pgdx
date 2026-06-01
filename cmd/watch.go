package cmd

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// watchLoop re-renders a command on a fixed interval until the user interrupts it (Ctrl-C
// or SIGTERM), the shared engine behind `--watch` on status and `get activity`. Each tick
// calls render. For a terminal (table) view it clears the screen first and prints a
// footer, like `watch`; for JSON (isJSON) it streams one snapshot per tick with no
// clearing, so the output stays valid for piping.
func watchLoop(cmd *cobra.Command, interval time.Duration, isJSON bool, render func() error) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	out := cmd.OutOrStdout()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if !isJSON {
			fmt.Fprint(out, "\033[H\033[2J") // cursor home, clear screen
		}
		if err := render(); err != nil {
			return err
		}
		if !isJSON {
			fmt.Fprintf(out, "\nrefreshing every %s — Ctrl-C to stop\n", interval)
		}
		select {
		case <-ctx.Done():
			if !isJSON {
				fmt.Fprintln(cmd.ErrOrStderr()) // leave the cursor on a fresh line
			}
			return nil
		case <-ticker.C:
		}
	}
}
