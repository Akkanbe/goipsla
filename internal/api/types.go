//declscope:core // the contract types (api.Provider, api.OperationRow, api.Error, ...): the package API itself

// Package api is the goipslad control API: HTTP/1.1 with JSON bodies over a
// local Unix domain socket. The daemon runs a Server around a Provider; the
// goipsla CLI uses a Client.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"goipsla/internal/config"
	"goipsla/internal/event"
	"goipsla/internal/op"
	"goipsla/internal/stats"
)

// Provider is the daemon-side data source of the Server. It ties the manager
// and the statistics store together (implemented in cmd/goipslad).
type Provider interface {
	Health() Health
	Operations() []OperationRow // sorted by ID
	Operation(id int, opts stats.SnapshotOptions) (*OperationDetail, bool)
	// Reload, Restart and Reset change the running daemon.
	Reload(ctx context.Context) (*ReloadResult, error)
	Restart(ctx context.Context, id int) error
	Reset(ctx context.Context) error
}

// Errors a Provider returns to select the HTTP status, and that a Client
// wraps when it receives that status.
var (
	ErrNotImplemented = errors.New("not implemented")         // 501
	ErrNotFound       = errors.New("not found")               // 404
	ErrNotActive      = errors.New("operation is not active") // 409, e.g. restart of a pending operation
)

// A Provider that returns a *config.ValidationError (for example from
// Reload) gets 400 with the body
// {"error": "invalid configuration", "errors": ["<path>: <msg>", ...]}.

// Operational states of an operation.
const (
	StatePending  = "pending"
	StateInactive = "inactive"
	StateActive   = "active"
)

// Health is the response of GET /v1/health.
type Health struct {
	Version    string    `json:"version"`
	StartedAt  time.Time `json:"started_at"`
	ConfigPath string    `json:"config_path"`
	Operations struct {
		Total    int `json:"total"`
		Active   int `json:"active"`
		Pending  int `json:"pending"`
		Inactive int `json:"inactive"`
	} `json:"operations"`
}

// OperationRow is one line of GET /v1/operations.
type OperationRow struct {
	ID     int           `json:"id"`
	Type   config.OpType `json:"type"`
	Target string        `json:"target"`
	Tag    string        `json:"tag,omitempty"`
	VRF    string        `json:"vrf,omitempty"`
	State  string        `json:"state"` // pending | inactive | active
	Latest LatestJSON    `json:"latest"`
	Totals CountersJSON  `json:"totals"`

	// Schedule information (P4), filled in by the Provider.
	LifeLeftS *int64     `json:"life_left_s,omitempty"`   // seconds of life left; 0 when not active; omitted for a forever life
	NextStart *time.Time `json:"next_start,omitempty"`    // next start of a pending / inactive operation, if known
	Ageout    *int64     `json:"ageout_left_s,omitempty"` // seconds until a non-active operation is aged out
}

// LatestJSON is the latest attempt (rttMonLatestRttOper*).
type LatestJSON struct {
	Valid  bool          `json:"valid"`
	Seq    uint32        `json:"seq,omitempty"`
	Start  *time.Time    `json:"start,omitempty"`
	End    *time.Time    `json:"end,omitempty"`
	RTTMs  *float64      `json:"rtt_ms,omitempty"` // only for ok / overThreshold
	Code   op.ReturnCode `json:"code"`
	Detail string        `json:"detail,omitempty"`
	// Jitter is the latest burst of an icmp-jitter operation (P3).
	Jitter *JitterResultJSON `json:"jitter,omitempty"`
}

// CountersJSON is stats.Counters with the derived values.
type CountersJSON struct {
	Initiations    uint64  `json:"initiations"`
	Completions    uint64  `json:"completions"`
	OverThresholds uint64  `json:"over_thresholds"`
	Timeouts       uint64  `json:"timeouts"`
	Busies         uint64  `json:"busies"`
	Drops          uint64  `json:"drops"`
	SequenceErrors uint64  `json:"sequence_errors"`
	VerifyErrors   uint64  `json:"verify_errors"`
	Successes      uint64  `json:"successes"`
	Failures       uint64  `json:"failures"`
	RTTSumMs       uint64  `json:"rtt_sum_ms"`
	RTTSum2Ms      uint64  `json:"rtt_sum2_ms"`
	RTTMinMs       uint64  `json:"rtt_min_ms"`
	RTTMaxMs       uint64  `json:"rtt_max_ms"`
	RTTAvgMs       float64 `json:"rtt_avg_ms"`
	RTTStdDevMs    float64 `json:"rtt_stddev_ms"`
}

