//declscope:core // the CISCO-RTTMON-MIB tree is the subject of the package doc: the unit the snmp package is named for

package snmp

import (
	"sort"
	"sync"
	"time"

	"goipsla/internal/config"
	"goipsla/internal/react"
	"goipsla/internal/snmp/agentx"
	"goipsla/internal/stats"
)

// This file is the MIB tree: an ordered list of tables whose rows come from
// a view of the Source, rebuilt at most once a second so that a walk sees a
// consistent picture and does not snapshot the store on every GetNext.

// viewTTL is how long a view of the Source is reused.
const viewTTL = time.Second

// opData is everything the tables read about one operation.
type opData struct {
	id       int
	cfg      *config.Operation
	state    string
	lifeLeft time.Duration
	forever  bool
	snap     *stats.Snapshot
	reacts   []react.ReactionState
}

// row is one conceptual row: its index and what its columns read.
type row struct {
	idx   agentx.OID
	op    *opData
	hour  *stats.HourGroup
	dist  *stats.DistBucket
	hist  *stats.HistoryBucket
	react *react.ReactionState
	n     uint32 // a small integer row key (supported types / protocols)
}

// column is one column of a table; get reports false when the row has no
// value for it (the instance does not exist).
type column struct {
	num uint32
	get func(v *view, r *row) (agentx.Value, bool)
}

// table is a conceptual table, or a group of scalars (rows = one row with
// index 0).
type table struct {
	entry agentx.OID
	cols  []column // ascending by num
	rows  func(v *view) []row
}

// view is one reading of the Source.
type view struct {
	m    *mib
	at   time.Time
	ids  []int
	ops  map[int]*opData
	rows map[*table][]row
}

// mib serves the tables to the AgentX session.
type mib struct {
	src    Source
	opts   Options
	tables []*table // ascending by entry

	mu   sync.Mutex
	cur  *view
	curT time.Time
}

// newMIB returns the tree over src.
//
//declscope:package // the package's Run builds the tree the subagent serves
func newMIB(src Source, opts Options) *mib {
	m := &mib{src: src, opts: opts.withDefaults()}
	m.tables = rttMonTables()
	sort.Slice(m.tables, func(i, j int) bool { return m.tables[i].entry.Compare(m.tables[j].entry) < 0 })
	return m
}

// Uptime is the agent's sysUpTime: hundredths of a second since Start.
func (m *mib) Uptime() uint32 { return m.opts.ticks(m.opts.Clock.Now()) }

// view returns the current view, rebuilding it when it is older than
// viewTTL.
func (m *mib) view() *view {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.opts.Clock.Now()
	if m.cur != nil && now.Sub(m.curT) < viewTTL && !now.Before(m.curT) {
		return m.cur
	}
	v := &view{m: m, at: now, ops: map[int]*opData{}, rows: map[*table][]row{}}
	for _, id := range m.src.IDs() {
		cfg, ok := m.src.Config(id)
		if !ok {
			continue
		}
		d := &opData{id: id, cfg: cfg}
		d.state, d.lifeLeft, d.forever, ok = m.src.State(id)
		if !ok {
			continue // removed between IDs and State
		}
		d.snap, _ = m.src.Snapshot(id, stats.SnapshotOptions{Hours: true, History: true})
		d.reacts = m.src.Reactions(id)
		v.ids = append(v.ids, id)
		v.ops[id] = d
	}
	m.cur, m.curT = v, now
	return v
}

func (v *view) rowsOf(t *table) []row {
	rs, ok := v.rows[t]
	if !ok {
		rs = t.rows(v)
		sort.Slice(rs, func(i, j int) bool { return rs[i].idx.Compare(rs[j].idx) < 0 })
		v.rows[t] = rs
	}
	return rs
}

// tableFor returns the table whose entry is the longest prefix of name
// (the rttMonAppl scalars contain two tables), for a name that names at
// least a column under it.
func (m *mib) tableFor(name agentx.OID) *table {
	var best *table
	for _, t := range m.tables {
		if name.HasPrefix(t.entry) && len(name) > len(t.entry) && (best == nil || len(t.entry) > len(best.entry)) {
			best = t
		}
	}
	return best
}

// Get implements handler.
func (m *mib) Get(name agentx.OID) agentx.Value {
	v := m.view()
	m.mu.Lock()
	defer m.mu.Unlock()
	t := m.tableFor(name)
	if t == nil {
		return agentx.NoSuchObject
	}
	num := name[len(t.entry)]
	for _, c := range t.cols {
		if c.num != num {
			continue
		}
		idx := name[len(t.entry)+1:]
		if len(idx) == 0 {
			return agentx.NoSuchInstance // the column itself, no instance
		}
		rs := v.rowsOf(t)
		i := sort.Search(len(rs), func(i int) bool { return rs[i].idx.Compare(idx) >= 0 })
		if i < len(rs) && rs[i].idx.Compare(idx) == 0 {
			if val, ok := c.get(v, &rs[i]); ok {
				return val
			}
		}
		return agentx.NoSuchInstance
	}
	return agentx.NoSuchObject
}

// Next implements handler. Tables can nest (rttMonAppl holds two), so the
// answer is the smallest candidate over the tables, not the first found.
func (m *mib) Next(start agentx.OID, include bool, end agentx.OID) (agentx.OID, agentx.Value, bool) {
	v := m.view()
	m.mu.Lock()
	defer m.mu.Unlock()
	var best agentx.OID
	var bestVal agentx.Value
	for _, t := range m.tables {
		if best != nil && t.entry.Compare(best) >= 0 {
			break // tables are sorted by entry: nothing later can be smaller
		}
		if !start.HasPrefix(t.entry) && start.Compare(t.entry) > 0 {
			continue // the whole table is before start
		}
		if name, val, ok := t.first(v, start, include); ok && (best == nil || name.Compare(best) < 0) {
			best, bestVal = name, val
		}
	}
	if best == nil || (len(end) > 0 && best.Compare(end) >= 0) {
		return nil, agentx.Value{}, false
	}
	return best, bestVal, true
}

// first returns the first instance of t after start (or at start when
// include).
func (t *table) first(v *view, start agentx.OID, include bool) (agentx.OID, agentx.Value, bool) {
	rs := v.rowsOf(t)
	for _, c := range t.cols {
		col := t.entry.Join(c.num)
		var from int
		switch {
		case start.HasPrefix(col):
			suffix := start[len(col):]
			from = sort.Search(len(rs), func(i int) bool {
				cmp := rs[i].idx.Compare(suffix)
				return cmp > 0 || (include && cmp == 0)
			})
		case start.Compare(col) < 0:
			from = 0
		default:
			continue // the column is before start
		}
		for i := from; i < len(rs); i++ {
			if val, ok := c.get(v, &rs[i]); ok {
				return col.Join(rs[i].idx...), val, true
			}
		}
	}
	return nil, agentx.Value{}, false
}
