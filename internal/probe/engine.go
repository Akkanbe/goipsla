// Part of the probe engine, the unit package probe is named for (its core).
//
//declscope:core

package probe

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/netip"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"goipsla/internal/clock"
)

const (
	// maxLateGrace caps how long a timed-out or answered request stays in the
	// pending table to recognize late and duplicate replies.
	maxLateGrace = 60 * time.Second
	// sweepInterval is the period of the timer that purges expired entries.
	sweepInterval = time.Second
	// ifIndexTTL is how long an interface name to index lookup is cached.
	ifIndexTTL = 30 * time.Second
	// vrfRetryAfter is how long a VRF socket that failed to open is not
	// retried; requests on that VRF fail with the remembered error meanwhile.
	vrfRetryAfter = 30 * time.Second
	// recvErrorLogEvery throttles the warning for a receive loop that keeps
	// failing.
	recvErrorLogEvery = time.Minute
	// readBufferSize holds the largest IPv4 packet.
	readBufferSize = 65536
	// readErrorBackoff throttles a receive loop that keeps failing.
	readErrorBackoff = 10 * time.Millisecond
	// maxRxLag bounds the kernel-to-user-space delay subtracted from an RTT.
	// A larger (or negative) difference between the wall-clock read time and
	// the kernel timestamp means the wall clock stepped, and the correction
	// is dropped.
	maxRxLag = time.Second
)

// New returns an Engine backed by raw sockets. A socket is opened per
// (address family, VRF) on first use and shared until Close. The default
// (no VRF) IPv4 and IPv6 sockets are opened here: New fails if the IPv4
// socket cannot be opened (typically EPERM without CAP_NET_RAW); if only the
// IPv6 socket fails, New logs a warning and IPv6 requests end in
// OutcomeError.
func New(opts Options) (Engine, error) {
	clk := opts.Clock
	if clk == nil {
		clk = clock.Real()
	}
	open := func(v6 bool, vrf string) (packetConn, error) { return openRaw(v6, vrf, clk) }
	return newEngine(opts, open, interfaceIndex)
}

func interfaceIndex(name string) (int, error) {
	ifi, err := net.InterfaceByName(name)
	if err != nil {
		return 0, err
	}
	return ifi.Index, nil
}

type sockKey struct {
	v6  bool
	vrf string
}

// reqKind distinguishes Echo and Timestamp requests, which share the
// (Identifier, Sequence) space of a socket.
type reqKind uint8

const (
	kindEcho reqKind = iota
	kindTimestamp
)

type pendKey struct {
	kind    reqKind
	id, seq uint16
}

type pendState uint8

const (
	stateUnarmed  pendState = iota // registered but not yet sent; never matched
	stateWaiting                   // sent, waiting for a reply
	stateAnswered                  // completed by a reply or an ICMP error
	stateExpired                   // timed out or abandoned
)

// pending is a request in the pending table.
type pending struct {
	kind      reqKind
	opID, seq uint32 // for a Timestamp packet, seq is the burst number
	target    netip.Addr
	data      []byte // Echo: data part as sent, for Verify
	verify    bool
	sentAt    time.Time
	deadline  time.Time // sentAt + Timeout: a reply arriving at or after it is late
	grace     time.Duration
	state     pendState
	expireAt  time.Time  // when an answered or expired entry may be purged; zero keeps it
	ch        chan Reply // Echo: buffered 1; receives the completion

	// Timestamp packets of a jitter burst
	burst     *burst
	index     int    // packet index in the burst
	originate uint32 // Originate Timestamp sent
	lateSeen  bool   // a late reply was already reported to the burst
}

// socket is one shared raw socket with its pending table and identifiers.
type socket struct {
	key  sockKey
	conn packetConn

	mu      sync.Mutex
	pending map[pendKey]*pending
	idents  map[uint32]uint16 // OpID -> ICMP Identifier, fixed once assigned
	owners  map[uint16]uint32 // ICMP Identifier -> OpID
	tsSeq   map[uint32]uint16 // OpID -> next Timestamp Request sequence number
}

