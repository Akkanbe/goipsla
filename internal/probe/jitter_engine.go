// Part of the probe engine, the unit package probe is named for (its core).
//
//declscope:core

package probe

import (
	"context"
	"errors"
	"fmt"
	"net/netip"
	"time"
)

// burst is the state of one Jitter call shared with the receive loop.
type burst struct {
	opID, seq uint32
	events    chan burstEvent // capacity 2 × NumPackets: one completion and one late event per packet at most
	done      bool            // the burst has ended; guarded by the socket's mu
}

// burstEvent reports a reply or ICMP error for one packet of a burst.
type burstEvent struct {
	index      int
	outcome    Outcome // OutcomeReply or OutcomeUnreachable
	late       bool    // a reply to a packet that had already timed out
	receivedAt time.Time
	rtt        time.Duration
	recv, xmit uint32
	detail     string
}

// completeBurstLocked marks p answered and hands ev to its burst. s.mu must
// be held.
// The entry keeps a zero expireAt until the burst ends (see finish).
func (s *socket) completeBurstLocked(p *pending, ev burstEvent) {
	p.state = stateAnswered
	select {
	case p.burst.events <- ev:
	default: // cannot happen with the channel's capacity
	}
}

func (e *engine) handleTimestampReply(s *socket, m parsedMsg, from netip.Addr, rx, now time.Time) {
	key := pendKey{kind: kindTimestamp, id: m.ID, seq: m.Seq}
	s.mu.Lock()
	p := s.pending[key]
	if p == nil || p.state == stateUnarmed || p.originate != m.Originate {
		s.mu.Unlock()
		e.dropForeign(from, "no matching timestamp request")
		return
	}
	if !sameHost(from, p.target) {
		s.mu.Unlock()
		e.dropForeign(from, "timestamp reply from another address")
		return
	}
	if p.burst.done {
		s.mu.Unlock()
		e.stats.late.Add(1)
		e.log.Debug("probe: timestamp reply after its burst", "op", p.opID, "seq", p.seq, "from", from)
		if e.onLate != nil {
			e.onLate(p.opID, p.seq, p.sentAt)
		}
		return
	}
	var rtt time.Duration
	if p.state == stateWaiting {
		rtt = e.rtt(p.sentAt, now, rx, p.opID, p.seq)
		if !p.sentAt.Add(rtt).Before(p.deadline) {
			// Arrived at or after the packet's timeout, before the sender
			// loop expired it: a late reply, not a success.
			p.state = stateExpired
		}
	}
	switch {
	case p.state == stateWaiting:
		s.completeBurstLocked(p, burstEvent{
			index:      p.index,
			outcome:    OutcomeReply,
			receivedAt: p.sentAt.Add(rtt),
			rtt:        rtt,
			recv:       m.Receive,
			xmit:       m.Transmit,
		})
	case p.state == stateExpired && !p.lateSeen:
		p.lateSeen = true
		select {
		case p.burst.events <- burstEvent{index: p.index, late: true}:
		default:
		}
	default:
		// A duplicate while the burst is still running.
		e.stats.late.Add(1)
	}
	s.mu.Unlock()
}

// validateJitter checks req and returns the normalized target and source.
func validateJitter(req *JitterRequest) (target, src netip.Addr, err error) {
	switch {
	case !req.Target.IsValid():
		return target, src, errors.New("invalid target address")
	case req.Target.Unmap().Is6():
		return target, src, ErrUnsupportedFamily
	case req.NumPackets < 1:
		return target, src, fmt.Errorf("number of packets %d must be at least 1", req.NumPackets)
	case req.Interval < 0:
		return target, src, errors.New("interval must not be negative")
	case req.Timeout <= 0:
		return target, src, errors.New("timeout must be positive")
	}
	target = req.Target.Unmap()
	if req.Source.IsValid() {
		src = req.Source.Unmap()
		if !src.Is4() {
			return target, src, fmt.Errorf("source %s is not an ipv4 address", req.Source)
		}
	}
	return target, src, nil
}

