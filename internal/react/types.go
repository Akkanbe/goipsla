//declscope:core // the public state types (react.ReactionState, react.TrackState): the package API itself

package react

import (
	"time"

	"goipsla/internal/op"
)

// ReactionState is the state of one reaction of an operation.
type ReactionState struct {
	Element       string
	ThresholdType string
	Upper, Lower  int
	Count, X, Y   int
	Action        string
	Occurred      bool
	Value         int64     // last value of the element (the raw value, also for average); 0 after a reset
	LastChange    time.Time // when Occurred last changed; zero if never
	Changes       int
}

// TrackState is the state of one track.
type TrackState struct {
	ID         int
	Operation  int
	Mode       string
	State      string // up | down | unknown
	Pending    string // the state a running delay leads to; empty if none
	Changes    int
	LastChange time.Time
	LatestRC   op.ReturnCode // RCOther before the first attempt
	LatestRTT  time.Duration // valid for ok / overThreshold
	DelayUp    time.Duration
	DelayDown  time.Duration
}
