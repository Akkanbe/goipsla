// show reactions (namespace "reaction").
//
//declscope:namespace reaction

package main

import (
	"fmt"
	"strconv"

	"github.com/spf13/cobra"
)

//declscope:package // the show command adds it as a subcommand
func newShowReactionsCmd(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "reactions [<id>]",
		Short: "Show the threshold reactions and whether they occurred (like show ip sla reaction-configuration)",
		Long: "Show every reaction row (ip sla reaction-configuration) of an operation, or of\n" +
			"every operation, with its thresholds and its state: OCCURRED is true while the\n" +
			"threshold is violated, VALUE is the last evaluated value.",
		Args: idArgs(true),
		RunE: func(cmd *cobra.Command, args []string) error {
			id := 0
			if len(args) == 1 {
				id, _ = parseIDArg(args[0])
			}
			rows, err := newClient(opts).Reactions(cmd.Context(), id)
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if opts.Output == outputJSON {
				return writeJSON(w, rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.ErrOrStderr(), "no reactions")
				return nil
			}
			tw := fmtTable(w)
			fmt.Fprintln(tw, "OP\tELEMENT\tTYPE\tRISING\tFALLING\tCOUNT\tX/Y\tACTION\tOCCURRED\tVALUE\tLAST-CHANGE")
			for _, r := range rows {
				rising, falling := strconv.Itoa(r.Upper), strconv.Itoa(r.Lower)
				if booleanReactionElement(r.Element) {
					rising, falling = "-", "-"
				}
				count, xy := "-", "-"
				switch r.ThresholdType {
				case "consecutive", "average":
					count = strconv.Itoa(r.Count)
				case "xofy":
					xy = fmt.Sprintf("%d/%d", r.X, r.Y)
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%t\t%d\t%s\n",
					r.OpID, r.Element, r.ThresholdType, rising, falling, count, xy, r.Action,
					r.Occurred, r.Value, fmtAgo(r.LastChange))
			}
			return tw.Flush()
		},
	}
}

// booleanReactionElement reports whether a reaction element has no thresholds
// (the value is 1 on violation, 0 on recovery).
func booleanReactionElement(element string) bool {
	return element == "timeout" || element == "verifyError"
}
