package main

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"goipsla/internal/config"
)

// noArgs rejects positional arguments with a usage error.
//
//declscope:package // shared CLI helper
func noArgs(cmd *cobra.Command, args []string) error {
	if len(args) > 0 {
		return usageError(cmd, fmt.Errorf("unexpected argument %q", args[0]))
	}
	return nil
}

// idArgs accepts exactly one operation ID, or at most one when optional.
//
//declscope:package // shared CLI helper
func idArgs(optional bool) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		switch {
		case len(args) > 1:
			return usageError(cmd, fmt.Errorf("too many arguments: %s", strings.Join(args, " ")))
		case len(args) == 0 && !optional:
			return usageError(cmd, fmt.Errorf("missing operation id"))
		case len(args) == 1:
			if _, err := parseIDArg(args[0]); err != nil {
				return usageError(cmd, err)
			}
		}
		return nil
	}
}

//declscope:package // shared CLI helper
func parseIDArg(s string) (int, error) {
	id, err := strconv.Atoi(s)
	if err != nil || id < 1 || id > config.MaxOpID {
		return 0, fmt.Errorf("invalid operation id %q: must be between 1 and %d", s, config.MaxOpID)
	}
	return id, nil
}

// fileArg accepts exactly one file name (goipsla validate).
//
//declscope:package // shared CLI helper
func fileArg(cmd *cobra.Command, args []string) error {
	switch {
	case len(args) == 0:
		return usageError(cmd, fmt.Errorf("missing configuration file"))
	case len(args) > 1:
		return usageError(cmd, fmt.Errorf("too many arguments: %s", strings.Join(args, " ")))
	}
	return nil
}
