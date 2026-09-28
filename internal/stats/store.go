package stats

import (
	"net/netip"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/op"
)

const hour = time.Hour

// Latest is the most recent attempt (rttMonLatestRttOper*).
type Latest struct {
	Valid  bool // false until the first attempt
	Seq    uint32
	Start  time.Time
	End    time.Time
	RTT    time.Duration // valid only when Code is ok / overThreshold
	Code   op.ReturnCode // RCOther before the first attempt
	Detail string
	Jitter *op.JitterResult // icmp-jitter: a copy of the latest burst; nil otherwise
}

// HourGroup is one hourly group of the aggregated statistics.
type HourGroup struct {
	Index    int // Start Time Index: 1-based, increasing
	Start    time.Time
	Counters Counters
	Dist     []DistBucket    // nil for icmp-jitter
	Jitter   *JitterCounters // icmp-jitter only
}

// HistoryBucket is one sample of the snapshot history.
type HistoryBucket struct {
	Life   int       // life index: 1-based, increasing
	Bucket int       // bucket index within the life: 1-based, increasing
	Sample int       // always 1 for echo
	Start  time.Time // attempt start time
	RTTMs  uint64    // 0 unless Code is ok (MIB definition)
	Code   op.ReturnCode
	Target netip.Addr
}

// EnhancedBucket is one interval of the enhanced history.
type EnhancedBucket struct {
	Index    int // 1-based, increasing
	Start    time.Time
	Counters Counters
}

// Snapshot is a copy of one operation's statistics.
type Snapshot struct {
	ID        int
	Type      config.OpType
	Target    netip.Addr
	Tag       string
	LifeStart time.Time // start of the current life
	LifeIndex int       // index of the current life
	Latest    Latest
	Totals    Counters // the whole current life
	// TotalsJitter accumulates the bursts of the current life (icmp-jitter
	// only; nil otherwise).
	TotalsJitter *JitterCounters
	Hours        []HourGroup      // oldest first; nil unless SnapshotOptions.Hours
	History      []HistoryBucket  // oldest first (life, then bucket); nil unless SnapshotOptions.History
	Enhanced     []EnhancedBucket // oldest first; nil unless SnapshotOptions.Enhanced
}

// SnapshotOptions selects the optional parts of a Snapshot.
type SnapshotOptions struct {
	Hours    bool // hour groups with their distribution buckets
	History  bool // snapshot history
	Enhanced bool // enhanced history
}

// SummaryRow is one line of the operation list.
type SummaryRow struct {
	ID     int
	Type   config.OpType
	Target netip.Addr
	Tag    string
	Latest Latest
	Totals Counters
}

// Store holds the statistics of all operations. It is safe for concurrent
// use.
//
// Locking: the map and the sorted ID list are guarded by an RWMutex that
// Record, Snapshot and Summary only read-lock; each row has its own mutex.
// Concurrent Records for different operations therefore do not contend, and
// Summary holds each row lock only while copying its latest result and
// totals: measured at about 240 ns per Record while Summary of 1,000 rows
// runs, where one Store mutex would stall every Record for about 150 µs
// (docs/statistics.md, "Concurrency").
type Store struct {
	clk clock.Clock

	discarded atomic.Uint64 // results of earlier lives dropped by Record

	mu   sync.RWMutex
	rows map[int]*row
	ids  []int // sorted
}

// NewStore returns an empty Store. clk supplies the time for results
// without a start time.
func NewStore(clk clock.Clock) *Store {
	if clk == nil {
		clk = clock.Real()
	}
	return &Store{clk: clk, rows: make(map[int]*row)}
}

// Add creates the statistics row of an operation and starts its life at
// start. If the row exists, Add behaves like Reset but also adopts cfg.
func (s *Store) Add(cfg *config.Operation, start time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.rows[cfg.ID]; ok {
		r.mu.Lock()
		r.setConfig(cfg)
		r.newLife(start, r.lifeIndex+1)
		r.mu.Unlock()
		return
	}
	r := &row{}
	r.setConfig(cfg)
	r.newLife(start, 1)
	s.rows[cfg.ID] = r
	i, _ := slices.BinarySearch(s.ids, cfg.ID)
	s.ids = slices.Insert(s.ids, i, cfg.ID)
}

