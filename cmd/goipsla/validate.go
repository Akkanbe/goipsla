package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"goipsla/internal/config"
)

func init() { registerCommand(newValidateCmd) }

func newValidateCmd(opts *rootOptions) *cobra.Command {
	var printCfg, showSecrets bool
	cmd := &cobra.Command{
		Use:   "validate <file>",
		Short: "Check a configuration file without contacting the daemon",
		Long: "Check a configuration file without contacting the daemon.\n\n" +
			"On success it prints \"OK: <N> operations\". On failure it prints every\n" +
			"error as \"<path>: <message>\" on standard error and exits with status 1.\n" +
			"Settings that are accepted but have no effect are printed as\n" +
			"\"warning: <path>: <message>\" on standard error; they do not fail.\n" +
			"With --print it prints the effective configuration (defaults filled in,\n" +
			"templates expanded) as a table or, with -o json, as JSON only. The JSON\n" +
			"masks the SNMP communities and everything after the host of webhook\n" +
			"URLs as \"***\" unless --show-secrets is given.\n\n" +
			"With -o json and without --print, the result is a JSON object on standard\n" +
			"output: {\"ok\": true, \"operations\": N, \"warnings\": [...]} or, with\n" +
			"status 1, {\"ok\": false, \"errors\": [\"<path>: <message>\", ...]}.",
		Args: fileArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			w := cmd.OutOrStdout()
			jsonResult := opts.Output == outputJSON && !printCfg
			cfg, err := config.Load(args[0])
			if err != nil {
				var ve *config.ValidationError
				if jsonResult {
					res := validateFailed{Errors: []string{err.Error()}}
					if errors.As(err, &ve) {
						res.Errors = res.Errors[:0]
						for _, fe := range ve.Errors {
							res.Errors = append(res.Errors, fe.String())
						}
					}
					if err := writeJSON(w, res); err != nil {
						return err
					}
					return &exitError{Code: 1}
				}
				if !errors.As(err, &ve) {
					return err
				}
				for _, fe := range ve.Errors {
					fmt.Fprintln(cmd.ErrOrStderr(), fe.String())
				}
				return &exitError{Code: 1}
			}
			if jsonResult {
				warnings := cfg.Warnings
				if warnings == nil {
					warnings = []string{}
				}
				return writeJSON(w, validateOK{OK: true, Operations: len(cfg.Operations), Warnings: warnings})
			}
			for _, warn := range cfg.Warnings {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %s\n", warn)
			}
			if printCfg {
				if opts.Output == outputJSON {
					raw, err := config.EffectiveConfigJSON(cfg, !showSecrets)
					if err != nil {
						return err
					}
					var buf bytes.Buffer
					if err := json.Indent(&buf, raw, "", "  "); err != nil {
						return err
					}
					buf.WriteByte('\n')
					_, err = buf.WriteTo(w)
					return err
				}
				if err := writeValidateTable(w, cfg); err != nil {
					return err
				}
			}
			_, err = fmt.Fprintf(w, "OK: %d operations\n", len(cfg.Operations))
			return err
		},
	}
	cmd.Flags().BoolVar(&printCfg, "print", false, "print the effective configuration")
	cmd.Flags().BoolVar(&showSecrets, "show-secrets", false, "with --print -o json, show SNMP communities and webhook URLs unmasked")
	return cmd
}

// validateOK and validateFailed are the output of validate -o json without
// --print. Errors are "<path>: <message>" as in the table output.
type (
	validateOK struct {
		OK         bool     `json:"ok"` // true
		Operations int      `json:"operations"`
		Warnings   []string `json:"warnings"`
	}
	validateFailed struct {
		OK     bool     `json:"ok"` // false
		Errors []string `json:"errors"`
	}
)

func writeValidateTable(w io.Writer, cfg *config.Config) error {
	tw := fmtTable(w)
	fmt.Fprintln(tw, "ID\tTYPE\tTARGET\tFREQUENCY\tTIMEOUT\tTHRESHOLD\tTAG")
	for _, op := range cfg.Operations {
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%s\t%s\t%s\n", op.ID, op.Type, op.TargetName,
			config.FormatDuration(op.Frequency), config.FormatMillis(op.Timeout), config.FormatMillis(op.Threshold), op.Tag)
	}
	return tw.Flush()
}
