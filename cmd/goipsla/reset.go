package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

func init() { registerCommand(newResetCmd) }

func newResetCmd(opts *rootOptions) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "reset",
		Short: "Discard the statistics of every operation and restart all schedules",
		Long: `Discard the statistics of every operation and put every schedule back into
its initial state, as when goipslad starts. Unlike Cisco "ip sla reset", the
configuration is kept: goipslad runs the configuration file it last loaded.

reset asks for confirmation; --yes skips the question and is required when
standard input is not a terminal.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !yes {
				if !isTerminalForReset(cmd.InOrStdin()) {
					return usageError(cmd, errors.New("reset needs --yes when standard input is not a terminal"))
				}
				ok, err := confirmReset(cmd.InOrStdin(), cmd.ErrOrStderr(),
					"Discard the statistics of every operation and restart all schedules? [y/N] ")
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(cmd.ErrOrStderr(), "reset canceled")
					return &exitError{Code: 1}
				}
			}
			if err := newClient(opts).Reset(cmd.Context()); err != nil {
				return err
			}
			if opts.Output == outputJSON {
				return writeJSON(cmd.OutOrStdout(), map[string]bool{"reset": true})
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "all operations reset")
			return err
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "do not ask for confirmation")
	return cmd
}

// isTerminalForReset reports whether r is a terminal. It is a variable so that
// tests can pretend to be interactive.
var isTerminalForReset = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	// A character device is not enough (/dev/null is one): ask for the
	// terminal attributes.
	_, err := unix.IoctlGetTermios(int(f.Fd()), unix.TCGETS)
	return err == nil
}

// confirmReset asks question on w and reads a yes / no answer from r.
func confirmReset(r io.Reader, w io.Writer, question string) (bool, error) {
	fmt.Fprint(w, question)
	line, err := bufio.NewReader(r).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return false, err
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	}
	return false, nil
}
