package main

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func init() { registerCommand(newHealthCmd) }

func newHealthCmd(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "health",
		Short: "Show the daemon version, start time and operation counts",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			h, err := newClient(opts).Health(cmd.Context())
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if opts.Output == outputJSON {
				return writeJSON(w, h)
			}
			up := fmtNow().Sub(h.StartedAt).Truncate(time.Second)
			tw := fmtTable(w)
			fmt.Fprintf(tw, "Version:\t%s\n", h.Version)
			fmt.Fprintf(tw, "Started:\t%s (up %s)\n", fmtStamp(h.StartedAt), up)
			fmt.Fprintf(tw, "Config:\t%s\n", fmtDash(h.ConfigPath))
			o := h.Operations
			fmt.Fprintf(tw, "Operations:\t%d total, %d active, %d pending, %d inactive\n", o.Total, o.Active, o.Pending, o.Inactive)
			return tw.Flush()
		},
	}
	return cmd
}
