// A test of the store; it shares the store's namespace.
//
//declscope:namespace store

package stats

import (
	"sync"
	"testing"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/op"
)

// TestInFlightAttemptAcrossReset: an attempt started in life 1 completes
// after Reset started life 2. Its result belongs to life 1 and must not be
// accounted in life 2.
func TestInFlightAttemptAcrossReset(t *testing.T) {
	cfg := historyCfg(2, 15, "all")
	s := newStore(t, cfg)
	s.Record(res(1, 0, op.RCOK, time.Millisecond)) // life 1, completed

	// Attempt 2 starts at 10 s; Reset at 12 s; attempt 2 then completes.
	s.Reset(1, t0.Add(12*time.Second))
	s.Record(res(2, 10*time.Second, op.RCOK, 3*time.Second))
	// A late reply to attempt 1 of life 1 (sequenceError) is dropped too.
	s.Record(res(1, 0, op.RCSequenceError, 0))

	sn := snap(t, s, 1)
	if sn.Latest.Valid || sn.Totals != (Counters{}) || len(sn.Hours) != 0 {
		t.Fatalf("life 2 accounted a life 1 result: latest %+v totals %+v hours %+v", sn.Latest, sn.Totals, sn.Hours)
	}
	// Life 1's history is kept as it was; life 2 has no bucket.
	if len(sn.History) != 1 || sn.History[0].Life != 1 || sn.History[0].Bucket != 1 {
		t.Errorf("history = %+v", sn.History)
	}
	if got := s.Discarded(); got != 2 {
		t.Errorf("discarded = %d, want 2", got)
	}

	// The first attempt of life 2 starts exactly at the life start: kept.
	s.Record(res(1, 12*time.Second, op.RCOK, time.Millisecond))
	sn = snap(t, s, 1)
	if !sn.Latest.Valid || sn.Latest.Seq != 1 || sn.Totals.Initiations != 1 || sn.Hours[0].Index != 1 {
		t.Errorf("life 2 first attempt: latest %+v totals %+v hours %+v", sn.Latest, sn.Totals, sn.Hours)
	}
	if h := sn.History; len(h) != 2 || h[1].Life != 2 || h[1].Bucket != 1 {
		t.Errorf("history = %+v", h)
	}
}

// TestInFlightAttemptAcrossAdd: the same for Add on an existing ID (reload).
func TestInFlightAttemptAcrossAdd(t *testing.T) {
	s := newStore(t, opCfg(1))
	s.Add(opCfg(1), t0.Add(time.Minute))
	s.Record(res(5, 59*time.Second, op.RCTimeout, 0))
	sn := snap(t, s, 1)
	if sn.LifeIndex != 2 || sn.Latest.Valid || sn.Totals.Initiations != 0 {
		t.Errorf("snapshot = %+v", sn)
	}
	if s.Discarded() != 1 {
		t.Errorf("discarded = %d", s.Discarded())
	}
}

// TestZeroStartNotDiscarded: a result without a start time is accounted at
// the current time even if that is before the life start.
func TestZeroStartNotDiscarded(t *testing.T) {
	clk := clock.NewFake(t0)
	s := NewStore(clk)
	s.Add(opCfg(1), t0.Add(time.Hour)) // life scheduled to start later
	s.Record(op.Result{OpID: 1, Code: op.RCSequenceError})
	sn := snap(t, s, 1)
	if sn.Totals.SequenceErrors != 1 || len(sn.Hours) != 1 || sn.Hours[0].Index != 1 || s.Discarded() != 0 {
		t.Errorf("snapshot = %+v discarded %d", sn, s.Discarded())
	}
}

// TestRecordRacesReset records results with increasing start times while
// the life is reset concurrently. Whatever the interleaving, nothing that
// started before the current life may be accounted in it. Run with -race.
func TestRecordRacesReset(t *testing.T) {
	for range 20 {
		s := newStore(t, historyCfg(2, 60, "all"))
		reset := t0.Add(500 * time.Second)
		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for i := range 1000 {
				s.Record(res(uint32(i+1), time.Duration(i)*time.Second, op.RCOK, time.Millisecond))
			}
		}()
		go func() {
			defer wg.Done()
			s.Reset(1, reset)
		}()
		wg.Wait()

		sn := snap(t, s, 1)
		if sn.LifeIndex != 2 {
			t.Fatalf("life index = %d", sn.LifeIndex)
		}
		if sn.Totals.Initiations > 500 {
			t.Fatalf("initiations = %d, more than the 500 attempts started in life 2", sn.Totals.Initiations)
		}
		if sn.Latest.Valid && sn.Latest.Start.Before(reset) {
			t.Fatalf("latest from life 1: %+v", sn.Latest)
		}
		for _, h := range sn.History {
			if h.Life == 2 && h.Start.Before(reset) {
				t.Fatalf("life 2 bucket from life 1: %+v", h)
			}
		}
		for _, g := range sn.Hours {
			if g.Start.Before(reset) {
				t.Fatalf("hour group before the life start: %+v", g)
			}
		}
	}
}