// counters are diagnostics for packets the engine drops (see Stats).
type counters struct {
	foreign    atomic.Uint64
	malformed  atomic.Uint64
	late       atomic.Uint64
	other      atomic.Uint64
	recvErrors atomic.Uint64
}

// vrfOpenError remembers why a VRF socket could not be opened.
type vrfOpenError struct {
	err     error
	retryAt time.Time
}

type ifCacheEntry struct {
	index int
	err   error
	at    time.Time
}

type engine struct {
	log     *slog.Logger
	clk     clock.Clock
	onLate  func(opID, seq uint32, sentAt time.Time)
	open    connOpener
	ifIndex func(string) (int, error)

	mu      sync.Mutex
	sockets map[sockKey]*socket
	v6Err   error                    // why the default IPv6 socket is unavailable
	vrfErrs map[sockKey]vrfOpenError // VRF sockets that failed to open, until retryAt
	closed  bool
	done    chan struct{}
	wg      sync.WaitGroup

	ifMu    sync.Mutex
	ifCache map[string]ifCacheEntry

	flowWarn sync.Once
	stats    counters

	// burstParked is true while a Jitter loop waits in its select. Tests
	// with a fake clock wait for it before advancing the clock.
	burstParked atomic.Bool
}

func newEngine(opts Options, open connOpener, ifIndex func(string) (int, error)) (*engine, error) {
	e := &engine{
		log:     opts.Logger,
		clk:     opts.Clock,
		onLate:  opts.OnLateReply,
		open:    open,
		ifIndex: ifIndex,
		sockets: make(map[sockKey]*socket),
		vrfErrs: make(map[sockKey]vrfOpenError),
		done:    make(chan struct{}),
		ifCache: make(map[string]ifCacheEntry),
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	if e.clk == nil {
		e.clk = clock.Real()
	}
	c4, err := open(false, "")
	if err != nil {
		return nil, fmt.Errorf("probe: %w", err)
	}
	e.addSocketLocked(sockKey{}, c4)
	if c6, err := open(true, ""); err != nil {
		e.v6Err = fmt.Errorf("ipv6 unavailable: %w", err)
		e.log.Warn("probe: cannot open the ipv6 raw socket; ipv6 operations will fail", "err", err)
	} else {
		e.addSocketLocked(sockKey{v6: true}, c6)
	}
	e.wg.Add(1)
	go e.sweepLoop()
	return e, nil
}

// addSocketLocked registers conn and starts its receive loop. e.mu must be
// held, or e must not yet be shared.
func (e *engine) addSocketLocked(k sockKey, conn packetConn) *socket {
	s := &socket{
		key:     k,
		conn:    conn,
		pending: make(map[pendKey]*pending),
		idents:  make(map[uint32]uint16),
		owners:  make(map[uint16]uint32),
		tsSeq:   make(map[uint32]uint16),
	}
	e.sockets[k] = s
	e.wg.Add(1)
	go e.recvLoop(s)
	return s
}

// socketFor returns the socket for k, opening a VRF socket on first use.
func (e *engine) socketFor(k sockKey) (*socket, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed {
		return nil, ErrClosed
	}
	if s, ok := e.sockets[k]; ok {
		return s, nil
	}
	if k.vrf == "" {
		// Only the default IPv6 socket can be missing; it is not retried.
		return nil, e.v6Err
	}
	now := e.clk.Now()
	if f, ok := e.vrfErrs[k]; ok && now.Before(f.retryAt) {
		return nil, f.err
	}
	conn, err := e.open(k.v6, k.vrf)
	if err != nil {
		err = fmt.Errorf("vrf %s: %w", k.vrf, err)
		if _, seen := e.vrfErrs[k]; !seen {
			// Warn once per VRF; every request on it reports err meanwhile.
			e.log.Warn("probe: cannot open the vrf raw socket; its operations fail until it opens",
				"vrf", k.vrf, "ipv6", k.v6, "err", err, "retry_after", vrfRetryAfter.String())
		}
		e.vrfErrs[k] = vrfOpenError{err: err, retryAt: now.Add(vrfRetryAfter)}
		return nil, err
	}
	if _, failed := e.vrfErrs[k]; failed {
		delete(e.vrfErrs, k)
		e.log.Info("probe: opened the vrf raw socket after earlier failures", "vrf", k.vrf, "ipv6", k.v6)
	} else {
		e.log.Debug("probe: opened vrf socket", "vrf", k.vrf, "ipv6", k.v6)
	}
	return e.addSocketLocked(k, conn), nil
}

// Close closes every socket, stops the receive loops and makes waiting and
// future Echo calls return OutcomeError with ErrClosed.
func (e *engine) Close() error {
	e.mu.Lock()
	if e.closed {
		e.mu.Unlock()
		return nil
	}
	e.closed = true
	close(e.done)
	socks := make([]*socket, 0, len(e.sockets))
	for _, s := range e.sockets {
		socks = append(socks, s)
	}
	e.mu.Unlock()

	var errs []error
	for _, s := range socks {
		if err := s.conn.Close(); err != nil {
			errs = append(errs, err)
		}
	}
	e.wg.Wait()
	return errors.Join(errs...)
}

// validate checks req and returns the normalized target and source.
func validate(req *Request) (target, src netip.Addr, err error) {
	if !req.Target.IsValid() {
		return target, src, errors.New("invalid target address")
	}
	target = req.Target.Unmap()
	v6 := target.Is6()
	limit := maxPayloadDataSize4
	if v6 {
		limit = maxPayloadDataSize6
	}
	switch {
	case req.DataSize < MinDataSize:
		return target, src, fmt.Errorf("request data size %d is below the minimum %d", req.DataSize, MinDataSize)
	case req.DataSize > limit:
		return target, src, fmt.Errorf("request data size %d exceeds the maximum %d", req.DataSize, limit)
	case req.Timeout <= 0:
		return target, src, errors.New("timeout must be positive")
	case v6 && req.FlowLabel > 0xfffff:
		return target, src, fmt.Errorf("flow label 0x%x exceeds 20 bits", req.FlowLabel)
	}
	if req.Source.IsValid() {
		src = req.Source.Unmap()
		if src.Is6() != v6 {
			return target, src, fmt.Errorf("source %s and target %s differ in address family", req.Source, req.Target)
		}
	}
	return target, src, nil
}

// Echo implements Engine.
func (e *engine) Echo(ctx context.Context, req Request) Reply {
	target, src, err := validate(&req)
	if err != nil {
		return Reply{Outcome: OutcomeError, Err: err}
	}
	if err := ctx.Err(); err != nil {
		return Reply{Outcome: OutcomeError, Err: err}
	}
	v6 := target.Is6()
	s, err := e.socketFor(sockKey{v6: v6, vrf: req.VRF})
	if err != nil {
		return Reply{Outcome: OutcomeError, Err: err}
	}
	ctl := connControl{Src: src, TOS: req.TOS}
	if v6 {
		ctl.FlowLabel = req.FlowLabel
	}
	if req.Interface != "" {
		if ctl.IfIndex, err = e.lookupIfIndex(req.Interface); err != nil {
			return Reply{Outcome: OutcomeError, Err: fmt.Errorf("interface %s: %w", req.Interface, err)}
		}
	}
	if zone := target.Zone(); zone != "" {
		if ctl.ScopeID, err = e.zoneIndex(zone); err != nil {
			return Reply{Outcome: OutcomeError, Err: fmt.Errorf("zone %s: %w", zone, err)}
		}
	}

	// Build the packet with a placeholder send time; the header is rewritten
	// right before sending.
	data := buildPayload(req.DataSize, req.OpID, req.Seq, req.Pattern, time.Time{})
	p := &pending{
		opID:   req.OpID,
		seq:    req.Seq,
		target: target.WithZone(""),
		data:   data,
		verify: req.Verify,
		grace:  min(2*req.Timeout, maxLateGrace),
		ch:     make(chan Reply, 1),
	}
	key, err := s.register(p)
	if err != nil {
		return Reply{Outcome: OutcomeError, Err: err}
	}

	// The timeout runs from the send time, which is taken right before
	// sendmsg; a send delayed past it (EAGAIN) ends in OutcomeTimeout.
	p.sentAt = e.clk.Now()
	timer := e.clk.NewTimer(req.Timeout)
	defer timer.Stop()
	p.deadline = p.sentAt.Add(req.Timeout)
	ctl.Deadline = p.deadline
	putPayloadHeader(data, req.OpID, req.Seq, p.sentAt)
	msg := buildICMPEcho(v6, key.id, key.seq, data)
	// p.sentAt and p.data are published to the receive loop by s.mu in arm.
	s.arm(key, p)

	err = s.conn.WriteTo(msg, target, ctl)
	if errors.Is(err, errConnSendTimeout) {
		s.remove(key, p)
		return Reply{Outcome: OutcomeTimeout, SentAt: p.sentAt, Detail: "send timed out"}
	}
	var fle *connFlowLabelError
	if errors.As(err, &fle) {
		// The packet went out without the label.
		e.flowWarn.Do(func() {
			e.log.Warn("probe: ipv6 flow label not supported on this host; sending without it",
				"op", req.OpID, "flow_label", fmt.Sprintf("0x%05x", fle.label), "err", fle.err)
		})
		err = nil
	}
	if err != nil {
		s.remove(key, p)
		if errors.Is(err, ErrClosed) || e.isClosed() {
			err = ErrClosed
		}
		return Reply{Outcome: OutcomeError, SentAt: p.sentAt, Err: err}
	}

	select {
	case r := <-p.ch:
		return r
	case <-timer.C():
		if r, ok := s.expire(key, p, e.clk.Now()); ok {
			return r
		}
		return Reply{Outcome: OutcomeTimeout, SentAt: p.sentAt}
	case <-ctx.Done():
		if r, ok := s.expire(key, p, e.clk.Now()); ok {
			return r
		}
		return Reply{Outcome: OutcomeError, SentAt: p.sentAt, Err: ctx.Err()}
	case <-e.done:
		return Reply{Outcome: OutcomeError, SentAt: p.sentAt, Err: ErrClosed}
	}
}

func (e *engine) isClosed() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// identFor returns the ICMP Identifier of opID on s, allocating one on first
// use: OpID&0xFFFF, or the next free value after it. s.mu must be held.
func (s *socket) identFor(opID uint32) (uint16, error) {
	if id, ok := s.idents[opID]; ok {
		return id, nil
	}
	start := uint16(opID)
	for i := range 1 << 16 {
		id := start + uint16(i)
		if _, used := s.owners[id]; !used {
			s.idents[opID] = id
			s.owners[id] = opID
			return id, nil
		}
	}
	return 0, errors.New("no free icmp identifier on the socket")
}

// register reserves the pending-table slot for p. An answered or expired
// entry with the same key (a 16-bit sequence wrap) is replaced. So is an
// Echo of the same operation still in flight: the first attempt of a new
// life reuses Seq, and the canceled attempt of the old life may not have
// left the table yet; it ends with ErrSuperseded.
func (s *socket) register(p *pending) (pendKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	id, err := s.identFor(p.opID)
	if err != nil {
		return pendKey{}, err
	}
	key := pendKey{kind: kindEcho, id: id, seq: uint16(p.seq)}
	if old, ok := s.pending[key]; ok && old.opID == p.opID && (old.state == stateWaiting || old.state == stateUnarmed) {
		old.state = stateAnswered
		select {
		case old.ch <- Reply{Outcome: OutcomeError, SentAt: old.sentAt, Err: ErrSuperseded}:
		default:
		}
		delete(s.pending, key)
	}
	if err := s.insertLocked(key, p); err != nil {
		return pendKey{}, err
	}
	return key, nil
}

// insertLocked puts p under key, unarmed. s.mu must be held.
func (s *socket) insertLocked(key pendKey, p *pending) error {
	if old, ok := s.pending[key]; ok && (old.state == stateWaiting || old.state == stateUnarmed ||
		(old.burst != nil && !old.burst.done)) {
		return fmt.Errorf("op %d seq %d: a request with icmp id %d seq %d is already outstanding",
			p.opID, p.seq, key.id, key.seq)
	}
	p.state = stateUnarmed
	p.expireAt = time.Time{}
	s.pending[key] = p
	return nil
}

// arm makes p eligible for matching.
func (s *socket) arm(key pendKey, p *pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[key] == p {
		p.state = stateWaiting
	}
}

func (s *socket) remove(key pendKey, p *pending) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending[key] == p {
		delete(s.pending, key)
	}
}