// Jitter implements Engine. It sends NumPackets Timestamp Requests on the
// grid S_i = S_0 + i × Interval and returns once every packet has been
// answered or has timed out, or ctx or the engine ends.
func (e *engine) Jitter(ctx context.Context, req JitterRequest) JitterReply {
	target, src, err := validateJitter(&req)
	n := max(req.NumPackets, 0)
	packets := make([]JitterPacket, n)
	for i := range packets {
		packets[i].Index = i
		packets[i].Outcome = OutcomeError
	}
	fail := func(err error) JitterReply {
		for i := range packets {
			packets[i].Err = err
		}
		return JitterReply{Packets: packets, Err: err}
	}
	if err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	s, err := e.socketFor(sockKey{vrf: req.VRF})
	if err != nil {
		return fail(err)
	}
	ctl := connControl{Src: src, TOS: req.TOS}
	if req.Interface != "" {
		if ctl.IfIndex, err = e.lookupIfIndex(req.Interface); err != nil {
			return fail(fmt.Errorf("interface %s: %w", req.Interface, err))
		}
	}

	b := &burst{opID: req.OpID, seq: req.Seq, events: make(chan burstEvent, 2*n)}
	run := &burstRun{
		e: e, s: s, b: b, req: &req, target: target, ctl: ctl,
		packets: packets,
		entries: make([]*pending, n),
		keys:    make([]pendKey, n),
		state:   make([]packetState, n),
	}
	return run.loop(ctx)
}

type packetState uint8

const (
	pktUnsent packetState = iota
	pktWaiting
	pktDone
)

// burstRun is the sender side of one burst.
type burstRun struct {
	e       *engine
	s       *socket
	b       *burst
	req     *JitterRequest
	target  netip.Addr
	ctl     connControl
	packets []JitterPacket
	entries []*pending
	keys    []pendKey
	state   []packetState

	next     int // next packet to send
	waiting  int
	arrivals int
	sent     int
	firstErr error
}

func (r *burstRun) loop(ctx context.Context) JitterReply {
	e, n := r.e, len(r.packets)
	start := e.clk.Now()
	timer := e.clk.NewTimer(r.req.Timeout)
	defer timer.Stop()
	var stopErr error
	for {
		now := r.sendDue(start)
		r.expireDue(now)
		if r.next == n && r.waiting == 0 {
			break
		}
		// Measure the delay right before arming so that it is not stale.
		timer.Reset(r.nextWake(start).Sub(e.clk.Now()))
		e.burstParked.Store(true)
		select {
		case ev := <-r.b.events:
			r.apply(ev)
		case <-timer.C():
		case <-ctx.Done():
			stopErr = ctx.Err()
		case <-e.done:
			stopErr = ErrClosed
		}
		e.burstParked.Store(false)
		if stopErr != nil {
			break
		}
	}
	r.finish(stopErr)
	reply := JitterReply{Packets: r.packets}
	if r.sent == 0 {
		reply.Err = r.firstErr
		if reply.Err == nil {
			reply.Err = stopErr
		}
		if reply.Err == nil {
			reply.Err = errors.New("no packet could be sent")
		}
	}
	return reply
}

// sendDue sends (or skips) every packet whose grid slot has come and returns
// the current time.
func (r *burstRun) sendDue(start time.Time) time.Time {
	now := r.e.clk.Now()
	for r.next < len(r.packets) {
		slot := start.Add(time.Duration(r.next) * r.req.Interval)
		if now.Before(slot) {
			break
		}
		i := r.next
		r.next++
		if r.req.Interval > 0 && now.Sub(slot) >= r.req.Interval {
			r.packets[i].Err = ErrSkipped
			r.state[i] = pktDone
			continue
		}
		if err := r.send(i); err != nil {
			r.packets[i].Err = err
			r.state[i] = pktDone
			if r.firstErr == nil {
				r.firstErr = err
			}
		}
		now = r.e.clk.Now()
	}
	return now
}

