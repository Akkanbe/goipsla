package react

import (
	"io"
	"log/slog"
	"net/netip"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/event"
	"goipsla/internal/op"
)

// base is the start of the fake clock.
//
//declscope:package // shared test fixture: tracker_test.go uses it too
var base = time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

// quiet is a logger that drops everything.
//
//declscope:package // shared test fixture: tracker_test.go uses it too
func quiet() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// result builders
func ok(ms int) op.Result {
	return op.Result{OpID: 5, Code: op.RCOK, RTT: time.Duration(ms)*time.Millisecond + 700*time.Microsecond}
}
func over(ms int) op.Result {
	r := ok(ms)
	r.Code = op.RCOverThreshold
	return r
}
func code(c op.ReturnCode) op.Result { return op.Result{OpID: 5, Code: c} }

// jit is a jitter result; a JitterResult without Sent is a burst of 10.
func jit(j *op.JitterResult, c op.ReturnCode) op.Result {
	if j != nil && j.Sent == 0 {
		j.Sent = 10
	}
	return op.Result{OpID: 5, Type: config.ICMPJitter, Code: c, RTT: 3 * time.Millisecond, Jitter: j}
}

func opWith(typ config.OpType, rs ...config.Reaction) *config.Operation {
	return &config.Operation{ID: 5, Type: typ, Target: netip.MustParseAddr("192.0.2.5"), Tag: "wan", VRF: "blue", React: rs}
}

// ev is the part of an event the table checks.
type ev struct {
	Kind  event.Kind
	Value int64
}

func events(b *event.Bus) []ev {
	var out []ev
	for _, e := range b.Recent(0) {
		out = append(out, ev{e.Kind, e.Value})
	}
	return out
}

const (
	up = event.ThresholdExceeded
	dn = event.ThresholdCleared
)

func rx(el, tt string, upper, lower int) config.Reaction {
	return config.Reaction{Element: el, ThresholdType: tt, Upper: upper, Lower: lower, Count: 5, X: 5, Y: 5, Action: "trap"}
}