// expire marks a waiting p as timed out, keeping it for its grace period to
// recognize late replies. If p was completed concurrently, expire returns
// that completion instead.
func (s *socket) expire(key pendKey, p *pending, now time.Time) (Reply, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if p.state == stateAnswered {
		select {
		case r := <-p.ch:
			return r, true
		default:
		}
	}
	if s.pending[key] == p && p.state == stateWaiting {
		p.state = stateExpired
		p.expireAt = now.Add(p.grace)
	}
	return Reply{}, false
}

// sweep purges answered and expired entries whose grace period is over.
func (s *socket) sweep(now time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, p := range s.pending {
		if p.state != stateWaiting && !p.expireAt.IsZero() && !now.Before(p.expireAt) {
			delete(s.pending, k)
		}
	}
}

func (e *engine) sweepLoop() {
	defer e.wg.Done()
	t := e.clk.NewTimer(sweepInterval)
	defer t.Stop()
	for {
		select {
		case <-e.done:
			return
		case <-t.C():
			e.sweep(e.clk.Now())
			t.Reset(sweepInterval)
		}
	}
}

func (e *engine) sweep(now time.Time) {
	e.mu.Lock()
	socks := make([]*socket, 0, len(e.sockets))
	for _, s := range e.sockets {
		socks = append(socks, s)
	}
	e.mu.Unlock()
	for _, s := range socks {
		s.sweep(now)
	}
}

