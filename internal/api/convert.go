//declscope:namespace new

package api

import (
	"encoding/json"

	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/react"
	"goipsla/internal/stats"
)

// This file converts the stats types to their JSON form. The Provider
// implementation uses NewOperationRow and NewOperationDetail.

// newLatestJSON converts the latest attempt. Before the first attempt only
// "valid": false and "code" are set.
func newLatestJSON(l stats.Latest) LatestJSON {
	j := LatestJSON{Valid: l.Valid, Code: l.Code}
	if !l.Valid {
		return j
	}
	j.Seq = l.Seq
	if !l.Start.IsZero() {
		t := l.Start.UTC()
		j.Start = &t
	}
	if !l.End.IsZero() {
		t := l.End.UTC()
		j.End = &t
	}
	if l.Code.HasRTT() {
		ms := op.RTTMillis(l.RTT)
		j.RTTMs = &ms
	}
	j.Detail = l.Detail
	j.Jitter = NewJitterResultJSON(l.Jitter)
	return j
}

// newJitterSideJSON converts one jitter side and adds its mean.
func newJitterSideJSON(s op.JitterSide) JitterSideJSON {
	return JitterSideJSON{Num: s.Num, SumMs: s.SumMs, Sum2Ms: s.Sum2Ms, MinMs: s.MinMs, MaxMs: s.MaxMs, AvgMs: s.AvgMs()}
}

// NewJitterResultJSON converts one burst; nil stays nil.
func NewJitterResultJSON(r *op.JitterResult) *JitterResultJSON {
	if r == nil {
		return nil
	}
	return &JitterResultJSON{
		NumPackets:       r.NumPackets,
		Sent:             r.Sent,
		Skipped:          r.Skipped,
		NumRTT:           r.NumRTT,
		RTTSumMs:         r.RTTSumMs,
		RTTSum2Ms:        r.RTTSum2Ms,
		RTTMinMs:         r.RTTMinMs,
		RTTMaxMs:         r.RTTMaxMs,
		RTTAvgMs:         rttAvgMs(r.RTTSumMs, r.NumRTT),
		NumOverThreshold: r.NumOverThreshold,
		PosSD:            newJitterSideJSON(r.PosSD),
		NegSD:            newJitterSideJSON(r.NegSD),
		PosDS:            newJitterSideJSON(r.PosDS),
		NegDS:            newJitterSideJSON(r.NegDS),
		AvgJitterMs:      r.AvgJitterMs(),
		AvgSDJitterMs:    r.AvgSDJitterMs(),
		AvgDSJitterMs:    r.AvgDSJitterMs(),
		PktLoss:          r.PktLoss,
		PktLateArrival:   r.PktLateArrival,
		PktOutSeqSD:      r.PktOutSeqSD,
		PktOutSeqDS:      r.PktOutSeqDS,
		PktOutSeqBoth:    r.PktOutSeqBoth,
		MinSucPktLoss:    r.MinSucPktLoss,
		MaxSucPktLoss:    r.MaxSucPktLoss,
		OneWay:           r.OneWay,
		NumOW:            r.NumOW,
		OWSD:             newJitterSideJSON(r.OWSD),
		OWDS:             newJitterSideJSON(r.OWDS),
	}
}

// NewJitterCountersJSON converts accumulated bursts; nil stays nil.
func NewJitterCountersJSON(c *stats.JitterCounters) *JitterCountersJSON {
	if c == nil {
		return nil
	}
	return &JitterCountersJSON{
		NumRTT:           c.NumRTT,
		RTTSumMs:         c.RTTSumMs,
		RTTSum2Ms:        c.RTTSum2Ms,
		RTTMinMs:         c.RTTMinMs,
		RTTMaxMs:         c.RTTMaxMs,
		RTTAvgMs:         rttAvgMs(c.RTTSumMs, c.NumRTT),
		NumOverThreshold: c.NumOverThreshold,
		PosSD:            newJitterSideJSON(c.PosSD),
		NegSD:            newJitterSideJSON(c.NegSD),
		PosDS:            newJitterSideJSON(c.PosDS),
		NegDS:            newJitterSideJSON(c.NegDS),
		AvgJitterMs:      c.AvgJitterMs(),
		AvgSDJitterMs:    c.AvgSDJitterMs(),
		AvgDSJitterMs:    c.AvgDSJitterMs(),
		PktLoss:          c.PktLoss,
		PktLateArrival:   c.PktLateArrival,
		PktOutSeqSD:      c.PktOutSeqSD,
		PktOutSeqDS:      c.PktOutSeqDS,
		PktOutSeqBoth:    c.PktOutSeqBoth,
		MinSucPktLoss:    c.MinSucPktLoss,
		MaxSucPktLoss:    c.MaxSucPktLoss,
		Skipped:          c.Skipped,
		NumOW:            c.NumOW,
		OWSD:             newJitterSideJSON(c.OWSD),
		OWDS:             newJitterSideJSON(c.OWDS),
	}
}