func TestEvaluate(t *testing.T) {
	avg3 := rx("rtt", "average", 5000, 3000)
	avg3.Count = 3
	cons3 := rx("rtt", "consecutive", 300, 200)
	cons3.Count = 3
	xy := rx("rtt", "xofy", 300, 200)
	xy.X, xy.Y = 2, 3
	tests := []struct {
		name    string
		typ     config.OpType
		r       config.Reaction
		results []op.Result
		want    []ev
	}{
		{
			name:    "average: Cisco example 6000, 6000, 5000 averages 5667 above 5000",
			r:       avg3,
			results: []op.Result{ok(6000), ok(6000), ok(5000)},
			want:    []ev{{up, 5667}},
		},
		{
			name:    "average: not evaluated before N values",
			r:       avg3,
			results: []op.Result{ok(9000), ok(9000)},
		},
		{
			name: "average: falls when the average drops below lower",
			r:    avg3,
			// (6000+1000+1000)/3 = 2667 < 3000 on the 5th value
			results: []op.Result{ok(6000), ok(6000), ok(6000), ok(1000), ok(1000), ok(1000)},
			want:    []ev{{up, 6000}, {dn, 2667}},
		},
		{
			name: "immediate: steps 1-4 of the configuration guide",
			r:    rx("rtt", "immediate", 300, 200),
			// 1 rising; 2 further violations silent; 3 falls below lower; 4 rises again
			results: []op.Result{ok(350), ok(400), ok(500), ok(250), ok(150), ok(120), ok(350)},
			want:    []ev{{up, 350}, {dn, 150}, {up, 350}},
		},
		{
			name:    "immediate: the thresholds themselves are neither violation nor recovery",
			r:       rx("rtt", "immediate", 300, 200),
			results: []op.Result{ok(300), ok(301), ok(200), ok(199)},
			want:    []ev{{up, 301}, {dn, 199}},
		},
		{
			name:    "overThreshold is a completion with an RTT",
			r:       rx("rtt", "immediate", 300, 200),
			results: []op.Result{over(900)},
			want:    []ev{{up, 900}},
		},
		{
			name:    "consecutive: N-1 violations do nothing, a neutral value restarts the count",
			r:       cons3,
			results: []op.Result{ok(350), ok(350), ok(250), ok(350), ok(350), ok(350), ok(150), ok(150), ok(150)},
			want:    []ev{{up, 350}, {dn, 150}},
		},
		{
			name:    "consecutive: a violation breaks the recovery run",
			r:       cons3,
			results: []op.Result{ok(400), ok(400), ok(400), ok(100), ok(100), ok(400), ok(100), ok(100)},
			want:    []ev{{up, 400}},
		},
		{
			name: "xofy: the window survives transitions",
			r:    xy,
			// [v r v] rises; [r v r] falls; [v r v] rises again at once
			results: []op.Result{ok(350), ok(100), ok(350), ok(100), ok(350)},
			want:    []ev{{up, 350}, {dn, 100}, {up, 350}},
		},
		{
			name:    "xofy: only the last y count",
			r:       xy,
			results: []op.Result{ok(350), ok(250), ok(250), ok(350)},
		},
		{
			name:    "never publishes nothing",
			r:       rx("rtt", "never", 300, 200),
			results: []op.Result{ok(900), ok(1)},
		},
		{
			name:    "rtt ignores failed attempts",
			r:       rx("rtt", "immediate", 300, 200),
			results: []op.Result{ok(350), code(op.RCTimeout), code(op.RCDropped), ok(100)},
			want:    []ev{{up, 350}, {dn, 100}},
		},
		{
			name:    "timeout is boolean and evaluated on every attempt",
			r:       config.Reaction{Element: "timeout", ThresholdType: "immediate", Count: 5, X: 5, Y: 5, Action: "syslog"},
			results: []op.Result{ok(1), code(op.RCTimeout), code(op.RCTimeout), code(op.RCVerifyError), ok(1)},
			want:    []ev{{up, 1}, {dn, 0}},
		},
		{
			name:    "verifyError consecutive",
			r:       config.Reaction{Element: "verifyError", ThresholdType: "consecutive", Count: 2, X: 5, Y: 5},
			results: []op.Result{code(op.RCVerifyError), ok(1), code(op.RCVerifyError), code(op.RCVerifyError), code(op.RCTimeout), ok(1)},
			want:    []ev{{up, 1}, {dn, 0}},
		},
		{
			name:    "busy and sequenceError are not attempts",
			r:       config.Reaction{Element: "timeout", ThresholdType: "consecutive", Count: 2, X: 5, Y: 5},
			results: []op.Result{code(op.RCTimeout), code(op.RCBusy), code(op.RCSequenceError), code(op.RCTimeout)},
			want:    []ev{{up, 1}},
		},
		{
			name: "jitter: packetLoss and successive loss are counts",
			typ:  config.ICMPJitter,
			r:    rx("packetLoss", "immediate", 3, 1),
			results: []op.Result{
				jit(&op.JitterResult{PktLoss: 2}, op.RCOK),
				jit(&op.JitterResult{PktLoss: 10}, op.RCTimeout),
				jit(&op.JitterResult{PktLoss: 0}, op.RCOK),
				ok(1), // no jitter data: not evaluated
			},
			want: []ev{{up, 10}, {dn, 0}},
		},
		{
			// An error attempt sent nothing: its zero loss is no recovery
			// (audit B15). The next real burst clears.
			name: "jitter: an error attempt (Sent 0) does not clear a loss reaction",
			typ:  config.ICMPJitter,
			r:    rx("packetLoss", "immediate", 5, 3),
			results: []op.Result{
				jit(&op.JitterResult{PktLoss: 10}, op.RCTimeout),
				{OpID: 5, Type: config.ICMPJitter, Code: op.RCError, Jitter: &op.JitterResult{Sent: 0}},
				jit(&op.JitterResult{PktLoss: 1}, op.RCOK),
			},
			want: []ev{{up, 10}, {dn, 1}},
		},
		{
			name: "jitter: jitterAvg needs samples",
			typ:  config.ICMPJitter,
			r:    rx("jitterAvg", "immediate", 5, 2),
			results: []op.Result{
				jit(&op.JitterResult{}, op.RCTimeout), // no samples: not a recovery nor anything
				jit(&op.JitterResult{PosSD: op.JitterSide{Num: 2, SumMs: 14, MaxMs: 9}, NegDS: op.JitterSide{Num: 1, SumMs: 4, MaxMs: 4}}, op.RCOK), // 18/3 = 6
				jit(&op.JitterResult{PosSD: op.JitterSide{Num: 3, SumMs: 5, MaxMs: 3}}, op.RCOK),                                                    // 1.67 -> 1
			},
			want: []ev{{up, 6}, {dn, 1}},
		},
		{
			name: "jitter: packetOutOfSequence sums the three directions; maxOfPositiveSD",
			typ:  config.ICMPJitter,
			r:    rx("packetOutOfSequence", "immediate", 2, 1),
			results: []op.Result{
				jit(&op.JitterResult{PktOutSeqBoth: 1, PktOutSeqSD: 1, PktOutSeqDS: 1}, op.RCOK),
			},
			want: []ev{{up, 3}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bus := event.NewBus(quiet())
			e := NewEngine(clock.NewFake(base), bus, quiet())
			typ := tt.typ
			if typ == "" {
				typ = config.ICMPEcho
			}
			e.Configure(opWith(typ, tt.r))
			for _, r := range tt.results {
				e.Observe(r)
			}
			if diff := cmp.Diff(tt.want, events(bus)); diff != "" {
				t.Errorf("events (-want +got):\n%s", diff)
			}
		})
	}
}