func (e *engine) recvLoop(s *socket) {
	defer e.wg.Done()
	buf := make([]byte, readBufferSize)
	var (
		failing  uint64    // consecutive failed reads
		lastWarn time.Time // when the failure was last logged at warn
	)
	for {
		n, from, rx, err := s.conn.ReadFrom(buf)
		now := e.clk.Now()
		if err != nil {
			if errors.Is(err, errConnClosed) || e.isClosed() {
				return
			}
			e.stats.recvErrors.Add(1)
			failing++
			// The first failure and then one a minute at warn: every
			// operation on the socket times out while reads fail.
			if failing == 1 || now.Sub(lastWarn) >= recvErrorLogEvery {
				e.log.Warn("probe: receive failed; replies on this socket are lost",
					"vrf", s.key.vrf, "ipv6", s.key.v6, "err", err, "count", failing)
				lastWarn = now
			} else {
				e.log.Debug("probe: receive failed", "vrf", s.key.vrf, "ipv6", s.key.v6, "err", err, "count", failing)
			}
			select {
			case <-e.done:
				return
			case <-e.clk.After(readErrorBackoff):
			}
			continue
		}
		if failing > 0 {
			e.log.Info("probe: receiving again", "vrf", s.key.vrf, "ipv6", s.key.v6, "failed_reads", failing)
			failing = 0
		}
		e.handle(s, buf[:n], from, rx, now)
	}
}

