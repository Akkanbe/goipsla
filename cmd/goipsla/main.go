// Command goipsla is the command-line client of goipslad. It talks to the
// daemon over its local Unix socket only.
package main

import (
	"errors"
	"fmt"
	"os"
)

func main() {
	if err := newRootCmd().Execute(); err != nil {
		var ee *exitError
		if errors.As(err, &ee) {
			os.Exit(ee.Code)
		}
		fmt.Fprintf(os.Stderr, "goipsla: %v\n", err)
		os.Exit(1)
	}
}
