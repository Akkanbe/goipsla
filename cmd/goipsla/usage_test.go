package main

import (
	"errors"
	"strings"
	"testing"
)

// TestUsageErrorsExit2: every usage mistake exits with 2 and points to --help
// (docs/cli.md, "Exit status"), including those cobra reports before a command
// runs.
func TestUsageErrorsExit2(t *testing.T) {
	for _, tc := range []struct {
		args []string
		msg  string
	}{
		{[]string{"-o", "yaml", "show", "operations"}, `goipsla: invalid output format "yaml": must be table or json` + "\nRun 'goipsla show operations --help' for usage.\n"},
		{[]string{"--bogus"}, "goipsla: unknown flag: --bogus\nRun 'goipsla --help' for usage.\n"},
		{[]string{"nosuchcmd"}, `goipsla: unknown command "nosuchcmd" for "goipsla"` + "\nRun 'goipsla --help' for usage.\n"},
		{[]string{"show", "operatoins"}, `goipsla: unknown command "operatoins" for "goipsla show"` + "\nRun 'goipsla show --help' for usage.\n"},
		{[]string{"validate"}, "goipsla: missing configuration file\nRun 'goipsla validate --help' for usage.\n"},
		{[]string{"validate", "a", "b"}, "goipsla: too many arguments: a b\nRun 'goipsla validate --help' for usage.\n"},
		{[]string{"validate", "--bogus", "x"}, "goipsla: unknown flag: --bogus\nRun 'goipsla validate --help' for usage.\n"},
		{[]string{"version", "--bogus"}, "goipsla: unknown flag: --bogus\nRun 'goipsla version --help' for usage.\n"},
	} {
		out, errOut, err := runCtl(t, append([]string{"--socket", "/nonexistent"}, tc.args...)...)
		var ee *exitError
		if !errors.As(err, &ee) || ee.Code != 2 {
			t.Errorf("%v: err %v, want exit 2", tc.args, err)
		}
		if out != "" || errOut != tc.msg {
			t.Errorf("%v: stdout %q\nstderr %q\nwant   %q", tc.args, out, errOut, tc.msg)
		}
	}
}

// TestGroupWithoutArgsPrintsHelp: goipsla and goipsla show without a
// subcommand still print the help and succeed.
func TestGroupWithoutArgsPrintsHelp(t *testing.T) {
	for _, args := range [][]string{{}, {"show"}} {
		out, errOut, err := runCtl(t, args...)
		if err != nil || errOut != "" || !strings.Contains(out, "Available Commands:") {
			t.Errorf("%v: err %v stderr %q stdout %q", args, err, errOut, out)
		}
	}
}
