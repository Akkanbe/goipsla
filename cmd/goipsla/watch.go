package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
)

// clearWatchScreen moves the cursor home and clears the terminal.
const clearWatchScreen = "\033[H\033[2J"

func init() { registerCommand(newWatchCmd) }

func newWatchCmd(opts *rootOptions) *cobra.Command {
	var (
		f        showFilter
		interval time.Duration
		count    int
	)
	cmd := &cobra.Command{
		Use:   "watch",
		Short: "Refresh the operation list periodically until Ctrl-C",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.Output == outputJSON {
				return usageError(cmd, errors.New("watch does not support -o json; use show operations -o json"))
			}
			if interval <= 0 {
				return usageError(cmd, fmt.Errorf("invalid --interval %s: must be positive", interval))
			}
			if err := f.check(); err != nil {
				return usageError(cmd, err)
			}
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			return runWatch(ctx, cmd, opts, &f, interval, count)
		},
	}
	f.add(cmd)
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "refresh interval")
	cmd.Flags().IntVar(&count, "count", 0, "stop after this many refreshes (0: until interrupted)")
	_ = cmd.Flags().MarkHidden("count") // for tests; the flag exists, so this cannot fail
	return cmd
}

// runWatch redraws the list every interval. Connection errors are shown on
// the screen and retried, so watch survives a daemon restart.
func runWatch(ctx context.Context, cmd *cobra.Command, opts *rootOptions, f *showFilter, interval time.Duration, count int) error {
	c := newClient(opts)
	w := cmd.OutOrStdout()
	title := fmt.Sprintf("Every %s: goipsla show operations", interval)
	if s := f.String(); s != "" {
		title += " " + s
	}
	for n := 1; ; n++ {
		var buf bytes.Buffer
		buf.WriteString(clearWatchScreen)
		fmt.Fprintf(&buf, "%s    %s\n\n", title, fmtNow().Format("2006-01-02 15:04:05"))
		rows, err := c.Operations(ctx, f.OperationFilter)
		switch {
		case ctx.Err() != nil:
			return nil
		case err != nil:
			fmt.Fprintf(&buf, "error: %v\n", err)
		case len(rows) == 0:
			buf.WriteString("no operations\n")
		default:
			if err := writeOperationsTable(&buf, rows); err != nil {
				return err
			}
		}
		if _, err := buf.WriteTo(w); err != nil {
			return err
		}
		if count > 0 && n >= count {
			return nil
		}
		t := time.NewTimer(interval)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C:
		}
	}
}
