package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"goipsla/internal/api"
	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/stats"
)

func init() { registerCommand(newShowCmd) }

func newShowCmd(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Show operations, statistics, history and configuration",
		Args:  cobra.ArbitraryArgs,
		RunE:  runGroup,
	}
	cmd.AddCommand(
		newShowOperationsCmd(opts),
		newShowStatisticsCmd(opts),
		newShowHistoryCmd(opts),
		newShowEnhancedHistoryCmd(opts),
		newShowConfigCmd(opts),
		newShowReactionsCmd(opts),
		newShowTrackCmd(opts),
		newShowEventsCmd(opts),
	)
	return cmd
}

// showFilter holds the list filters shared by show operations and watch.
//
//declscope:package // shared CLI helper
type showFilter struct{ api.OperationFilter }

//declscope:package // shared CLI helper
func (f *showFilter) add(cmd *cobra.Command) {
	fl := cmd.Flags()
	fl.StringVar(&f.Tag, "tag", "", "only operations with this tag")
	fl.StringVar(&f.State, "state", "", "only operations in this state: pending, inactive or active")
	fl.StringVar(&f.RC, "rc", "", "only operations whose latest return code is this (MIB name, e.g. ok, timeout, overThreshold)")
	fl.StringVar(&f.Type, "type", "", "only operations of this type: icmp-echo or icmp-jitter")
}

//declscope:package // shared CLI helper
func (f *showFilter) check() error {
	switch f.State {
	case "", api.StatePending, api.StateInactive, api.StateActive:
	default:
		return fmt.Errorf("invalid --state %q: must be pending, inactive or active", f.State)
	}
	if f.RC != "" {
		if _, err := op.ParseReturnCode(f.RC); err != nil {
			return fmt.Errorf("invalid --rc: %w", err)
		}
	}
	switch f.Type {
	case "", string(config.ICMPEcho), string(config.ICMPJitter):
	default:
		return fmt.Errorf("invalid --type %q: must be icmp-echo or icmp-jitter", f.Type)
	}
	return nil
}

// String renders the set filters as flags, for the watch header.
func (f *showFilter) String() string {
	var parts []string
	for _, kv := range [][2]string{{"tag", f.Tag}, {"state", f.State}, {"rc", f.RC}, {"type", f.Type}} {
		if kv[1] != "" {
			parts = append(parts, "--"+kv[0]+" "+kv[1])
		}
	}
	return strings.Join(parts, " ")
}

// fetchDetailsToShow returns the detail of the operation named in args, or of
// every operation in one request when args is empty.
//
//declscope:package // the id-optional show commands share it
func fetchDetailsToShow(cmd *cobra.Command, opts *rootOptions, args []string, parts stats.SnapshotOptions) ([]*api.OperationDetail, error) {
	c := newClient(opts)
	if len(args) == 1 {
		id, _ := parseIDArg(args[0])
		d, err := c.Operation(cmd.Context(), id, parts)
		if err != nil {
			return nil, err
		}
		return []*api.OperationDetail{d}, nil
	}
	ds, err := c.OperationDetails(cmd.Context(), api.OperationFilter{}, parts)
	if err != nil {
		return nil, err
	}
	out := make([]*api.OperationDetail, len(ds))
	for i := range ds {
		out[i] = &ds[i]
	}
	return out, nil
}
