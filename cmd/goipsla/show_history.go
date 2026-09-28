// show history and show enhanced-history (namespace "history").
//
//declscope:namespace history

package main

import (
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"goipsla/internal/api"
	"goipsla/internal/stats"
)

//declscope:package // the show command adds it as a subcommand
func newShowHistoryCmd(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "history [<id>]",
		Short: "Show the history buckets of an operation (like show ip sla history tabular)",
		Long: "With an id, show that operation's history buckets. Without an id, show\n" +
			"those of every operation (fetched in one request), one section each.",
		Args: idArgs(true),
		RunE: func(cmd *cobra.Command, args []string) error {
			return showHistoryParts(cmd, opts, args, stats.SnapshotOptions{History: true},
				func(d *api.OperationDetail) bool { return len(d.History) > 0 },
				"no history buckets", writeHistory)
		},
	}
}

//declscope:package // the show command adds it as a subcommand
func newShowEnhancedHistoryCmd(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "enhanced-history [<id>]",
		Short: "Show the enhanced history buckets of an operation (like show ip sla enhanced-history)",
		Long: "With an id, show that operation's enhanced history buckets. Without an\n" +
			"id, show those of every operation (fetched in one request), one section each.",
		Args: idArgs(true),
		RunE: func(cmd *cobra.Command, args []string) error {
			return showHistoryParts(cmd, opts, args, stats.SnapshotOptions{Enhanced: true},
				func(d *api.OperationDetail) bool { return len(d.Enhanced) > 0 },
				"no enhanced history buckets", writeEnhancedHistory)
		},
	}
}

// showHistoryParts prints one part of the detail (history or enhanced history) for
// the operation in args, or for every operation when args is empty. With an
// id and nothing to show, it prints empty on stderr, as before; for every
// operation it prints a section per operation, with empty under its heading
// when it has nothing.
func showHistoryParts(cmd *cobra.Command, opts *rootOptions, args []string, parts stats.SnapshotOptions,
	has func(*api.OperationDetail) bool, empty string, write func(io.Writer, *api.OperationDetail) error,
) error {
	ds, err := fetchDetailsToShow(cmd, opts, args, parts)
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
	if len(args) == 1 {
		if !has(ds[0]) {
			fmt.Fprintln(cmd.ErrOrStderr(), empty)
			return nil
		}
		return write(w, ds[0])
	}
	if len(ds) == 0 {
		fmt.Fprintln(cmd.ErrOrStderr(), "no operations")
		return nil
	}
	for i, d := range ds {
		if i > 0 {
			fmt.Fprintln(w)
		}
		fmt.Fprintf(w, "Operation %d (%s %s)\n", d.ID, d.Type, d.Target)
		if !has(d) {
			fmt.Fprintln(w, empty)
			continue
		}
		if err := write(w, d); err != nil {
			return err
		}
	}
	return nil
}

func writeHistory(w io.Writer, d *api.OperationDetail) error {
	tw := fmtTable(w)
	fmt.Fprintln(tw, "LIFE\tBUCKET\tSAMPLE\tSTART\tRTT(ms)\tSENSE\tTARGET")
	for _, b := range d.History {
		fmt.Fprintf(tw, "%d\t%d\t%d\t%s\t%s\t%s\t%s\n",
			b.Life, b.Bucket, b.Sample, fmtClock(b.Start), fmtUint(b.RTTMs), b.Code, b.Target)
	}
	return tw.Flush()
}

func writeEnhancedHistory(w io.Writer, d *api.OperationDetail) error {
	tw := fmtTable(w)
	fmt.Fprintln(tw, "INDEX\tSTART\tCOMPS\tOVERTH\tSUM\tSUM2\tMIN\tMAX\tTIMEOUT\tBUSY\tDROP\tSEQERR\tVERERR")
	for _, e := range d.Enhanced {
		c := e.Counters
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			e.Index, fmtClock(e.Start), fmtUint(c.Completions), fmtUint(c.OverThresholds), fmtUint(c.RTTSumMs), fmtUint(c.RTTSum2Ms),
			fmtUint(c.RTTMinMs), fmtUint(c.RTTMaxMs), fmtUint(c.Timeouts), fmtUint(c.Busies), fmtUint(c.Drops), fmtUint(c.SequenceErrors), fmtUint(c.VerifyErrors))
	}
	return tw.Flush()
}