// Stats implements Engine.
func (e *engine) Stats() Stats {
	return Stats{
		Foreign:    e.stats.foreign.Load(),
		Malformed:  e.stats.malformed.Load(),
		Late:       e.stats.late.Load(),
		Other:      e.stats.other.Load(),
		RecvErrors: e.stats.recvErrors.Load(),
	}
}

// Forget implements Engine: it releases the ICMP Identifier and the
// Timestamp sequence counter of opID on every socket. Entries of the
// operation still in a pending table stay until they expire; a reply to one
// cannot complete a request of the operation that later gets the
// Identifier, because the payload's OpID and send time (Echo) or the
// Originate (Timestamp) must match too.
func (e *engine) Forget(opID uint32) {
	e.mu.Lock()
	socks := make([]*socket, 0, len(e.sockets))
	for _, s := range e.sockets {
		socks = append(socks, s)
	}
	e.mu.Unlock()
	for _, s := range socks {
		s.mu.Lock()
		if id, ok := s.idents[opID]; ok {
			delete(s.idents, opID)
			delete(s.owners, id)
		}
		delete(s.tsSeq, opID)
		s.mu.Unlock()
	}
}

// handle matches one received packet against the pending table of s. It
// runs on the receive goroutine and only does bookkeeping.
func (e *engine) handle(s *socket, pkt []byte, from netip.Addr, rx, now time.Time) {
	v6 := s.key.v6
	m, err := parseReply(v6, pkt)
	if err != nil {
		e.stats.malformed.Add(1)
		e.log.Debug("probe: dropped malformed packet", "from", from, "err", err)
		return
	}
	switch m.Kind {
	case parsedEchoReply:
		e.handleEchoReply(s, m, from, rx, now)
	case parsedTimestampReply:
		e.handleTimestampReply(s, m, from, rx, now)
	case parsedDestUnreach, parsedTimeExceeded:
		e.handleError(s, m, from, now)
	default:
		e.stats.other.Add(1)
	}
}

