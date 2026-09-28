// show operations and the operation list shared with show statistics and watch (namespace "operations").
//
//declscope:namespace operations

package main

import (
	"fmt"
	"io"
	"strconv"

	"github.com/spf13/cobra"

	"goipsla/internal/api"
)

//declscope:package // the show command adds it as a subcommand
func newShowOperationsCmd(opts *rootOptions) *cobra.Command {
	var f showFilter
	cmd := &cobra.Command{
		Use:   "operations",
		Short: "List every operation on one line each (like show ip sla summary)",
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if err := f.check(); err != nil {
				return usageError(cmd, err)
			}
			rows, err := newClient(opts).Operations(cmd.Context(), f.OperationFilter)
			if err != nil {
				return err
			}
			return printOperations(cmd, opts, rows)
		},
	}
	f.add(cmd)
	return cmd
}

// printOperations prints the operation list, or "no operations" on stderr.
//
//declscope:package // show statistics without an id prints the same list
func printOperations(cmd *cobra.Command, opts *rootOptions, rows []api.OperationRow) error {
	if opts.Output == outputJSON {
		if rows == nil {
			rows = []api.OperationRow{}
		}
		return writeJSON(cmd.OutOrStdout(), rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "no operations")
		return nil
	}
	return writeOperationsTable(cmd.OutOrStdout(), rows)
}

// writeOperationsTable prints the operation list as a table.
//
//declscope:package // watch redraws the same table
func writeOperationsTable(w io.Writer, rows []api.OperationRow) error {
	tw := fmtTable(w)
	fmt.Fprintln(tw, "ID\tTYPE\tTARGET\tTAG\tSTATE\tRC\tRTT(ms)\tLAST\tSUCC\tFAIL\tAVG(ms)\tJIT(ms)\tLOSS")
	for _, r := range rows {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			strconv.Itoa(r.ID), r.Type, r.Target, fmtDash(r.Tag), r.State,
			fmtRC(r.Latest), fmtRTT(r.Latest), fmtAgo(r.Latest.End),
			fmtUint(r.Totals.Successes), fmtUint(r.Totals.Failures), fmtAvg(r.Totals),
			fmtOperationsJitter(r.Latest.Jitter), fmtOperationsLoss(r.Latest.Jitter))
	}
	return tw.Flush()
}

// fmtOperationsJitter prints the mean jitter of the latest burst; "-" for icmp-echo
// or before the first burst. For icmp-jitter, RTT(ms) is the burst's mean
// RTT.
func fmtOperationsJitter(j *api.JitterResultJSON) string {
	if j == nil {
		return "-"
	}
	return fmtMs(j.AvgJitterMs)
}

// fmtOperationsLoss prints the packets lost in the latest burst out of those
// sent, e.g. "1/10" (skipped packets are in neither count, as in the result
// detail "loss 1/10").
func fmtOperationsLoss(j *api.JitterResultJSON) string {
	if j == nil {
		return "-"
	}
	return fmtUint(j.PktLoss) + "/" + strconv.Itoa(j.Sent)
}
