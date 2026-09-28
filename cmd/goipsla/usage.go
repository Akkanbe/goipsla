package main

import (
	"fmt"

	"github.com/spf13/cobra"
)

// usageError reports a usage mistake and makes goipsla exit with status 2.
//
//declscope:package // shared CLI helper
func usageError(cmd *cobra.Command, err error) error {
	w := cmd.ErrOrStderr()
	fmt.Fprintf(w, "goipsla: %v\n", err)
	fmt.Fprintf(w, "Run '%s --help' for usage.\n", cmd.CommandPath())
	return &exitError{Code: 2}
}
