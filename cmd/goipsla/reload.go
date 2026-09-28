package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/spf13/cobra"

	"goipsla/internal/api"
)

func init() { registerCommand(newReloadCmd) }

func newReloadCmd(opts *rootOptions) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "reload",
		Short: "Make goipslad read its configuration file again",
		Long: `Make goipslad read its configuration file again and apply the differences.

Operations whose measurement settings changed are rebuilt with new
statistics; operations where only tag, owner or react changed keep their
statistics. An invalid file is not applied at all: goipslad keeps running the
previous configuration and the errors are printed one per line.`,
		Args: noArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := newClient(opts).Reload(cmd.Context())
			if err != nil {
				return reloadError(cmd.ErrOrStderr(), err)
			}
			w := cmd.OutOrStdout()
			if opts.Output == outputJSON {
				return writeJSON(w, res)
			}
			printReload(w, res)
			return nil
		},
	}
	return cmd
}

func printReload(w io.Writer, res *api.ReloadResult) {
	tw := fmtTable(w)
	fmt.Fprintf(tw, "added:\t%s\n", reloadIDList(res.Added))
	fmt.Fprintf(tw, "removed:\t%s\n", reloadIDList(res.Removed))
	fmt.Fprintf(tw, "restarted:\t%s\n", reloadIDList(res.Restarted))
	fmt.Fprintf(tw, "updated:\t%s\n", reloadIDList(res.Updated))
	tw.Flush()
	for _, warn := range res.Warnings {
		fmt.Fprintf(w, "warning: %s\n", warn)
	}
}

func reloadIDList(ids []int) string {
	s := make([]string, len(ids))
	for i, id := range ids {
		s[i] = fmt.Sprint(id)
	}
	return "[" + strings.Join(s, " ") + "]"
}

// reloadError prints a rejected configuration one error per line. The API
// answers 400 with every validation error in api.Error.Errors; other errors
// are returned as they are.
func reloadError(w io.Writer, err error) error {
	var ae *api.Error
	if !errors.As(err, &ae) || ae.Status != http.StatusBadRequest || len(ae.Errors) == 0 {
		return err
	}
	fmt.Fprintf(w, "goipsla: reload failed; goipslad keeps the running configuration: %s:\n", ae.Message)
	for _, e := range ae.Errors {
		fmt.Fprintf(w, "  %s\n", e)
	}
	return &exitError{Code: 1}
}