// OperationDetail is the response of GET /v1/operations/{id}.
type OperationDetail struct {
	OperationRow
	Config    json.RawMessage      `json:"config"` // config.EffectiveJSON
	LifeStart time.Time            `json:"life_start"`
	LifeIndex int                  `json:"life_index"`
	Hours     []HourGroupJSON      `json:"hours,omitempty"`
	History   []HistoryBucketJSON  `json:"history,omitempty"`
	Enhanced  []EnhancedBucketJSON `json:"enhanced,omitempty"`
	// TotalsJitter accumulates the bursts of the current life of an
	// icmp-jitter operation (P3).
	TotalsJitter *JitterCountersJSON `json:"totals_jitter,omitempty"`
}

// HourGroupJSON is one hour group of the aggregated statistics.
type HourGroupJSON struct {
	Index    int              `json:"index"`
	Start    time.Time        `json:"start"`
	Counters CountersJSON     `json:"counters"`
	Dist     []DistBucketJSON `json:"dist"` // empty for icmp-jitter
	// Jitter accumulates the bursts of the hour (icmp-jitter only, P3).
	Jitter *JitterCountersJSON `json:"jitter,omitempty"`
}

// DistBucketJSON is one distribution bucket of an hour group.
type DistBucketJSON struct {
	Index       int     `json:"index"`
	LowerMs     uint64  `json:"lower_ms"`
	UpperMs     *uint64 `json:"upper_ms"` // null for the last bucket
	Completions uint64  `json:"completions"`
	// OverThresholds is the part of Completions over the threshold.
	OverThresholds uint64  `json:"over_thresholds"`
	RTTSumMs       uint64  `json:"rtt_sum_ms"`
	RTTSum2Ms      uint64  `json:"rtt_sum2_ms"`
	RTTMinMs       uint64  `json:"rtt_min_ms"`
	RTTMaxMs       uint64  `json:"rtt_max_ms"`
	RTTAvgMs       float64 `json:"rtt_avg_ms"`
	Percent        float64 `json:"percent"` // share of the hour group's completions, 0..100
}

// HistoryBucketJSON is one history bucket.
type HistoryBucketJSON struct {
	Life   int           `json:"life"`
	Bucket int           `json:"bucket"`
	Sample int           `json:"sample"`
	Start  time.Time     `json:"start"`
	RTTMs  uint64        `json:"rtt_ms"`
	Code   op.ReturnCode `json:"code"`
	Target string        `json:"target"`
}

// EnhancedBucketJSON is one enhanced history bucket.
type EnhancedBucketJSON struct {
	Index    int          `json:"index"`
	Start    time.Time    `json:"start"`
	Counters CountersJSON `json:"counters"`
}

// OperationFilter selects rows of GET /v1/operations. Empty fields match
// everything; set fields are ANDed.
type OperationFilter struct {
	Tag   string
	State string
	RC    string // MIB name, case-insensitive
	Type  string
}

// ReloadResult is the response of POST /v1/reload.
type ReloadResult struct {
	Added     []int `json:"added"`
	Removed   []int `json:"removed"`
	Restarted []int `json:"restarted"` // rebuilt because a measurement setting changed
	Updated   []int `json:"updated"`   // only reactions changed

	Warnings []string `json:"warnings,omitempty"` // global settings that need a daemon restart (P4)
}

// JitterSideJSON is op.JitterSide: one kind of jitter sample, or the one-way
// delays of one direction, in whole milliseconds.
type JitterSideJSON struct {
	Num    uint64  `json:"num"`
	SumMs  uint64  `json:"sum_ms"`
	Sum2Ms uint64  `json:"sum2_ms"`
	MinMs  uint64  `json:"min_ms"`
	MaxMs  uint64  `json:"max_ms"`
	AvgMs  float64 `json:"avg_ms"`
}

