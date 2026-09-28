//go:build race

// raceEnabled for the collector tests; it shares their namespace.
//
//declscope:namespace metrics

package metrics

const raceEnabled = true
