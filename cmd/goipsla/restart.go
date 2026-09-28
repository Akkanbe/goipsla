package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

func init() { registerCommand(newRestartCmd) }

func newRestartCmd(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "restart <id>",
		Short: "Discard the statistics of an active operation and start a new life",
		Long: `Discard the statistics of an active operation and start a new life now
(Cisco "ip sla restart"). The life is reloaded from the configuration and the
sequence numbers start again at 1. Only an active operation can be restarted.`,
		Args: idArgs(false),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, _ := parseIDArg(args[0])
			if err := newClient(opts).Restart(cmd.Context(), id); err != nil {
				return err
			}
			if opts.Output == outputJSON {
				return writeJSON(cmd.OutOrStdout(), map[string]any{"restarted": id})
			}
			_, err := fmt.Fprintf(cmd.OutOrStdout(), "operation %d restarted\n", id)
			return err
		},
	}
	return cmd
}