// JitterResultJSON is op.JitterResult, one icmp-jitter burst.
type JitterResultJSON struct {
	NumPackets       int            `json:"num_packets"`
	Sent             int            `json:"sent"`
	Skipped          int            `json:"skipped"`
	NumRTT           uint64         `json:"num_rtt"`
	RTTSumMs         uint64         `json:"rtt_sum_ms"`
	RTTSum2Ms        uint64         `json:"rtt_sum2_ms"`
	RTTMinMs         uint64         `json:"rtt_min_ms"`
	RTTMaxMs         uint64         `json:"rtt_max_ms"`
	RTTAvgMs         float64        `json:"rtt_avg_ms"`
	NumOverThreshold uint64         `json:"num_over_threshold"`
	PosSD            JitterSideJSON `json:"pos_sd"`
	NegSD            JitterSideJSON `json:"neg_sd"`
	PosDS            JitterSideJSON `json:"pos_ds"`
	NegDS            JitterSideJSON `json:"neg_ds"`
	AvgJitterMs      float64        `json:"avg_jitter_ms"`
	AvgSDJitterMs    float64        `json:"avg_sd_jitter_ms"`
	AvgDSJitterMs    float64        `json:"avg_ds_jitter_ms"`
	PktLoss          uint64         `json:"pkt_loss"`
	PktLateArrival   uint64         `json:"pkt_late_arrival"`
	PktOutSeqSD      uint64         `json:"pkt_out_seq_sd"`
	PktOutSeqDS      uint64         `json:"pkt_out_seq_ds"`
	PktOutSeqBoth    uint64         `json:"pkt_out_seq_both"`
	MinSucPktLoss    uint64         `json:"min_suc_pkt_loss"`
	MaxSucPktLoss    uint64         `json:"max_suc_pkt_loss"`
	OneWay           bool           `json:"one_way"`
	NumOW            uint64         `json:"num_ow"`
	OWSD             JitterSideJSON `json:"ow_sd"`
	OWDS             JitterSideJSON `json:"ow_ds"`
}

// JitterCountersJSON is stats.JitterCounters, the bursts accumulated over
// an hour group or the current life.
type JitterCountersJSON struct {
	NumRTT           uint64         `json:"num_rtt"`
	RTTSumMs         uint64         `json:"rtt_sum_ms"`
	RTTSum2Ms        uint64         `json:"rtt_sum2_ms"`
	RTTMinMs         uint64         `json:"rtt_min_ms"`
	RTTMaxMs         uint64         `json:"rtt_max_ms"`
	RTTAvgMs         float64        `json:"rtt_avg_ms"`
	NumOverThreshold uint64         `json:"num_over_threshold"`
	PosSD            JitterSideJSON `json:"pos_sd"`
	NegSD            JitterSideJSON `json:"neg_sd"`
	PosDS            JitterSideJSON `json:"pos_ds"`
	NegDS            JitterSideJSON `json:"neg_ds"`
	AvgJitterMs      float64        `json:"avg_jitter_ms"`
	AvgSDJitterMs    float64        `json:"avg_sd_jitter_ms"`
	AvgDSJitterMs    float64        `json:"avg_ds_jitter_ms"`
	PktLoss          uint64         `json:"pkt_loss"`
	PktLateArrival   uint64         `json:"pkt_late_arrival"`
	PktOutSeqSD      uint64         `json:"pkt_out_seq_sd"`
	PktOutSeqDS      uint64         `json:"pkt_out_seq_ds"`
	PktOutSeqBoth    uint64         `json:"pkt_out_seq_both"`
	MinSucPktLoss    uint64         `json:"min_suc_pkt_loss"`
	MaxSucPktLoss    uint64         `json:"max_suc_pkt_loss"`
	Skipped          uint64         `json:"skipped"`
	NumOW            uint64         `json:"num_ow"`
	OWSD             JitterSideJSON `json:"ow_sd"`
	OWDS             JitterSideJSON `json:"ow_ds"`
}

// errorBody is the body of every error response. Errors lists the
// individual validation errors of a 400 "invalid configuration".
//
//declscope:package // the error body the server writes and the client reads
type errorBody struct {
	Error  string   `json:"error"`
	Errors []string `json:"errors,omitempty"`
}

