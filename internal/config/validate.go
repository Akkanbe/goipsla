//declscope:namespace parse

package config

import (
	"slices"
	"sort"
	"strconv"
	"time"
)

// expand turns the parsed file into a Config: it expands templates and targets,
// fills in defaults and runs the checks that need the effective operation.
// Errors are appended to d.errs; the returned Config is meaningful only when
// there are none.
func (d *parser) expand(f *parsedFile) *Config {
	cfg := &Config{Global: f.global}
	known := make(map[int]string) // id -> where it was first defined
	usedTemplates := make(map[string]bool)
	for i, item := range f.operations {
		p := indexPath("operations", i)
		var tmpl *parsedOp
		tmplName := ""
		if item.template != nil {
			tmplName = *item.template
			usedTemplates[tmplName] = true
			tmpl = f.templates[tmplName]
			if tmpl == nil {
				d.add(joinPath(p, "template"), "unknown template %q", tmplName)
			}
		}

		var targets []parsedTarget
		switch {
		case item.hasTargets:
			if item.target != nil {
				d.add(joinPath(p, "target"), "cannot be used together with targets")
			}
			if item.id != nil || item.idFailed {
				d.add(joinPath(p, "id"), "cannot be used together with targets; the keys of targets are the ids")
			}
			if item.template == nil && !item.failed["template"] {
				d.add(joinPath(p, "template"), "required when targets is used")
			}
			targets = item.targets
		default:
			if item.id == nil && !item.idFailed {
				d.add(joinPath(p, "id"), "required")
			}
			if item.target == nil {
				d.add(joinPath(p, "target"), "required")
			}
			if item.id != nil && item.target != nil {
				t := *item.target
				t.id = *item.id
				targets = []parsedTarget{t}
			}
		}

		for _, t := range targets {
			where := p
			if item.hasTargets {
				where = t.path
			}
			if first, dup := known[t.id]; dup {
				d.add(opPath(t.id)+".id", "duplicate id %d (first defined at %s)", t.id, first)
				continue
			}
			known[t.id] = where
			if item.template != nil && tmpl == nil {
				continue // unknown template: already reported
			}
			if op := d.operation(item, tmpl, tmplName, t); op != nil {
				cfg.Operations = append(cfg.Operations, op)
			}
		}
	}
	if n := len(known); n > operationsLimit {
		d.add("operations", "at most %d operations are allowed, got %d", operationsLimit, n)
	}
	sort.Slice(cfg.Operations, func(i, j int) bool { return cfg.Operations[i].ID < cfg.Operations[j].ID })

	d.expandTracks(cfg, f, known)
	d.warnUnused(cfg, f, usedTemplates)
	return cfg
}

// expandTracks checks the tracks and actions against the operations
// (known: id -> where it was defined) and fills cfg.Tracks and cfg.Actions.
func (d *parser) expandTracks(cfg *Config, f *parsedFile, known map[int]string) {
	trackIDs := make(map[int]bool)
	for i, rt := range f.tracks {
		p := indexPath("tracks", i)
		if rt.id != nil {
			if trackIDs[*rt.id] {
				d.add(joinPath(p, "id"), "duplicate id %d", *rt.id)
			}
			trackIDs[*rt.id] = true
		}
		if rt.operation != nil {
			if _, ok := known[*rt.operation]; !ok {
				d.add(joinPath(p, "operation"), "operation %d is not defined", *rt.operation)
			}
		}
		if rt.id != nil && rt.operation != nil {
			cfg.Tracks = append(cfg.Tracks, Track{
				ID: *rt.id, Operation: *rt.operation, Mode: rt.mode, DelayUp: rt.delayUp, DelayDown: rt.delayDown,
			})
		}
	}
	sort.SliceStable(cfg.Tracks, func(i, j int) bool { return cfg.Tracks[i].ID < cfg.Tracks[j].ID })

	for i, ra := range f.actions {
		a := Action{On: ra.on, Webhook: ra.webhook, Exec: ra.exec}
		if ra.track != nil {
			if !trackIDs[*ra.track] {
				d.add(joinPath(indexPath("actions", i), "track"), "track %d is not defined", *ra.track)
			}
			a.Track = *ra.track
		}
		cfg.Actions = append(cfg.Actions, a)
		if ra.track != nil && !slices.Contains(ra.on, eventTrackUp) && !slices.Contains(ra.on, eventTrackDown) {
			d.warn(joinPath(indexPath("actions", i), "track"), "has no effect: on lists no track-up / track-down event")
		}
	}
}

