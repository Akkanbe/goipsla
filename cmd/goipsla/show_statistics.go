// show statistics (namespace "statistics").
//
//declscope:namespace statistics

package main

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"goipsla/internal/api"
	"goipsla/internal/config"
	"goipsla/internal/stats"
)

//declscope:package // the show command adds it as a subcommand
func newShowStatisticsCmd(opts *rootOptions) *cobra.Command {
	var details bool
	cmd := &cobra.Command{
		Use:   "statistics [<id>]",
		Short: "Show the latest result and totals (like show ip sla statistics)",
		Long: "Without an id, list every operation as show operations does (or, with\n" +
			"--details, show each one vertically). With an id, show that operation\n" +
			"vertically in the style of Cisco's show ip sla statistics.",
		Args: idArgs(true),
		RunE: func(cmd *cobra.Command, args []string) error {
			c := newClient(opts)
			ctx := cmd.Context()
			if len(args) == 1 {
				id, _ := parseIDArg(args[0])
				d, err := c.Operation(ctx, id, stats.SnapshotOptions{})
				if err != nil {
					return err
				}
				if opts.Output == outputJSON {
					return writeJSON(cmd.OutOrStdout(), d)
				}
				return writeStatistics(cmd.OutOrStdout(), d, details)
			}
			if !details {
				rows, err := c.Operations(ctx, api.OperationFilter{})
				if err != nil {
					return err
				}
				return printOperations(cmd, opts, rows)
			}
			// Every operation's detail in one request.
			ds, err := c.OperationDetails(ctx, api.OperationFilter{}, stats.SnapshotOptions{})
			if err != nil {
				return err
			}
			if opts.Output == outputJSON {
				return writeJSON(cmd.OutOrStdout(), ds)
			}
			if len(ds) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no operations")
				return nil
			}
			w := cmd.OutOrStdout()
			for i := range ds {
				if i > 0 {
					fmt.Fprintln(w)
				}
				if err := writeStatistics(w, &ds[i], true); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&details, "details", false, "include every counter of the current life")
	cmd.AddCommand(newShowAggregatedCmd(opts))
	return cmd
}

// writeStatistics prints one operation vertically, after Cisco's
// show ip sla statistics [details].
func writeStatistics(w io.Writer, d *api.OperationDetail, details bool) error {
	if d.Type == config.ICMPJitter {
		return writeJitterStatistics(w, d, details)
	}
	tw := fmtTable(w)
	kv := func(k, v string) { fmt.Fprintf(tw, "%s:\t%s\n", k, v) }
	l, t := d.Latest, d.Totals
	kv("IPSLA operation id", strconv.Itoa(d.ID))
	kv("Type of operation", string(d.Type))
	target := d.Target
	if d.VRF != "" {
		target += " (vrf " + d.VRF + ")"
	}
	kv("Target address", target)
	if d.Tag != "" {
		kv("Tag", d.Tag)
	}
	switch {
	case !l.Valid:
		kv("Latest RTT", "Unknown")
	case l.RTTMs != nil:
		kv("Latest RTT", fmtMs(*l.RTTMs)+" milliseconds")
	default:
		kv("Latest RTT", "NoConnection/Busy/Timeout")
	}
	if l.Start != nil {
		kv("Latest operation start time", fmtStamp(*l.Start))
	} else {
		kv("Latest operation start time", "-")
	}
	kv("Latest operation return code", l.Code.Display())
	if l.Detail != "" {
		kv("Latest operation detail", l.Detail)
	}
	kv("Number of successes", fmtUint(t.Successes))
	kv("Number of failures", fmtUint(t.Failures))
	kv("Operational state", d.State)
	kv("Life start", fmt.Sprintf("%s (life %d)", fmtStamp(d.LifeStart), d.LifeIndex))
	if details {
		kv("Initiations", fmtUint(t.Initiations))
		kv("Completions", fmtUint(t.Completions))
		kv("Over thresholds", fmtUint(t.OverThresholds))
		kv("Timeouts", fmtUint(t.Timeouts))
		kv("Busies", fmtUint(t.Busies))
		kv("Drops", fmtUint(t.Drops))
		kv("Sequence errors", fmtUint(t.SequenceErrors))
		kv("Verify errors", fmtUint(t.VerifyErrors))
		kv("RTT Min/Avg/Max", fmtMinAvgMax(t)+" milliseconds")
		kv("RTT standard deviation", fmtMs(t.RTTStdDevMs)+" milliseconds")
		kv("RTT sum / sum of squares", fmtUint(t.RTTSumMs)+" / "+fmtUint(t.RTTSum2Ms))
	}
	return tw.Flush()
}