func TestElementValues(t *testing.T) {
	j := &op.JitterResult{
		PosSD: op.JitterSide{Num: 1, SumMs: 5, MaxMs: 5}, NegSD: op.JitterSide{Num: 1, SumMs: 3, MaxMs: 3},
		PosDS: op.JitterSide{Num: 2, SumMs: 8, MaxMs: 6}, NegDS: op.JitterSide{Num: 1, SumMs: 1, MaxMs: 1},
		PktLateArrival: 4, PktOutSeqSD: 1, MaxSucPktLoss: 3, PktLoss: 5,
		OneWay: true, OWSD: op.JitterSide{Num: 2, SumMs: 21, MaxMs: 12}, OWDS: op.JitterSide{Num: 2, SumMs: 9, MaxMs: 5},
	}
	r := jit(j, op.RCOK)
	want := map[string]int64{
		"rtt": 3, "timeout": 0, "verifyError": 0,
		"jitterAvg": 3, "jitterSDAvg": 4, "jitterDSAvg": 3,
		"maxOfPositiveSD": 5, "maxOfNegativeSD": 3, "maxOfPositiveDS": 6, "maxOfNegativeDS": 1,
		"packetLateArrival": 4, "packetOutOfSequence": 1, "successivePacketLoss": 3, "packetLoss": 5,
		"maxOfLatencySD": 12, "maxOfLatencyDS": 5, "latencySDAvg": 10, "latencyDSAvg": 4,
	}
	for el, w := range want {
		got, ok := elementValue(el, &r)
		if !ok || got != w {
			t.Errorf("%s = %d, %v; want %d", el, got, ok, w)
		}
	}
	j.OneWay = false
	for _, el := range []string{"maxOfLatencySD", "maxOfLatencyDS", "latencySDAvg", "latencyDSAvg"} {
		if _, ok := elementValue(el, &r); ok {
			t.Errorf("%s evaluated without one-way delays", el)
		}
	}
	if _, ok := elementValue("unknownElement", &r); ok {
		t.Error("unknown element evaluated")
	}
}