// warnUnused adds the warnings for settings that end up doing nothing:
// templates no operation uses, and reaction actions whose receiver
// (global.syslog, global.snmp.traps) is not configured.
func (d *parser) warnUnused(cfg *Config, f *parsedFile, usedTemplates map[string]bool) {
	names := make([]string, 0, len(f.templates))
	for name := range f.templates {
		if !usedTemplates[name] {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	for _, name := range names {
		d.warn(joinPath("templates", name), "not used by any operation")
	}

	reactions := func(n int) string {
		if n == 1 {
			return "1 reaction"
		}
		return strconv.Itoa(n) + " reactions"
	}
	noSyslog := cfg.Global.Syslog == nil
	noTrap := cfg.Global.SNMP == nil || len(cfg.Global.SNMP.Traps) == 0
	type miss struct {
		first string
		n     int
	}
	var syslogMiss, trapMiss miss
	for _, op := range cfg.Operations {
		for j, r := range op.React {
			where := joinPath(indexPath(joinPath(opPath(op.ID), "react"), j), "action")
			if noSyslog && (r.Action == ActionSyslog || r.Action == ActionTrapAndSyslog) {
				if syslogMiss.n == 0 {
					syslogMiss.first = where
				}
				syslogMiss.n++
			}
			if noTrap && (r.Action == ActionTrap || r.Action == ActionTrapAndSyslog) {
				if trapMiss.n == 0 {
					trapMiss.first = where
				}
				trapMiss.n++
			}
		}
	}
	if syslogMiss.n > 0 {
		d.warn(syslogMiss.first, "sends to syslog, but global.syslog is not set: no syslog message is sent (%s)", reactions(syslogMiss.n))
	}
	if trapMiss.n > 0 {
		d.warn(trapMiss.first, "sends a trap, but global.snmp.traps is empty: no trap is sent (%s)", reactions(trapMiss.n))
	}
}

// pickParsed returns the operation's value if set, else the template's.
func pickParsed[T any](tmpl, item *T) *T {
	if item != nil {
		return item
	}
	return tmpl
}

func parsedOr[T any](v *T, def T) T {
	if v != nil {
		return *v
	}
	return def
}

// mergeParsed returns the operation after overlaying the item on its template.
func mergeParsed(t, o *parsedOp) *parsedOp {
	if t == nil {
		return o
	}
	m := &parsedOp{
		opType:          pickParsed(t.opType, o.opType),
		sourceIP:        pickParsed(t.sourceIP, o.sourceIP),
		sourceInterface: pickParsed(t.sourceInterface, o.sourceInterface),
		vrf:             pickParsed(t.vrf, o.vrf),
		tos:             pickParsed(t.tos, o.tos),
		trafficClass:    pickParsed(t.trafficClass, o.trafficClass),
		flowLabel:       pickParsed(t.flowLabel, o.flowLabel),
		frequency:       pickParsed(t.frequency, o.frequency),
		timeout:         pickParsed(t.timeout, o.timeout),
		threshold:       pickParsed(t.threshold, o.threshold),
		requestDataSize: pickParsed(t.requestDataSize, o.requestDataSize),
		dataPattern:     pickParsed(t.dataPattern, o.dataPattern),
		verifyData:      pickParsed(t.verifyData, o.verifyData),
		tag:             pickParsed(t.tag, o.tag),
		owner:           pickParsed(t.owner, o.owner),
		interval:        pickParsed(t.interval, o.interval),
		numPackets:      pickParsed(t.numPackets, o.numPackets),
		oneWayDelay:     pickParsed(t.oneWayDelay, o.oneWayDelay),
		history: parsedHistory{
			livesKept:    pickParsed(t.history.livesKept, o.history.livesKept),
			bucketsKept:  pickParsed(t.history.bucketsKept, o.history.bucketsKept),
			filter:       pickParsed(t.history.filter, o.history.filter),
			hoursKept:    pickParsed(t.history.hoursKept, o.history.hoursKept),
			distKept:     pickParsed(t.history.distKept, o.history.distKept),
			distInterval: pickParsed(t.history.distInterval, o.history.distInterval),
		},
		react:    t.react,
		reactSet: t.reactSet,
		failed:   make(map[string]bool),
	}
	switch te, oe := t.history.enhanced, o.history.enhanced; {
	case te != nil && oe != nil:
		m.history.enhanced = &parsedEnhanced{interval: pickParsed(te.interval, oe.interval), buckets: pickParsed(te.buckets, oe.buckets)}
	default:
		m.history.enhanced = pickParsed(te, oe)
	}
	switch ts, os := t.schedule, o.schedule; {
	case ts != nil && os != nil:
		m.schedule = &parsedSchedule{
			life:      pickParsed(ts.life, os.life),
			forever:   pickParsed(ts.forever, os.forever),
			start:     pickParsed(ts.start, os.start),
			ageout:    pickParsed(ts.ageout, os.ageout),
			recurring: pickParsed(ts.recurring, os.recurring),
		}
		if os.forever != nil { // life written on the operation wins as a whole
			m.schedule.life = os.life
		}
	default:
		m.schedule = pickParsed(ts, os)
	}
	if o.reactSet {
		m.react, m.reactSet = o.react, true
	}
	for k := range t.failed {
		m.failed[k] = true
	}
	for k := range o.failed {
		m.failed[k] = true
	}
	return m
}

// operation builds and checks one effective operation.
func (d *parser) operation(item, tmpl *parsedOp, tmplName string, tgt parsedTarget) *Operation {
	p := opPath(tgt.id)
	m := mergeParsed(tmpl, item)

	op := &Operation{
		ID:              tgt.id,
		Target:          tgt.addr,
		TargetName:      tgt.name,
		SourceInterface: parsedOr(m.sourceInterface, ""),
		VRF:             parsedOr(m.vrf, ""),
		RequestDataSize: parsedOr(m.requestDataSize, defaultRequestDataSize),
		DataPattern:     parsedOr(m.dataPattern, uint32(defaultDataPattern)),
		VerifyData:      parsedOr(m.verifyData, false),
		Tag:             parsedOr(m.tag, ""),
		Owner:           parsedOr(m.owner, ""),
		Template:        tmplName,
		Interval:        parsedOr(m.interval, defaultInterval),
		NumPackets:      parsedOr(m.numPackets, defaultNumPackets),
		OneWayDelay:     parsedOr(m.oneWayDelay, false),
		Stats: StatsConfig{
			HoursKept:    parsedOr(m.history.hoursKept, defaultHoursKept),
			DistBuckets:  parsedOr(m.history.distKept, defaultDistBuckets),
			DistInterval: parsedOr(m.history.distInterval, defaultDistInterval),
		},
		History: HistoryConfig{
			Lives:   parsedOr(m.history.livesKept, defaultLivesKept),
			Buckets: parsedOr(m.history.bucketsKept, defaultBucketsKept),
			Filter:  parsedOr(m.history.filter, FilterNone),
		},
	}
	if m.opType != nil {
		op.Type = *m.opType
	} else if !m.failed["type"] {
		d.add(joinPath(p, "type"), "required")
	}

	d.family(p, item, tmpl, m, tgt, op)

	// Keys that do not apply to the type.
	na := func(set bool, key string) {
		if set {
			d.add(joinPath(p, key), "not applicable to %s", op.Type)
		}
	}
	switch op.Type {
	case ICMPJitter:
		na(m.requestDataSize != nil, "request-data-size")
		na(m.dataPattern != nil, "data-pattern")
		na(m.verifyData != nil, "verify-data")
	case ICMPEcho:
		na(m.interval != nil, "interval")
		na(m.numPackets != nil, "num-packets")
		na(m.oneWayDelay != nil, "one-way-delay")
	}

	d.timing(p, m, op)
	if e := m.history.enhanced; e != nil {
		op.Enhanced = &EnhancedHistory{
			Interval: parsedOr(e.interval, defaultEnhancedInterval),
			Buckets:  parsedOr(e.buckets, defaultEnhancedBuckets),
		}
	}
	if m.schedule != nil {
		op.Schedule = d.effectiveSchedule(joinPath(p, "schedule"), m.schedule)
	}
	op.React = d.reactions(joinPath(p, "react"), op, m.react)
	return op
}

// family applies the keys that depend on the target's address family: the
// source-ip family, tos and traffic-class (the same DS byte, carried over
// when a template or a targets item writes the other one) and flow-label
// (ignored for IPv4 unless written on a single-target item, which is an
// error).
func (d *parser) family(p string, item, tmpl, m *parsedOp, tgt parsedTarget, op *Operation) {
	// A family-specific key written on a single-target item is an error when
	// it does not match the target.
	explicit := !item.hasTargets
	if !tgt.ok {
		return
	}
	v4 := tgt.addr.Is4()
	if op.Type == ICMPJitter && !v4 {
		d.add(joinPath(p, "target"), "icmp-jitter supports IPv4 targets only")
	}
	if m.sourceIP != nil {
		if m.sourceIP.Is4() != v4 {
			d.add(joinPath(p, "source-ip"), "must be the same address family as target")
		}
		op.SourceIP = *m.sourceIP
	}
	switch {
	case v4:
		if m.tos != nil {
			op.TOS = *m.tos
		}
		if m.trafficClass != nil {
			if explicit && item.trafficClass != nil {
				d.add(joinPath(p, "traffic-class"), "applies to IPv6 targets only; use tos")
			} else if m.tos == nil {
				op.TOS = *m.trafficClass
			}
		}
		if m.flowLabel != nil {
			if explicit && item.flowLabel != nil {
				d.add(joinPath(p, "flow-label"), "applies to IPv6 targets only")
			} else {
				from := item.path // a targets item
				if item.flowLabel == nil {
					from = tmpl.path
				}
				d.warn(joinPath(from, "flow-label"), "ignored for the IPv4 targets")
			}
		}
	default:
		if m.trafficClass != nil {
			op.TrafficClass = *m.trafficClass
		}
		if m.tos != nil {
			if explicit && item.tos != nil {
				d.add(joinPath(p, "tos"), "applies to IPv4 targets only; use traffic-class")
			} else if m.trafficClass == nil {
				op.TrafficClass = *m.tos
			}
		}
		if m.flowLabel != nil {
			op.FlowLabel = *m.flowLabel
		}
	}
}

// timing fills frequency, timeout and threshold and enforces
// threshold <= timeout <= frequency (and, for icmp-jitter,
// timeout + interval*num-packets <= frequency).
//
// Unwritten timeout and threshold default to 5000ms but are lowered to fit
// under the written frequency / timeout, so that e.g. "frequency: 2s" alone is
// valid. Only written values can violate the rules.
func (d *parser) timing(p string, m *parsedOp, op *Operation) {
	if m.failed["frequency"] || m.failed["timeout"] || m.failed["threshold"] ||
		m.failed["interval"] || m.failed["num-packets"] {
		op.Frequency = parsedOr(m.frequency, defaultFrequency)
		op.Timeout = parsedOr(m.timeout, defaultTimeout)
		op.Threshold = parsedOr(m.threshold, defaultThreshold)
		return
	}
	op.Frequency = parsedOr(m.frequency, defaultFrequency)
	var burst time.Duration // time the jitter packets take to send
	if op.Type == ICMPJitter {
		burst = op.Interval * time.Duration(op.NumPackets)
	}
	if m.timeout != nil {
		op.Timeout = *m.timeout
	} else {
		op.Timeout = max(timeoutLimit.min, min(defaultTimeout, op.Frequency-burst))
	}
	if m.threshold != nil {
		op.Threshold = *m.threshold
	} else {
		op.Threshold = min(defaultThreshold, op.Timeout)
	}

	if op.Threshold > op.Timeout {
		d.add(joinPath(p, "threshold"), "must not exceed timeout (%s)", op.Timeout)
	}
	if op.Type == ICMPJitter {
		if op.Timeout+burst > op.Frequency {
			d.add(joinPath(p, "frequency"), "must be at least timeout + interval * num-packets (%s) for icmp-jitter", op.Timeout+burst)
		}
	} else if op.Timeout > op.Frequency {
		d.add(joinPath(p, "timeout"), "must not exceed frequency (%s)", op.Frequency)
	}
}

func (d *parser) effectiveSchedule(p string, s *parsedSchedule) *Schedule {
	out := &Schedule{
		Forever:   parsedOr(s.forever, true),
		Life:      parsedOr(s.life, 0),
		Start:     StartNow,
		Ageout:    parsedOr(s.ageout, 0),
		Recurring: parsedOr(s.recurring, false),
	}
	if s.start != nil {
		out.Start = s.start.kind
		out.After = s.start.after
		out.At = s.start.at
		out.Daily = s.start.daily
	}
	daily := out.Daily
	if out.Recurring {
		rp := joinPath(p, "recurring")
		if !daily {
			d.add(rp, "requires start-time in HH:MM[:SS] form")
		}
		if out.Forever || out.Life >= recurringLimit {
			d.add(rp, "requires life shorter than 24h")
		} else if out.Ageout != 0 && out.Life+out.Ageout <= recurringLimit {
			d.add(rp, "requires ageout 0s or life + ageout longer than 24h")
		}
	}
	if *out == (Schedule{Forever: true, Start: StartNow}) {
		return nil // "schedule: {}" is the same as no schedule
	}
	return out
}

func (d *parser) reactions(p string, op *Operation, rs []parsedReact) []Reaction {
	typ := op.Type
	var out []Reaction
	seen := make(map[string]bool)
	for j, r := range rs {
		rp := indexPath(p, j)
		if r.element == nil {
			continue // reported while decoding
		}
		el := *r.element
		info, known := reactElements[typ][el]
		if typ != "" && !known {
			d.add(joinPath(rp, "element"), "unknown element %q for %s", el, typ)
		}
		if known && info.oneWay && !op.OneWayDelay {
			d.add(joinPath(rp, "element"), "element %q needs one-way-delay: true", el)
		}
		if seen[el] {
			d.add(joinPath(rp, "element"), "duplicate element %q", el)
		}
		seen[el] = true

		tt := parsedOr(r.thresholdType, ThresholdNever)
		if info.boolean {
			if r.upper != nil {
				d.add(joinPath(rp, "upper"), "not applicable to element %s", el)
			}
			if r.lower != nil {
				d.add(joinPath(rp, "lower"), "not applicable to element %s", el)
			}
			if tt == ThresholdAverage {
				d.add(joinPath(rp, "threshold-type"), "average is not available for element %s", el)
			}
		}
		if r.count != nil && tt != ThresholdConsecutive && tt != ThresholdAverage {
			d.add(joinPath(rp, "count"), "only applies to threshold-type consecutive or average")
		}
		if (r.x != nil || r.y != nil) && tt != ThresholdXofY {
			key := "x"
			if r.x == nil {
				key = "y"
			}
			d.add(joinPath(rp, key), "only applies to threshold-type xofy")
		}
		x, y := parsedOr(r.x, defaultThresholdCount), parsedOr(r.y, defaultThresholdCount)
		if x > y {
			d.add(joinPath(rp, "x"), "must not exceed y (%d)", y)
		}
		// Cisco's threshold-value takes both values: a lone upper or lower
		// would be checked against a default the user did not write.
		if (r.upper == nil) != (r.lower == nil) && !info.boolean {
			key := "upper"
			if r.upper == nil {
				key = "lower"
			}
			d.add(joinPath(rp, key), "upper and lower must be given together")
		}
		upper, lower := parsedOr(r.upper, info.upper), parsedOr(r.lower, info.lower)
		if lower > upper && (r.upper == nil) == (r.lower == nil) {
			d.add(joinPath(rp, "lower"), "must not exceed upper (%d)", upper)
		}
		out = append(out, Reaction{
			Element:       el,
			ThresholdType: tt,
			Count:         parsedOr(r.count, defaultThresholdCount),
			X:             x,
			Y:             y,
			Upper:         upper,
			Lower:         lower,
			Action:        parsedOr(r.action, actionNone),
		})
	}
	return out
}