func (e *engine) handleEchoReply(s *socket, m parsedMsg, from netip.Addr, rx, now time.Time) {
	h, ok := parsePayloadHeader(m.Data)
	if !ok {
		e.dropForeign(from, "no payload header")
		return
	}
	key := pendKey{kind: kindEcho, id: m.ID, seq: m.Seq}
	s.mu.Lock()
	p := s.pending[key]
	// The send time in the payload identifies the attempt: an operation that
	// restarts its life reuses Seq, and a late reply to the earlier attempt
	// with the same OpID and Seq must not complete the new one.
	if p == nil || p.opID != h.OpID || p.seq != h.Seq || p.state == stateUnarmed || h.SentNs != p.sentAt.UnixNano() {
		s.mu.Unlock()
		e.dropForeign(from, "no matching request")
		return
	}
	if !sameHost(from, p.target) {
		s.mu.Unlock()
		e.dropForeign(from, "reply from another address")
		return
	}
	rtt := e.rtt(p.sentAt, now, rx, h.OpID, h.Seq)
	if p.state == stateWaiting && !p.sentAt.Add(rtt).Before(p.deadline) {
		// Arrived at or after the timeout, before the waiting Echo noticed:
		// the attempt times out and the reply is late.
		p.state = stateExpired
		p.expireAt = now.Add(p.grace)
	}
	if p.state != stateWaiting {
		s.mu.Unlock()
		e.stats.late.Add(1)
		e.log.Debug("probe: late or duplicate reply", "op", h.OpID, "seq", h.Seq, "from", from)
		if e.onLate != nil {
			e.onLate(h.OpID, h.Seq, p.sentAt)
		}
		return
	}
	r := Reply{Outcome: OutcomeReply, RTT: rtt, SentAt: p.sentAt, ReceivedAt: p.sentAt.Add(rtt)}
	if p.verify && !bytes.Equal(m.Data, p.data) {
		r.Outcome = OutcomeVerifyError
		r.Detail = verifyDetail(m.Data, p.data)
	}
	s.completeLocked(p, r, now)
	s.mu.Unlock()
}

