//declscope:core // Load / Parse are the entry points of the package (the API itself)

package config

import (
	"fmt"
	"os"
	"time"

	"goipsla/internal/clock"
)

// now is the reference time for start-time HH:MM[:SS]. Tests replace it.
var now = func() time.Time { return clock.Real().Now() }

// Load reads the file at path and parses it with Parse.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	return Parse(data)
}

// Parse decodes YAML, expands templates, fills in defaults and validates the
// result. Validation failures are returned as a *ValidationError holding every
// error found. YAML syntax errors are returned the same way, as a single
// FieldError with an empty Path.
//
// Unknown keys are rejected (the equivalent of yaml.v3's KnownFields(true));
// the node tree is walked by hand so that every error is collected with its
// path instead of stopping at the first one.
func Parse(data []byte) (*Config, error) {
	return parse(data, now())
}