// NewCountersJSON converts counters and adds the derived values.
func NewCountersJSON(c stats.Counters) CountersJSON {
	return CountersJSON{
		Initiations:    c.Initiations,
		Completions:    c.Completions,
		OverThresholds: c.OverThresholds,
		Timeouts:       c.Timeouts,
		Busies:         c.Busies,
		Drops:          c.Drops,
		SequenceErrors: c.SequenceErrors,
		VerifyErrors:   c.VerifyErrors,
		Successes:      c.Successes(),
		Failures:       c.Failures(),
		RTTSumMs:       c.RTTSumMs,
		RTTSum2Ms:      c.RTTSum2Ms,
		RTTMinMs:       c.RTTMinMs,
		RTTMaxMs:       c.RTTMaxMs,
		RTTAvgMs:       c.AvgMs(),
		RTTStdDevMs:    c.StdDevMs(),
	}
}

// NewOperationRow builds a list row from a stats summary row. The Provider
// fills in what the manager knows: VRF, State, LifeLeftS, NextStart,
// AgeoutLeftS.
func NewOperationRow(r stats.SummaryRow) OperationRow {
	return OperationRow{
		ID:     r.ID,
		Type:   r.Type,
		Target: r.Target.String(),
		Tag:    r.Tag,
		Latest: newLatestJSON(r.Latest),
		Totals: NewCountersJSON(r.Totals),
	}
}

// NewOperationDetail builds the detail of one operation from its snapshot
// and effective configuration (VRF included). As for NewOperationRow, the
// Provider fills in State and the schedule fields.
func NewOperationDetail(s *stats.Snapshot, cfg *config.Operation) (*OperationDetail, error) {
	row := NewOperationRow(stats.SummaryRow{
		ID: s.ID, Type: s.Type, Target: s.Target, Tag: s.Tag, Latest: s.Latest, Totals: s.Totals,
	})
	row.VRF = cfg.VRF
	raw, err := config.EffectiveJSON(cfg)
	if err != nil {
		return nil, err
	}
	d := &OperationDetail{
		OperationRow: row,
		Config:       json.RawMessage(raw),
		LifeStart:    s.LifeStart.UTC(),
		LifeIndex:    s.LifeIndex,
		TotalsJitter: NewJitterCountersJSON(s.TotalsJitter),
	}
	for _, h := range s.Hours {
		d.Hours = append(d.Hours, NewHourGroupJSON(h))
	}
	for _, b := range s.History {
		d.History = append(d.History, HistoryBucketJSON{
			Life: b.Life, Bucket: b.Bucket, Sample: b.Sample, Start: b.Start.UTC(),
			RTTMs: b.RTTMs, Code: b.Code, Target: b.Target.String(),
		})
	}
	for _, e := range s.Enhanced {
		d.Enhanced = append(d.Enhanced, EnhancedBucketJSON{
			Index: e.Index, Start: e.Start.UTC(), Counters: NewCountersJSON(e.Counters),
		})
	}
	return d, nil
}

// NewHourGroupJSON converts an hour group and its distribution.
func NewHourGroupJSON(h stats.HourGroup) HourGroupJSON {
	j := HourGroupJSON{
		Index:    h.Index,
		Start:    h.Start.UTC(),
		Counters: NewCountersJSON(h.Counters),
		Dist:     make([]DistBucketJSON, 0, len(h.Dist)),
		Jitter:   NewJitterCountersJSON(h.Jitter),
	}
	for _, b := range h.Dist {
		db := DistBucketJSON{
			Index:          b.Index,
			LowerMs:        b.LowerMs,
			Completions:    b.Completions,
			OverThresholds: b.OverThresholds,
			RTTSumMs:       b.RTTSumMs,
			RTTSum2Ms:      b.RTTSum2Ms,
			RTTMinMs:       b.RTTMinMs,
			RTTMaxMs:       b.RTTMaxMs,
		}
		if b.UpperMs != 0 {
			u := b.UpperMs
			db.UpperMs = &u
		}
		db.RTTAvgMs = rttAvgMs(b.RTTSumMs, b.Completions)
		if h.Counters.Completions > 0 {
			db.Percent = 100 * float64(b.Completions) / float64(h.Counters.Completions)
		}
		j.Dist = append(j.Dist, db)
	}
	return j
}

// NewReactionJSON converts the state of one reaction row of operation id.
func NewReactionJSON(id int, st react.ReactionState) ReactionJSON {
	j := ReactionJSON{
		OpID: id, Element: st.Element, ThresholdType: st.ThresholdType,
		Upper: st.Upper, Lower: st.Lower, Count: st.Count, X: st.X, Y: st.Y,
		Action: st.Action, Occurred: st.Occurred, Value: st.Value, Changes: st.Changes,
	}
	if !st.LastChange.IsZero() {
		t := st.LastChange.UTC()
		j.LastChange = &t
	}
	return j
}

// NewTrackJSON converts the state of one track.
func NewTrackJSON(st react.TrackState) TrackJSON {
	j := TrackJSON{
		ID: st.ID, Operation: st.Operation, Mode: st.Mode, State: st.State, Pending: st.Pending,
		Changes: st.Changes, LatestRC: st.LatestRC,
		DelayUp: st.DelayUp.String(), DelayDown: st.DelayDown.String(),
	}
	if !st.LastChange.IsZero() {
		t := st.LastChange.UTC()
		j.LastChange = &t
	}
	if st.LatestRC.HasRTT() {
		ms := op.RTTMillis(st.LatestRTT)
		j.LatestRTTMs = &ms
	}
	return j
}
