// show statistics aggregated (namespace "aggregated").
//
//declscope:namespace aggregated

package main

import (
	"fmt"
	"io"
	"slices"
	"strconv"

	"github.com/spf13/cobra"

	"goipsla/internal/api"
	"goipsla/internal/config"
	"goipsla/internal/stats"
)

// newShowAggregatedCmd is "show statistics aggregated".
//
//declscope:package // show statistics adds it as a subcommand
func newShowAggregatedCmd(opts *rootOptions) *cobra.Command {
	var details bool
	cmd := &cobra.Command{
		Use:   "aggregated [<id>]",
		Short: "Show the hourly statistics (like show ip sla statistics aggregated)",
		Args:  idArgs(true),
		RunE: func(cmd *cobra.Command, args []string) error {
			ds, err := fetchDetailsToShow(cmd, opts, args, stats.SnapshotOptions{Hours: true})
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if opts.Output == outputJSON {
				if len(args) == 1 {
					return writeJSON(w, ds[0])
				}
				return writeJSON(w, ds)
			}
			if len(ds) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no operations")
				return nil
			}
			if len(args) == 1 && len(ds[0].Hours) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no hour groups")
				return nil
			}
			if details && slices.ContainsFunc(ds, func(d *api.OperationDetail) bool { return d.Type == config.ICMPJitter }) {
				fmt.Fprintln(cmd.ErrOrStderr(), "distribution is not kept for icmp-jitter")
			}
			for i, d := range ds {
				if i > 0 {
					fmt.Fprintln(w)
				}
				if err := writeAggregated(w, d, details); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&details, "details", false, "add the distribution buckets of each hour group (icmp-echo only; icmp-jitter keeps no distribution)")
	return cmd
}

func writeAggregated(w io.Writer, d *api.OperationDetail, details bool) error {
	fmt.Fprintf(w, "Operation %d (%s %s)\n", d.ID, d.Type, d.Target)
	if len(d.Hours) == 0 {
		fmt.Fprintln(w, "no hour groups")
		return nil
	}
	if d.Type == config.ICMPJitter {
		return writeJitterAggregated(w, d)
	}
	tw := fmtTable(w)
	fmt.Fprintln(tw, "INDEX\tSTART\tRTTs\tMIN/AVG/MAX\tSUCC\tFAIL\tOVERTH\tTIMEOUT\tBUSY\tDROP\tSEQERR\tVERERR")
	for _, h := range d.Hours {
		c := h.Counters
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			h.Index, fmtClock(h.Start), fmtUint(c.Completions), fmtMinAvgMax(c), fmtUint(c.Successes), fmtUint(c.Failures),
			fmtUint(c.OverThresholds), fmtUint(c.Timeouts), fmtUint(c.Busies), fmtUint(c.Drops), fmtUint(c.SequenceErrors), fmtUint(c.VerifyErrors))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if !details {
		return nil
	}
	for _, h := range d.Hours {
		fmt.Fprintf(w, "\nDistribution of hour group %d:\n", h.Index)
		tw := fmtTable(w)
		fmt.Fprintln(tw, "RANGE\tCOMPLETIONS\tOVERTH\t%\tAVG(ms)")
		for _, b := range h.Dist {
			avg := "-"
			if b.Completions > 0 {
				avg = fmtMs(b.RTTAvgMs)
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", fmtAggregatedRange(b), fmtUint(b.Completions), fmtUint(b.OverThresholds),
				strconv.FormatFloat(b.Percent, 'f', 1, 64), avg)
		}
		if err := tw.Flush(); err != nil {
			return err
		}
	}
	return nil
}

// fmtAggregatedRange prints a distribution bucket range: "0-<20ms", ">=20ms".
func fmtAggregatedRange(b api.DistBucketJSON) string {
	if b.UpperMs == nil {
		return ">=" + fmtUint(b.LowerMs) + "ms"
	}
	return fmtUint(b.LowerMs) + "-<" + fmtUint(*b.UpperMs) + "ms"
}
