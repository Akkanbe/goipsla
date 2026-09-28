// Package apitest provides a fake api.Provider with fixed data and a helper
// that runs an api.Server on a temporary socket, for tests of the API and of
// goipsla.
package apitest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"goipsla/internal/api"
	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/stats"
)

// Base is the fixed "now" of the fake data: 2026-09-27 12:00:00 UTC.
var Base = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// Provider is a fake api.Provider. Its fields may be changed before use.
type Provider struct {
	mu sync.Mutex

	HealthV api.Health
	Rows    []api.OperationRow
	Details map[int]*api.OperationDetail

	ReloadErr, RestartErr, ResetErr error
	ReloadRes                       *api.ReloadResult

	// Calls records the provider calls, e.g. "operation 101 hours,history".
	Calls []string
}

func (p *Provider) record(s string) {
	p.mu.Lock()
	p.Calls = append(p.Calls, s)
	p.mu.Unlock()
}

// Health implements api.Provider.
func (p *Provider) Health() api.Health { p.record("health"); return p.HealthV }

// Operations implements api.Provider.
func (p *Provider) Operations() []api.OperationRow {
	p.record("operations")
	return p.Rows
}

// Operation implements api.Provider. It trims the parts not asked for, as a
// real provider would.
func (p *Provider) Operation(id int, opts stats.SnapshotOptions) (*api.OperationDetail, bool) {
	p.record(fmt.Sprintf("operation %d %v", id, opts))
	d, ok := p.Details[id]
	if !ok {
		return nil, false
	}
	c := *d
	if !opts.Hours {
		c.Hours = nil
	}
	if !opts.History {
		c.History = nil
	}
	if !opts.Enhanced {
		c.Enhanced = nil
	}
	return &c, true
}

// Reload implements api.Provider.
func (p *Provider) Reload(context.Context) (*api.ReloadResult, error) {
	p.record("reload")
	if p.ReloadErr != nil {
		return nil, p.ReloadErr
	}
	if p.ReloadRes == nil {
		return nil, api.ErrNotImplemented
	}
	return p.ReloadRes, nil
}

// Restart implements api.Provider.
func (p *Provider) Restart(_ context.Context, id int) error {
	p.record(fmt.Sprintf("restart %d", id))
	if _, ok := p.Details[id]; !ok && p.RestartErr == nil {
		return fmt.Errorf("operation %d %w", id, api.ErrNotFound) // "operation 999 not found", as goipslad says
	}
	return p.RestartErr
}

// Reset implements api.Provider.
func (p *Provider) Reset(context.Context) error { p.record("reset"); return p.ResetErr }

// LatestJitter is the latest burst of operation 1: 10 packets, packet 4
// lost, SD jitter samples +0 ×5, +1 ×1 and −1 ×1, DS samples all 0.
var LatestJitter = op.JitterResult{
	NumPackets: 10, Sent: 10,
	NumRTT: 9, RTTSumMs: 4, RTTSum2Ms: 4, RTTMinMs: 0, RTTMaxMs: 1,
	PosSD:   op.JitterSide{Num: 6, SumMs: 1, Sum2Ms: 1, MinMs: 0, MaxMs: 1},
	NegSD:   op.JitterSide{Num: 1, SumMs: 1, Sum2Ms: 1, MinMs: 1, MaxMs: 1},
	PosDS:   op.JitterSide{Num: 7},
	PktLoss: 1, MinSucPktLoss: 1, MaxSucPktLoss: 1,
}

// TotalsJitter accumulates the 40 bursts of operation 1's life.
var TotalsJitter = stats.JitterCounters{
	NumRTT: 396, RTTSumMs: 160, RTTSum2Ms: 170, RTTMinMs: 0, RTTMaxMs: 3,
	NumOverThreshold: 0,
	PosSD:            op.JitterSide{Num: 300, SumMs: 40, Sum2Ms: 44, MinMs: 0, MaxMs: 2},
	NegSD:            op.JitterSide{Num: 50, SumMs: 60, Sum2Ms: 80, MinMs: 1, MaxMs: 3},
	PosDS:            op.JitterSide{Num: 340, SumMs: 10, Sum2Ms: 10, MinMs: 0, MaxMs: 1},
	NegDS:            op.JitterSide{Num: 10, SumMs: 10, Sum2Ms: 10, MinMs: 1, MaxMs: 1},
	PktLoss:          4, PktLateArrival: 1, PktOutSeqSD: 0, PktOutSeqDS: 2, PktOutSeqBoth: 0,
	MinSucPktLoss: 1, MaxSucPktLoss: 2,
	Skipped: 0,
}

func ms(v float64) *float64 { return &v }
func at(d time.Duration) *time.Time {
	t := Base.Add(d)
	return &t
}

