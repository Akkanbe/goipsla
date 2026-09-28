// The root command, the global options and the error convention of goipsla:
// the core of package main, as main.go is for goipslad.
//
//declscope:core

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"runtime"

	"github.com/spf13/cobra"

	"goipsla/internal/api"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

const defaultSocket = "/run/goipslad/goipslad.sock"

// Values of -o/--output.
const (
	outputTable = "table"
	//declscope:package // shared CLI helper
	outputJSON = "json"
)

// rootOptions holds the persistent flags shared by every subcommand.
//
//declscope:package // shared CLI helper
type rootOptions struct {
	Output string // outputTable or outputJSON
	Socket string // path of the goipslad API socket
}

func (o *rootOptions) validate() error {
	switch o.Output {
	case outputTable, outputJSON:
		return nil
	default:
		return fmt.Errorf("invalid output format %q: must be table or json", o.Output)
	}
}

// commandFactories builds the subcommands that live in their own files.
// Such a file registers its constructor from init so that root.go does not
// change when commands are added:
//
//	func init() { registerCommand(newValidateCmd) }
//
//	func newValidateCmd(opts *rootOptions) *cobra.Command { ... }
var commandFactories []func(*rootOptions) *cobra.Command

// registerCommand adds a subcommand constructor. Call it only from init.
//
//declscope:package // shared CLI helper
func registerCommand(f func(*rootOptions) *cobra.Command) {
	commandFactories = append(commandFactories, f)
}

// exitError makes main exit with Code without printing anything; the command
// has already reported the problem (e.g. validation errors, one per line).
//
//declscope:package // shared CLI helper
type exitError struct{ Code int }

func (e *exitError) Error() string { return fmt.Sprintf("exit status %d", e.Code) }

//declscope:package // shared CLI helper
func newRootCmd() *cobra.Command {
	// Run the root's PersistentPreRunE even when a subcommand defines its own.
	cobra.EnableTraverseRunHooks = true

	opts := &rootOptions{}
	cmd := &cobra.Command{
		Use:           "goipsla",
		Short:         "Inspect and control the goipslad IP SLA daemon",
		Version:       version,
		SilenceUsage:  true,
		SilenceErrors: true,
		Args:          cobra.ArbitraryArgs,
		RunE:          runGroup,
		PersistentPreRunE: func(cmd *cobra.Command, _ []string) error {
			if err := opts.validate(); err != nil {
				return usageError(cmd, err)
			}
			return nil
		},
	}
	cmd.SetVersionTemplate("goipsla {{.Version}}\n")
	pf := cmd.PersistentFlags()
	pf.StringVarP(&opts.Output, "output", "o", outputTable, "output format: table or json")
	pf.StringVar(&opts.Socket, "socket", defaultSocket, "path of the goipslad API socket")

	cmd.AddCommand(newVersionCmd(opts))
	for _, f := range commandFactories {
		cmd.AddCommand(f(opts))
	}
	// Flag errors of every command exit with 2; subcommands inherit it.
	cmd.SetFlagErrorFunc(usageError)
	return cmd
}

func newVersionCmd(opts *rootOptions) *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the goipsla version",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w := cmd.OutOrStdout()
			if opts.Output == outputJSON {
				return writeJSON(w, map[string]string{
					"version":  version,
					"go":       runtime.Version(),
					"platform": runtime.GOOS + "/" + runtime.GOARCH,
				})
			}
			_, err := fmt.Fprintf(w, "goipsla %s (%s %s/%s)\n", version, runtime.Version(), runtime.GOOS, runtime.GOARCH)
			return err
		},
	}
}

// runGroup is the RunE of a command that only groups subcommands (goipsla,
// goipsla show): without arguments it prints the help; an argument is a
// subcommand cobra did not find, a usage error. Without a RunE, cobra would
// print the help and exit 0 for a misspelled subcommand.
//
//declscope:package // shared CLI helper
func runGroup(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError(cmd, fmt.Errorf("unknown command %q for %q", args[0], cmd.CommandPath()))
	}
	return cmd.Help()
}

// writeJSON writes v as indented JSON followed by a newline.
//
//declscope:package // shared CLI helper
func writeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

// newClient returns the API client for the socket in the root options.
//
//declscope:package // shared CLI helper
func newClient(opts *rootOptions) *api.Client { return api.NewClient(opts.Socket) }
