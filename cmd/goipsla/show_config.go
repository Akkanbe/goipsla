package main

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/spf13/cobra"

	"goipsla/internal/stats"
)

//declscope:package // the show command adds it as a subcommand
func newShowConfigCmd(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "config <id>",
		Short: "Show the effective configuration of an operation (like show ip sla configuration)",
		Long: "Show the effective configuration of an operation: defaults filled in and\n" +
			"templates expanded, with the key names of the configuration file. With\n" +
			"-o json it prints the configuration object only.",
		Args: idArgs(false),
		RunE: func(cmd *cobra.Command, args []string) error {
			id, _ := parseIDArg(args[0])
			d, err := newClient(opts).Operation(cmd.Context(), id, stats.SnapshotOptions{})
			if err != nil {
				return err
			}
			w := cmd.OutOrStdout()
			if opts.Output == outputJSON {
				var buf bytes.Buffer
				if err := json.Indent(&buf, d.Config, "", "  "); err != nil {
					return fmt.Errorf("decode config: %w", err)
				}
				buf.WriteByte('\n')
				_, err := buf.WriteTo(w)
				return err
			}
			return writeYAMLish(w, d.Config)
		},
	}
}