// SetConfig applies settings that do not affect measurement (tag, owner,
// react, ...) to an operation's row while keeping its statistics, for a
// reload that changed only those. Of them the row keeps only the tag, which
// Summary and Snapshot report. Measurement settings in cfg (type, target,
// statistics and history settings) are ignored: changing them requires Add,
// which starts a new life. Unknown IDs are ignored.
func (s *Store) SetConfig(cfg *config.Operation) {
	r := s.row(cfg.ID)
	if r == nil {
		return
	}
	r.mu.Lock()
	r.tag = cfg.Tag
	r.mu.Unlock()
}

// Remove deletes the statistics row of an operation.
func (s *Store) Remove(id int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rows[id]; !ok {
		return
	}
	delete(s.rows, id)
	if i, ok := slices.BinarySearch(s.ids, id); ok {
		s.ids = slices.Delete(s.ids, i, i+1)
	}
}

// Reset discards the statistics and starts a new life at start, keeping the
// configuration. Hour group and enhanced history indexes restart at 1; the
// history life index advances by one.
func (s *Store) Reset(id int, start time.Time) {
	r := s.row(id)
	if r == nil {
		return
	}
	r.mu.Lock()
	r.newLife(start, r.lifeIndex+1)
	r.mu.Unlock()
}

// Record accounts one result. Results for unknown operations are dropped.
//
// A result whose attempt started before the current life began belongs to
// an earlier life (an attempt still in flight across Reset or Add, or a late
// reply to one) and is dropped too, so that a new life starts from nothing.
// A result without a start time is accounted at the current time and never
// dropped.
func (s *Store) Record(res op.Result) {
	r := s.row(res.OpID)
	if r == nil {
		return
	}
	stamped := !res.Start.IsZero()
	if !stamped {
		res.Start = s.clk.Now()
	}
	r.mu.Lock()
	if stamped && res.Start.Before(r.lifeStart) {
		r.mu.Unlock()
		s.discarded.Add(1)
		return
	}
	r.record(&res)
	r.mu.Unlock()
}

// Discarded returns how many results Record dropped because they belonged
// to an earlier life. It is a diagnostic counter for all operations.
func (s *Store) Discarded() uint64 { return s.discarded.Load() }

// Snapshot returns a copy of one operation's statistics. The caller may
// modify it freely.
func (s *Store) Snapshot(id int, opts SnapshotOptions) (*Snapshot, bool) {
	r := s.row(id)
	if r == nil {
		return nil, false
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.snapshot(opts), true
}

// Summary returns one row per operation in ID order.
func (s *Store) Summary() []SummaryRow {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]SummaryRow, len(s.ids))
	for i, id := range s.ids {
		r := s.rows[id]
		r.mu.Lock()
		out[i] = SummaryRow{
			ID:     r.id,
			Type:   r.typ,
			Target: r.target,
			Tag:    r.tag,
			Latest: r.latestCopy(),
			Totals: r.totals,
		}
		r.mu.Unlock()
	}
	return out
}

func (s *Store) row(id int) *row {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.rows[id]
}

// life is one life of the snapshot history.
type life struct {
	index    int
	attempts int             // attempts so far; the next bucket index is attempts+1
	buckets  []HistoryBucket // the most recent historyBuckets
}

// row is the statistics of one operation. All fields are guarded by mu.
type row struct {
	mu sync.Mutex

	// configuration
	id          int
	typ         config.OpType
	target      netip.Addr
	tag         string
	hoursKept   int
	distBuckets int
	distWidthMs uint64
	lives       int
	histBuckets int
	filter      config.HistoryFilter
	enhInterval time.Duration // 0 disables the enhanced history
	enhBuckets  int

	jitter bool // icmp-jitter: jitter counters, no distribution, no history

	// current life
	lifeStart    time.Time
	lifeIndex    int
	latest       Latest
	totals       Counters
	totalsJitter *JitterCounters  // icmp-jitter only
	hours        []HourGroup      // oldest first, at most hoursKept
	history      []life           // oldest first, at most lives (current last)
	enhanced     []EnhancedBucket // oldest first, at most enhBuckets
}

