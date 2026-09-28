// A test of the store; it shares the store's namespace.
//
//declscope:namespace store

package stats

import (
	"math/rand/v2"
	"sync/atomic"
	"testing"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/op"
)

// benchStore returns a store with n operations using the Cisco defaults plus
// history and enhanced history, so Record touches every structure.
func benchStore(n int) *Store {
	s := NewStore(clock.NewFake(t0))
	for id := 1; id <= n; id++ {
		cfg := opCfg(id)
		cfg.Stats.DistBuckets = 5
		cfg.Stats.DistInterval = 10 * time.Millisecond
		cfg.History = config.HistoryConfig{Lives: 2, Buckets: 15, Filter: "all"}
		cfg.Enhanced = &config.EnhancedHistory{Interval: 15 * time.Minute, Buckets: 100}
		s.Add(cfg, t0)
	}
	return s
}

func benchResults(n int) []op.Result {
	rng := rand.New(rand.NewPCG(1, 2))
	rs := make([]op.Result, 4096)
	for i := range rs {
		rs[i] = op.Result{
			OpID:  1 + rng.IntN(n),
			Type:  config.ICMPEcho,
			Seq:   uint32(i),
			Start: t0.Add(time.Duration(i) * time.Second),
			RTT:   time.Duration(rng.IntN(50_000)) * time.Microsecond,
			Code:  op.RCOK,
		}
		if rng.IntN(10) == 0 {
			rs[i].Code = op.RCTimeout
		}
	}
	return rs
}

// BenchmarkRecord records into 1,000 operations with random IDs.
func BenchmarkRecord(b *testing.B) {
	s := benchStore(1000)
	rs := benchResults(1000)
	b.ReportAllocs()
	i := 0
	for b.Loop() {
		s.Record(rs[i%len(rs)])
		i++
	}
}

// BenchmarkRecordParallel records from all Ps at once.
func BenchmarkRecordParallel(b *testing.B) {
	s := benchStore(1000)
	rs := benchResults(1000)
	var next atomic.Uint64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Record(rs[next.Add(1)%uint64(len(rs))])
		}
	})
}

// BenchmarkRecordWithSummary records from all Ps while one goroutine keeps
// calling Summary, the API's list view.
func BenchmarkRecordWithSummary(b *testing.B) {
	s := benchStore(1000)
	rs := benchResults(1000)
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
				s.Summary()
			}
		}
	}()
	var next atomic.Uint64
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			s.Record(rs[next.Add(1)%uint64(len(rs))])
		}
	})
	b.StopTimer()
	close(stop)
	<-done
}

// BenchmarkSummary1000 lists 1,000 operations.
func BenchmarkSummary1000(b *testing.B) {
	s := benchStore(1000)
	for _, r := range benchResults(1000) {
		s.Record(r)
	}
	b.ReportAllocs()
	for b.Loop() {
		s.Summary()
	}
}

// BenchmarkSnapshotFull reads one operation with every optional part.
func BenchmarkSnapshotFull(b *testing.B) {
	s := benchStore(1)
	for i := range 3 * 3600 {
		s.Record(op.Result{OpID: 1, Start: t0.Add(time.Duration(i) * time.Second), RTT: 12 * time.Millisecond, Code: op.RCOK})
	}
	b.ReportAllocs()
	for b.Loop() {
		s.Snapshot(1, SnapshotOptions{Hours: true, History: true, Enhanced: true})
	}
}