func TestEventFields(t *testing.T) {
	bus := event.NewBus(quiet())
	fake := clock.NewFake(base)
	e := NewEngine(fake, bus, quiet())
	e.Configure(opWith(config.ICMPEcho,
		config.Reaction{Element: "rtt", ThresholdType: "immediate", Upper: 300, Lower: 200, Count: 5, X: 5, Y: 5, Action: "trap-and-syslog"},
		config.Reaction{Element: "timeout", ThresholdType: "immediate", Count: 5, X: 5, Y: 5, Action: "syslog"},
	))
	e.Observe(ok(350))
	fake.Advance(time.Second)
	e.Observe(code(op.RCTimeout))
	e.Observe(ok(100))
	got := bus.Recent(0)
	want := []event.Event{
		{Time: base, Kind: up, OpID: 5, Type: config.ICMPEcho, Target: "192.0.2.5", Tag: "wan", VRF: "blue",
			Element: "rtt", ThresholdType: "immediate", Value: 350, Upper: 300, Lower: 200, Action: "trap-and-syslog", ReactionIndex: 1,
			Message: "%RTT-3-IPSLATHRESHOLD: IP SLAs(5): Threshold exceeded for rtt (value 350, rising 300, falling 200)"},
		{Time: base.Add(time.Second), Kind: up, OpID: 5, Type: config.ICMPEcho, Target: "192.0.2.5", Tag: "wan", VRF: "blue",
			Element: "timeout", ThresholdType: "immediate", Value: 1, Action: "syslog", ReactionIndex: 2,
			Message: "%RTT-4-OPER_TIMEOUT: IP SLAs(5): Threshold exceeded for timeout"},
		{Time: base.Add(time.Second), Kind: dn, OpID: 5, Type: config.ICMPEcho, Target: "192.0.2.5", Tag: "wan", VRF: "blue",
			Element: "rtt", ThresholdType: "immediate", Value: 100, Upper: 300, Lower: 200, Action: "trap-and-syslog", ReactionIndex: 1,
			Message: "%RTT-3-IPSLATHRESHOLD: IP SLAs(5): Threshold below for rtt (value 100, rising 300, falling 200)"},
		{Time: base.Add(time.Second), Kind: dn, OpID: 5, Type: config.ICMPEcho, Target: "192.0.2.5", Tag: "wan", VRF: "blue",
			Element: "timeout", ThresholdType: "immediate", Action: "syslog", ReactionIndex: 2,
			Message: "%RTT-4-OPER_TIMEOUT: IP SLAs(5): Threshold below for timeout"},
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	st := e.Snapshot(5)
	if len(st) != 2 || st[0].Occurred || st[0].Changes != 2 || st[0].Value != 100 || !st[0].LastChange.Equal(base.Add(time.Second)) {
		t.Errorf("snapshot %+v", st)
	}
	if all := e.All(); len(all) != 1 || len(all[5]) != 2 {
		t.Errorf("all %+v", all)
	}
	if ids := e.IDs(); len(ids) != 1 || ids[0] != 5 {
		t.Errorf("ids %v", ids)
	}
}

func TestResetAndConfigureAreSilent(t *testing.T) {
	bus := event.NewBus(quiet())
	e := NewEngine(clock.NewFake(base), bus, quiet())
	r := rx("rtt", "immediate", 300, 200)
	e.Configure(opWith(config.ICMPEcho, r))
	e.Observe(ok(400)) // exceeded
	e.Reset(5)
	if st := e.Snapshot(5); st[0].Occurred || st[0].Value != 0 {
		t.Fatalf("Reset left state: %+v", st[0]) // Value too (audit B16)
	}
	e.Observe(ok(100)) // occurred is false: a recovery says nothing
	e.Observe(ok(400)) // rises again
	e.Configure(opWith(config.ICMPEcho, r))
	e.Observe(ok(100)) // fresh state after Configure: silent
	want := []ev{{up, 400}, {up, 400}}
	if diff := cmp.Diff(want, events(bus)); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}

	// average window is cleared too
	avg := rx("rtt", "average", 5000, 3000)
	avg.Count = 2
	e.Configure(opWith(config.ICMPEcho, avg))
	e.Observe(ok(9000))
	e.Reset(5)
	e.Observe(ok(9000)) // only one value in the window again
	if n := len(bus.Recent(0)); n != 2 {
		t.Errorf("average fired across Reset: %d events", n)
	}

	e.Remove(5)
	e.Observe(ok(9000))
	e.Observe(ok(9000))
	if e.Snapshot(5) != nil || len(bus.Recent(0)) != 2 {
		t.Error("removed operation still evaluated")
	}
	e.Configure(opWith(config.ICMPEcho)) // no reactions: nothing registered
	if len(e.All()) != 0 {
		t.Error("operation without reactions registered")
	}
}

func TestSetTag(t *testing.T) {
	bus := event.NewBus(quiet())
	e := NewEngine(clock.NewFake(base), bus, quiet())
	e.Configure(opWith(config.ICMPEcho, rx("rtt", "immediate", 300, 200)))
	e.Observe(ok(400)) // exceeded, tag "wan"
	e.SetTag(5, "core")
	e.SetTag(99, "nothing") // unknown operation: ignored
	e.Observe(ok(100))      // cleared with the new tag; the state was kept
	evs := bus.Recent(0)
	if len(evs) != 2 || evs[0].Tag != "wan" || evs[1].Tag != "core" || evs[1].Kind != dn {
		t.Errorf("events %+v", evs)
	}
	if st := e.Snapshot(5); st[0].Changes != 2 {
		t.Errorf("state lost: %+v", st)
	}
}