func (r *row) setConfig(cfg *config.Operation) {
	r.id = cfg.ID
	r.typ = cfg.Type
	r.target = cfg.Target
	r.tag = cfg.Tag
	r.jitter = cfg.Type == config.ICMPJitter
	r.hoursKept = max(cfg.Stats.HoursKept, 0)
	r.distBuckets = max(cfg.Stats.DistBuckets, 1)
	r.distWidthMs = max(rttMs(cfg.Stats.DistInterval), 1)
	r.lives = max(cfg.History.Lives, 0)
	r.histBuckets = max(cfg.History.Buckets, 1)
	r.filter = cfg.History.Filter
	r.enhInterval, r.enhBuckets = 0, 0
	if e := cfg.Enhanced; e != nil && e.Interval > 0 && e.Buckets > 0 {
		r.enhInterval, r.enhBuckets = e.Interval, e.Buckets
	}
	// A shrunk history setting takes effect at once.
	if len(r.history) > r.lives {
		r.history = slices.Delete(r.history, 0, len(r.history)-r.lives)
	}
}

// newLife discards the statistics and starts life index at start. Previous
// lives stay in the history up to the lives setting.
func (r *row) newLife(start time.Time, index int) {
	r.lifeStart = start
	r.lifeIndex = index
	r.latest = Latest{}
	r.totals = Counters{}
	r.totalsJitter = nil
	if r.jitter {
		r.totalsJitter = &JitterCounters{}
	}
	r.hours = nil
	r.enhanced = nil
	if r.lives == 0 || r.jitter {
		r.history = nil
		return
	}
	r.history = append(r.history, life{index: index})
	if len(r.history) > r.lives {
		r.history = slices.Delete(r.history, 0, len(r.history)-r.lives)
	}
}

// periodIndex returns the 1-based index of the period of length p that
// contains t, counting from the life start. Times before the start (only
// possible for results without a start time, stamped with the clock) fall in
// period 1.
func (r *row) periodIndex(t time.Time, p time.Duration) int {
	d := t.Sub(r.lifeStart)
	if d < 0 {
		return 1
	}
	return int(d/p) + 1
}

func (r *row) record(res *op.Result) {
	code := res.Code
	ms := uint64(0)
	if isCompletion(code) {
		ms = rttMs(res.RTT)
	}
	r.totals.add(code, ms)

	if isAttempt(code) {
		r.latest = Latest{
			Valid:  true,
			Seq:    res.Seq,
			Start:  res.Start,
			End:    res.End,
			Code:   code,
			Detail: res.Detail,
		}
		if isCompletion(code) {
			r.latest.RTT = res.RTT
		}
		if r.jitter {
			r.latest.Jitter = cloneJitterResult(res.Jitter)
		} else {
			r.recordHistory(res, ms)
		}
	}
	jr := res.Jitter
	if !r.jitter {
		jr = nil
	}
	if jr != nil {
		r.totalsJitter.add(jr)
	}

	if r.hoursKept > 0 {
		if g := r.hourGroup(r.periodIndex(res.Start, hour)); g != nil {
			g.Counters.add(code, ms)
			if g.Dist != nil && isCompletion(code) {
				g.Dist[distIndex(ms, r.distBuckets, r.distWidthMs)].add(ms, code == op.RCOverThreshold)
			}
			if jr != nil {
				g.Jitter.add(jr)
			}
		}
	}
	if r.enhInterval > 0 {
		if b := r.enhancedBucket(r.periodIndex(res.Start, r.enhInterval)); b != nil {
			b.Counters.add(code, ms)
		}
	}
}

