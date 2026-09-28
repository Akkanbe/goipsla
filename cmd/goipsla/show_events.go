package main

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/spf13/cobra"

	"goipsla/internal/api"
)

//declscope:package // the show command adds it as a subcommand
func newShowEventsCmd(opts *rootOptions) *cobra.Command {
	var limit int
	cmd := &cobra.Command{
		Use:   "events",
		Short: "Show the latest events (threshold and track notifications), oldest first",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if limit < 1 || limit > api.MaxEventLimit {
				return usageError(cmd, errors.New("invalid --limit "+strconv.Itoa(limit)+": must be between 1 and "+strconv.Itoa(api.MaxEventLimit)))
			}
			evs, err := newClient(opts).Events(cmd.Context(), limit)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if opts.Output == outputJSON {
				return writeJSON(w, evs)
			}
			if len(evs) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no events")
				return nil
			}
			tw := fmtTable(w)
			fmt.Fprintln(tw, "TIME\tKIND\tOP\tTARGET\tDETAIL")
			for _, ev := range evs {
				fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%s\n", fmtStamp(ev.Time), ev.Kind, ev.OpID, fmtDash(ev.Target), ev.Message)
			}
			return tw.Flush()
		},
	}
	cmd.Flags().IntVar(&limit, "limit", api.DefaultEventLimit, "number of events to show (1-1000)")
	return cmd
}