// send registers and sends packet i.
func (r *burstRun) send(i int) error {
	e, s := r.e, r.s
	p := &pending{
		kind:   kindTimestamp,
		opID:   r.req.OpID,
		seq:    r.req.Seq,
		target: r.target,
		grace:  min(2*r.req.Timeout, maxLateGrace),
		burst:  r.b,
		index:  i,
	}
	s.mu.Lock()
	id, err := s.identFor(r.req.OpID)
	if err != nil {
		s.mu.Unlock()
		return err
	}
	seq := s.tsSeq[r.req.OpID]
	s.tsSeq[r.req.OpID] = seq + 1
	key := pendKey{kind: kindTimestamp, id: id, seq: seq}
	err = s.insertLocked(key, p)
	s.mu.Unlock()
	if err != nil {
		return err
	}

	p.sentAt = e.clk.Now()
	p.deadline = p.sentAt.Add(r.req.Timeout)
	p.originate = icmpTimestampMs(p.sentAt)
	ctl := r.ctl
	ctl.Deadline = p.deadline
	msg := buildICMPTimestamp(id, seq, p.originate)
	s.arm(key, p)
	if err := s.conn.WriteTo(msg, r.target, ctl); err != nil {
		s.remove(key, p)
		if errors.Is(err, ErrClosed) || e.isClosed() {
			return ErrClosed
		}
		return err
	}
	r.entries[i], r.keys[i] = p, key
	r.state[i] = pktWaiting
	r.packets[i].SentAt = p.sentAt
	r.packets[i].Originate = p.originate
	r.waiting++
	r.sent++
	return nil
}

// expireDue times out the waiting packets whose Timeout has passed.
func (r *burstRun) expireDue(now time.Time) {
	for i := range r.next {
		if r.state[i] != pktWaiting || now.Before(r.packets[i].SentAt.Add(r.req.Timeout)) {
			continue
		}
		p := r.entries[i]
		r.s.mu.Lock()
		expired := p.state == stateWaiting
		if expired {
			p.state = stateExpired // kept (expireAt zero) until the burst ends
		}
		r.s.mu.Unlock()
		if !expired {
			continue // answered concurrently; its event is on the channel
		}
		r.packets[i].Outcome = OutcomeTimeout
		r.state[i] = pktDone
		r.waiting--
	}
}

// nextWake is the earliest of the next grid slot and the waiting packets'
// deadlines.
func (r *burstRun) nextWake(start time.Time) time.Time {
	var wake time.Time
	if r.next < len(r.packets) {
		wake = start.Add(time.Duration(r.next) * r.req.Interval)
	}
	for i := range r.next {
		if r.state[i] != pktWaiting {
			continue
		}
		if d := r.packets[i].SentAt.Add(r.req.Timeout); wake.IsZero() || d.Before(wake) {
			wake = d
		}
	}
	return wake
}

func (r *burstRun) apply(ev burstEvent) {
	pk := &r.packets[ev.index]
	if ev.late {
		pk.Late = true
		// The receive loop may have found the reply past the deadline before
		// expireDue did; the packet is then a timeout.
		if r.state[ev.index] == pktWaiting {
			pk.Outcome = OutcomeTimeout
			r.state[ev.index] = pktDone
			r.waiting--
		}
		return
	}
	if r.state[ev.index] != pktWaiting {
		return
	}
	pk.Outcome = ev.outcome
	pk.ReceivedAt = ev.receivedAt
	pk.Detail = ev.detail
	if ev.outcome == OutcomeReply {
		pk.RTT = ev.rtt
		pk.Receive = ev.recv
		pk.Transmit = ev.xmit
		pk.ArrivalPos = r.arrivals
		r.arrivals++
	}
	r.state[ev.index] = pktDone
	r.waiting--
}

// finish ends the burst: later replies go to OnLateReply, entries get their
// grace period, and events that raced with the end are applied. stopErr is
// set when ctx or the engine ended the burst early.
func (r *burstRun) finish(stopErr error) {
	now := r.e.clk.Now()
	r.s.mu.Lock()
	r.b.done = true
	for i, p := range r.entries {
		if p == nil || r.s.pending[r.keys[i]] != p {
			continue
		}
		if p.state == stateWaiting {
			p.state = stateExpired
		}
		p.expireAt = now.Add(p.grace)
	}
	r.s.mu.Unlock()
	for {
		select {
		case ev := <-r.b.events:
			r.apply(ev)
			continue
		default:
		}
		break
	}
	if stopErr == nil {
		return
	}
	for i := range r.packets {
		if r.state[i] != pktDone {
			r.packets[i].Outcome = OutcomeError
			r.packets[i].Err = stopErr
			r.state[i] = pktDone
		}
	}
}