// hourGroup returns the group with index idx, creating it if idx is newer
// than every kept group (hours without results get no group). It returns nil
// for a group that has already been discarded.
func (r *row) hourGroup(idx int) *HourGroup {
	if n := len(r.hours); n == 0 || idx > r.hours[n-1].Index {
		g := HourGroup{Index: idx, Start: r.lifeStart.Add(time.Duration(idx-1) * hour)}
		if r.jitter {
			g.Jitter = &JitterCounters{}
		} else {
			g.Dist = newDist(r.distBuckets, r.distWidthMs)
		}
		r.hours = append(r.hours, g)
		if len(r.hours) > r.hoursKept {
			r.hours = slices.Delete(r.hours, 0, len(r.hours)-r.hoursKept)
		}
		return &r.hours[len(r.hours)-1]
	}
	// A result older than the current group (out of order).
	for i := len(r.hours) - 1; i >= 0; i-- {
		if r.hours[i].Index == idx {
			return &r.hours[i]
		}
	}
	return nil
}

// enhancedBucket is hourGroup for the enhanced history.
func (r *row) enhancedBucket(idx int) *EnhancedBucket {
	if n := len(r.enhanced); n == 0 || idx > r.enhanced[n-1].Index {
		r.enhanced = append(r.enhanced, EnhancedBucket{
			Index: idx,
			Start: r.lifeStart.Add(time.Duration(idx-1) * r.enhInterval),
		})
		if len(r.enhanced) > r.enhBuckets {
			r.enhanced = slices.Delete(r.enhanced, 0, len(r.enhanced)-r.enhBuckets)
		}
		return &r.enhanced[len(r.enhanced)-1]
	}
	for i := len(r.enhanced) - 1; i >= 0; i-- {
		if r.enhanced[i].Index == idx {
			return &r.enhanced[i]
		}
	}
	return nil
}

// recordHistory writes an attempt to the current life if the filter keeps
// it. The bucket index counts every attempt of the life
// (rttMonHistoryCollectionBucketIndex "increments on each operation
// attempt"), so filtered-out attempts leave gaps.
func (r *row) recordHistory(res *op.Result, ms uint64) {
	if r.lives == 0 || len(r.history) == 0 {
		return
	}
	l := &r.history[len(r.history)-1]
	l.attempts++
	if !keep(r.filter, res.Code) {
		return
	}
	b := HistoryBucket{
		Life:   l.index,
		Bucket: l.attempts,
		Sample: 1,
		Start:  res.Start,
		Code:   res.Code,
		Target: r.target,
	}
	if res.Code == op.RCOK {
		b.RTTMs = ms
	}
	l.buckets = append(l.buckets, b)
	if len(l.buckets) > r.histBuckets {
		l.buckets = slices.Delete(l.buckets, 0, len(l.buckets)-r.histBuckets)
	}
}

// keep applies rttMonHistoryAdminFilter to an attempt.
func keep(f config.HistoryFilter, code op.ReturnCode) bool {
	switch f {
	case "all":
		return true
	case "overThreshold":
		return code == op.RCOverThreshold
	case "failures":
		return !isCompletion(code)
	default: // "none" or unset
		return false
	}
}

// latestCopy returns the latest result with its own copy of the jitter
// result.
func (r *row) latestCopy() Latest {
	l := r.latest
	l.Jitter = cloneJitterResult(l.Jitter)
	return l
}

func (r *row) snapshot(opts SnapshotOptions) *Snapshot {
	s := &Snapshot{
		ID:        r.id,
		Type:      r.typ,
		Target:    r.target,
		Tag:       r.tag,
		LifeStart: r.lifeStart,
		LifeIndex: r.lifeIndex,
		Latest:    r.latestCopy(),
		Totals:    r.totals,

		TotalsJitter: cloneJitterCounters(r.totalsJitter),
	}
	if opts.Hours {
		s.Hours = make([]HourGroup, len(r.hours))
		for i, g := range r.hours {
			g.Dist = slices.Clone(g.Dist)
			g.Jitter = cloneJitterCounters(g.Jitter)
			s.Hours[i] = g
		}
	}
	if opts.History {
		n := 0
		for _, l := range r.history {
			n += len(l.buckets)
		}
		s.History = make([]HistoryBucket, 0, n)
		for _, l := range r.history {
			s.History = append(s.History, l.buckets...)
		}
	}
	// A disabled enhanced history stays nil even when requested.
	if opts.Enhanced && r.enhInterval > 0 {
		s.Enhanced = slices.Clone(r.enhanced)
		if s.Enhanced == nil {
			s.Enhanced = []EnhancedBucket{}
		}
	}
	return s
}