func (e *engine) handleError(s *socket, m parsedMsg, from netip.Addr, now time.Time) {
	kind := kindEcho
	if m.InnerTimestamp {
		kind = kindTimestamp
	}
	key := pendKey{kind: kind, id: m.ID, seq: m.Seq}
	s.mu.Lock()
	p := s.pending[key]
	if p == nil || p.state != stateWaiting || m.InnerDst != p.target {
		s.mu.Unlock()
		e.dropForeign(from, "icmp error for no waiting request")
		return
	}
	// The quoted data may be truncated; check what is present.
	if kind == kindEcho && len(m.Data) >= payloadHeaderLen {
		if h, ok := parsePayloadHeader(m.Data); !ok || h.OpID != p.opID || h.Seq != p.seq || h.SentNs != p.sentAt.UnixNano() {
			s.mu.Unlock()
			e.dropForeign(from, "icmp error quoting another request")
			return
		}
	}
	if kind == kindTimestamp && len(m.Data) >= 4 && binary.BigEndian.Uint32(m.Data) != p.originate {
		s.mu.Unlock()
		e.dropForeign(from, "icmp error quoting another timestamp request")
		return
	}
	detail := m.errorDetail(s.key.v6, from)
	if kind == kindTimestamp {
		s.completeBurstLocked(p, burstEvent{index: p.index, outcome: OutcomeUnreachable, receivedAt: now, detail: detail})
		s.mu.Unlock()
		return
	}
	s.completeLocked(p, Reply{
		Outcome:    OutcomeUnreachable,
		SentAt:     p.sentAt,
		ReceivedAt: now,
		Detail:     detail,
	}, now)
	s.mu.Unlock()
}

// completeLocked hands r to the waiting Echo and keeps p to detect
// duplicates. s.mu must be held.
func (s *socket) completeLocked(p *pending, r Reply, now time.Time) {
	p.state = stateAnswered
	p.expireAt = now.Add(p.grace)
	select {
	case p.ch <- r:
	default:
	}
}

// sameHost reports whether a reply's source is the request's target
// (ignoring the zone of a link-local address, which the reply carries as a
// number).
func sameHost(from, target netip.Addr) bool {
	return from.Unmap().WithZone("") == target.WithZone("")
}

func (e *engine) dropForeign(from netip.Addr, why string) {
	e.stats.foreign.Add(1)
	e.log.Debug("probe: dropped packet", "from", from, "reason", why)
}

// rtt computes the round-trip time from the monotonic send and read times,
// minus the delay between the kernel timestamp rx and the read:
//
//	RTT = (now - sentAt) - (wall(now) - rx)
//
// so that a wall-clock step between send and receive does not distort it.
//
// The correction is a short wall-clock difference. If it is negative or
// larger than maxRxLag, the wall clock stepped between the kernel timestamp
// and the read; the correction is then dropped and the uncorrected
// monotonic RTT is used.
func (e *engine) rtt(sentAt, now, rx time.Time, opID, seq uint32) time.Duration {
	d := now.Sub(sentAt)
	if !rx.IsZero() {
		lag := now.Round(0).Sub(rx)
		if lag < 0 || lag > maxRxLag {
			e.log.Debug("probe: receive timestamp correction dropped (wall clock step?)",
				"op", opID, "seq", seq, "lag_ns", int64(lag))
		} else {
			d -= lag
		}
	}
	if d < 0 {
		e.log.Debug("probe: negative rtt rounded to zero", "op", opID, "seq", seq, "rtt_ns", int64(d))
		d = 0
	}
	return d
}

func verifyDetail(got, want []byte) string {
	if len(got) != len(want) {
		return fmt.Sprintf("data size mismatch: got %d bytes, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			return fmt.Sprintf("data mismatch at offset %d: got 0x%02x, want 0x%02x", i, got[i], want[i])
		}
	}
	return ""
}

// lookupIfIndex resolves an interface name, caching the result for
// ifIndexTTL.
func (e *engine) lookupIfIndex(name string) (int, error) {
	now := e.clk.Now()
	e.ifMu.Lock()
	c, ok := e.ifCache[name]
	e.ifMu.Unlock()
	if ok && now.Sub(c.at) < ifIndexTTL && now.Sub(c.at) >= 0 {
		return c.index, c.err
	}
	idx, err := e.ifIndex(name)
	e.ifMu.Lock()
	e.ifCache[name] = ifCacheEntry{index: idx, err: err, at: now}
	e.ifMu.Unlock()
	return idx, err
}

// zoneIndex resolves an IPv6 zone: a numeric index or an interface name.
func (e *engine) zoneIndex(zone string) (int, error) {
	if n, err := strconv.Atoi(zone); err == nil {
		return n, nil
	}
	return e.lookupIfIndex(zone)
}