// EventProvider is implemented by a Provider that also serves reactions,
// tracks and events (P5). The Server answers 501 on /v1/reactions,
// /v1/tracks and /v1/events for a Provider without it.
type EventProvider interface {
	// Reactions returns the reaction rows of operation id (0: of every
	// operation, by ID). ok is false for an unknown id.
	Reactions(id int) (rows []ReactionJSON, ok bool)
	// Tracks returns track id (0: every track, by ID). ok is false for an
	// unknown id.
	Tracks(id int) (tracks []TrackJSON, ok bool)
	// Events returns up to limit of the latest events, oldest first.
	Events(limit int) []EventJSON
}

// ReactionJSON is one reaction row ("ip sla reaction-configuration") and
// its state, in GET /v1/reactions.
type ReactionJSON struct {
	OpID          int        `json:"op_id"`
	Element       string     `json:"element"`
	ThresholdType string     `json:"threshold_type"`
	Upper         int        `json:"upper"`
	Lower         int        `json:"lower"`
	Count         int        `json:"count,omitempty"`
	X             int        `json:"x,omitempty"`
	Y             int        `json:"y,omitempty"`
	Action        string     `json:"action"`
	Occurred      bool       `json:"occurred"`
	Value         int64      `json:"value"`                 // last evaluated value (the moving average for average)
	LastChange    *time.Time `json:"last_change,omitempty"` // when occurred last changed
	Changes       int        `json:"changes"`
}

// TrackJSON is one track ("track N ip sla OP state|reachability") in
// GET /v1/tracks.
type TrackJSON struct {
	ID          int           `json:"id"`
	Operation   int           `json:"operation"`
	Mode        string        `json:"mode"`              // state | reachability
	State       string        `json:"state"`             // up | down | unknown
	Pending     string        `json:"pending,omitempty"` // the state a running delay leads to
	Changes     int           `json:"changes"`
	LastChange  *time.Time    `json:"last_change,omitempty"`
	LatestRC    op.ReturnCode `json:"latest_rc"`
	LatestRTTMs *float64      `json:"latest_rtt_ms,omitempty"` // ok / overThreshold only
	DelayUp     string        `json:"delay_up"`                // e.g. "5s"
	DelayDown   string        `json:"delay_down"`
}

// EventJSON is one event in GET /v1/events. It is event.Event, whose JSON
// form is the one webhooks and exec hooks receive.
type EventJSON = event.Event

// DefaultEventLimit and MaxEventLimit bound GET /v1/events?limit=.
const (
	DefaultEventLimit = 100
	MaxEventLimit     = 1000
)

// Error is an error response of the API. It wraps ErrNotFound for 404,
// ErrNotActive for 409 and ErrNotImplemented for 501. For a 400 "invalid
// configuration", Errors holds the individual "<path>: <msg>" lines.
type Error struct {
	Status  int
	Message string
	Errors  []string
}

// Error returns the message. With validation errors it joins them on one
// line in the format of config.ValidationError.Error, e.g.
// "invalid configuration: 2 errors: a: x; b: y"; print Errors one per line
// for a terminal.
func (e *Error) Error() string {
	switch n := len(e.Errors); n {
	case 0:
		return e.Message
	case 1:
		return e.Message + ": " + e.Errors[0]
	default:
		return e.Message + ": " + strconv.Itoa(n) + " errors: " + strings.Join(e.Errors, "; ")
	}
}

// Unwrap maps the status to the package errors.
func (e *Error) Unwrap() error {
	switch e.Status {
	case http.StatusNotFound:
		return ErrNotFound
	case http.StatusNotImplemented:
		return ErrNotImplemented
	case http.StatusConflict:
		return ErrNotActive
	}
	return nil
}

// match reports whether row passes the filter. f must have been checked.
//
//declscope:package // the operations handler filters its rows with it
func (f OperationFilter) match(row OperationRow) bool {
	if f.Tag != "" && row.Tag != f.Tag {
		return false
	}
	if f.State != "" && row.State != f.State {
		return false
	}
	if f.RC != "" {
		rc, err := op.ParseReturnCode(f.RC)
		if err != nil || row.Latest.Code != rc {
			return false
		}
	}
	if f.Type != "" && string(row.Type) != f.Type {
		return false
	}
	return true
}