// NewProvider returns a fake with four operations:
//
//	1   icmp-jitter 10.100.2.11     active, ok, one packet of ten lost, with hours
//	101 icmp-echo   10.100.1.11 wan active, ok, with hours / history / enhanced
//	102 icmp-echo   fd00:100:2::11 wan active, timeout (unreachable)
//	103 icmp-echo   10.100.1.13 lan pending, never attempted
func NewProvider() *Provider {
	p := &Provider{Details: map[int]*api.OperationDetail{}}
	p.HealthV = api.Health{Version: "1.2.3", StartedAt: Base.Add(-90 * time.Minute), ConfigPath: "/etc/goipslad/goipslad.yaml"}
	p.HealthV.Operations.Total = 4
	p.HealthV.Operations.Active = 3
	p.HealthV.Operations.Pending = 1

	ok := stats.Counters{Initiations: 120, Completions: 118, OverThresholds: 3, Timeouts: 2, RTTSumMs: 236, RTTSum2Ms: 600, RTTMinMs: 1, RTTMaxMs: 9}
	p.Rows = []api.OperationRow{
		{
			ID: 1, Type: config.ICMPJitter, Target: "10.100.2.11", State: api.StateActive,
			Latest: api.LatestJSON{Valid: true, Seq: 40, Start: at(-3 * time.Second), End: at(-3*time.Second + 250*time.Millisecond), RTTMs: ms(0.412), Code: op.RCOK,
				Detail: "loss 1/10", Jitter: api.NewJitterResultJSON(&LatestJitter)},
			Totals: api.NewCountersJSON(stats.Counters{Initiations: 40, Completions: 40, RTTSumMs: 40, RTTSum2Ms: 40, RTTMinMs: 1, RTTMaxMs: 1}),
		},
		{
			ID: 101, Type: config.ICMPEcho, Target: "10.100.1.11", Tag: "wan", VRF: "blue", State: api.StateActive,
			Latest: api.LatestJSON{Valid: true, Seq: 120, Start: at(-130 * time.Second), End: at(-130*time.Second + 1234*time.Microsecond), RTTMs: ms(1.234), Code: op.RCOK},
			Totals: api.NewCountersJSON(ok),
		},
		{
			ID: 102, Type: config.ICMPEcho, Target: "fd00:100:2::11", Tag: "wan", State: api.StateActive,
			Latest: api.LatestJSON{Valid: true, Seq: 12, Start: at(-7 * time.Second), End: at(-7*time.Second + 2*time.Millisecond), Code: op.RCTimeout, Detail: "destination unreachable: host, from fd00:100:1::254"},
			Totals: api.NewCountersJSON(stats.Counters{Initiations: 12, Timeouts: 12}),
		},
		{
			ID: 103, Type: config.ICMPEcho, Target: "10.100.1.13", Tag: "lan", State: api.StatePending,
			Latest: api.LatestJSON{Code: op.RCOther},
		},
	}
	cfgs := map[int]string{
		1:   `{"id":1,"type":"icmp-jitter","target":"10.100.2.11","target-name":"10.100.2.11","tos":0,"frequency":"30s","timeout":"5000ms","threshold":"5000ms","interval":"20ms","num-packets":10,"tag":"","owner":"","history":{"lives-kept":0,"buckets-kept":15,"filter":"none","hours-of-statistics-kept":2,"distributions-of-statistics-kept":1,"statistics-distribution-interval":"20ms","enhanced":null},"schedule":null,"react":[]}`,
		101: `{"id":101,"type":"icmp-echo","target":"10.100.1.11","target-name":"10.100.1.11","template":"wan-echo","source-interface":"eth1","vrf":"blue","tos":184,"frequency":"10s","timeout":"2000ms","threshold":"300ms","request-data-size":28,"data-pattern":"0xABCDABCD","verify-data":false,"tag":"wan","owner":"","history":{"lives-kept":1,"buckets-kept":15,"filter":"all","hours-of-statistics-kept":2,"distributions-of-statistics-kept":2,"statistics-distribution-interval":"5ms","enhanced":{"interval":"60s","buckets":100}},"schedule":{"life":"forever","start-time":"now","ageout":"0s","recurring":false},"react":[{"element":"rtt","threshold-type":"consecutive","count":3,"x":5,"y":5,"upper":300,"lower":200,"action":"trap"}]}`,
		102: `{"id":102,"type":"icmp-echo","target":"fd00:100:2::11","target-name":"fd00:100:2::11","traffic-class":0,"flow-label":0,"frequency":"10s","timeout":"2000ms","threshold":"300ms","request-data-size":28,"data-pattern":"0xABCDABCD","verify-data":false,"tag":"wan","owner":"","history":{"lives-kept":0,"buckets-kept":15,"filter":"none","hours-of-statistics-kept":2,"distributions-of-statistics-kept":1,"statistics-distribution-interval":"20ms","enhanced":null},"schedule":null,"react":[]}`,
		103: `{"id":103,"type":"icmp-echo","target":"10.100.1.13","target-name":"10.100.1.13","tos":0,"frequency":"60s","timeout":"5000ms","threshold":"5000ms","request-data-size":28,"data-pattern":"0xABCDABCD","verify-data":false,"tag":"lan","owner":"","history":{"lives-kept":0,"buckets-kept":15,"filter":"none","hours-of-statistics-kept":2,"distributions-of-statistics-kept":1,"statistics-distribution-interval":"20ms","enhanced":null},"schedule":{"life":"3600s","start-time":"pending","ageout":"0s","recurring":false},"react":[]}`,
	}
	for _, r := range p.Rows {
		p.Details[r.ID] = &api.OperationDetail{
			OperationRow: r,
			Config:       json.RawMessage(cfgs[r.ID]),
			LifeStart:    Base.Add(-2 * time.Hour),
			LifeIndex:    1,
		}
	}
	j := p.Details[1]
	j.TotalsJitter = api.NewJitterCountersJSON(&TotalsJitter)
	hj := TotalsJitter
	j.Hours = []api.HourGroupJSON{
		api.NewHourGroupJSON(stats.HourGroup{
			Index: 2, Start: Base.Add(-time.Hour),
			Counters: stats.Counters{Initiations: 40, Completions: 40, RTTSumMs: 40, RTTSum2Ms: 40, RTTMinMs: 1, RTTMaxMs: 1},
			Jitter:   &hj,
		}),
	}
	d := p.Details[101]
	d.Hours = []api.HourGroupJSON{
		api.NewHourGroupJSON(stats.HourGroup{
			Index: 1, Start: Base.Add(-2 * time.Hour),
			Counters: stats.Counters{Initiations: 60, Completions: 59, OverThresholds: 1, Timeouts: 1, RTTSumMs: 118, RTTSum2Ms: 300, RTTMinMs: 1, RTTMaxMs: 9},
			Dist: []stats.DistBucket{
				{Index: 1, LowerMs: 0, UpperMs: 5, Completions: 58, RTTSumMs: 109, RTTSum2Ms: 219, RTTMinMs: 1, RTTMaxMs: 4},
				{Index: 2, LowerMs: 5, Completions: 1, RTTSumMs: 9, RTTSum2Ms: 81, RTTMinMs: 9, RTTMaxMs: 9},
			},
		}),
		api.NewHourGroupJSON(stats.HourGroup{
			Index: 2, Start: Base.Add(-time.Hour),
			Counters: stats.Counters{Initiations: 60, Completions: 59, OverThresholds: 2, Timeouts: 1, Busies: 1, RTTSumMs: 118, RTTSum2Ms: 300, RTTMinMs: 1, RTTMaxMs: 4},
			Dist: []stats.DistBucket{
				{Index: 1, LowerMs: 0, UpperMs: 5, Completions: 59, RTTSumMs: 118, RTTSum2Ms: 300, RTTMinMs: 1, RTTMaxMs: 4},
				{Index: 2, LowerMs: 5},
			},
		}),
	}
	d.History = []api.HistoryBucketJSON{
		{Life: 1, Bucket: 14, Sample: 1, Start: Base.Add(-150 * time.Second), RTTMs: 2, Code: op.RCOK, Target: "10.100.1.11"},
		{Life: 1, Bucket: 15, Sample: 1, Start: Base.Add(-140 * time.Second), RTTMs: 0, Code: op.RCTimeout, Target: "10.100.1.11"},
		{Life: 1, Bucket: 16, Sample: 1, Start: Base.Add(-130 * time.Second), RTTMs: 1, Code: op.RCOK, Target: "10.100.1.11"},
	}
	d.Enhanced = []api.EnhancedBucketJSON{
		{Index: 1, Start: Base.Add(-2 * time.Minute), Counters: api.NewCountersJSON(stats.Counters{Initiations: 6, Completions: 6, OverThresholds: 1, RTTSumMs: 12, RTTSum2Ms: 30, RTTMinMs: 1, RTTMaxMs: 4})},
		{Index: 2, Start: Base.Add(-time.Minute), Counters: api.NewCountersJSON(stats.Counters{Initiations: 6, Completions: 5, Timeouts: 1, RTTSumMs: 10, RTTSum2Ms: 20, RTTMinMs: 2, RTTMaxMs: 2})},
	}
	return p
}

// Serve runs an api.Server for p on a socket in a new temporary directory
// and returns the socket path. The server stops when the test ends.
func Serve(t testing.TB, p api.Provider) string {
	t.Helper()
	// Unix socket paths are limited to ~108 bytes; t.TempDir can be longer.
	dir, err := os.MkdirTemp("", "goipsla")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	path := filepath.Join(dir, "s.sock")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv, err := api.NewServer(path, 0o600, "", p, logger)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.Serve(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-done; err != nil {
			t.Errorf("Serve: %v", err)
		}
		srv.Close()
	})
	return path
}
