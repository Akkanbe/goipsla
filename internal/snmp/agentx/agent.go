//declscope:namespace agentx

package agentx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
)

// This file runs the AgentX session: connect, Open, Register, answer the
// master's requests, and reconnect when the connection drops.

// Handler answers the reads of the registered subtrees.
type Handler interface {
	// Get returns the value of an exact instance, or a noSuchObject /
	// noSuchInstance value.
	Get(name OID) Value
	// Next returns the first instance after start (or at start when
	// include), before end when end is not empty.
	Next(start OID, include bool, end OID) (OID, Value, bool)
	// Uptime is sysUpTime for the responses: hundredths of a second since
	// the daemon started.
	Uptime() uint32
}

// agentConfig is what a session needs.
type agentConfig struct {
	network, address string
	subtrees         []OID
	descr            string
	timeout          uint8 // seconds, sent in Open; 0 means the master's default
	retry            time.Duration
}

const (
	defaultRetry   = 10 * time.Second
	requestTimeout = 10 * time.Second
)

// RunSubagent serves h for subtrees through the AgentX master at address
// (net-snmp syntax) until ctx is done. The subagent identifies itself in
// Open with the first subtree and descr. Only a malformed address or no
// subtree is an error: an unreachable master is retried every 10 s.
func RunSubagent(ctx context.Context, address, descr string, subtrees []OID, h Handler, clk clock.Clock, logger *slog.Logger) error {
	if len(subtrees) == 0 {
		return errors.New("agentx: no subtree to register")
	}
	network, addr, err := config.ParseAgentXAddress(address)
	if err != nil {
		return err
	}
	cfg := agentConfig{network: network, address: addr, subtrees: subtrees, descr: descr, retry: defaultRetry}
	return runAgent(ctx, cfg, h, clk, logger)
}

// runAgent keeps a session with the master until ctx is done. It returns
// nil when ctx is done.
//
// Logs: "agentx session open" (info) each time a session is up, with the
// failed attempts and the time without a session before it; "agentx session
// lost" (warn) when an open session ends; "agentx master not reachable"
// (warn) for the first failed attempt and every 30th after it, and
// "agentx registration refused" (error, same rate) when the master answers
// Open or Register with an error, which retrying does not fix.
func runAgent(ctx context.Context, cfg agentConfig, h Handler, clk clock.Clock, logger *slog.Logger) error {
	if cfg.retry <= 0 {
		cfg.retry = defaultRetry
	}
	failures := 0
	down := clk.Now() // since when there is no session
	var up time.Time  // since when the current session is open
	for {
		opened, err := serveSession(ctx, cfg, h, func(session uint32) {
			up = clk.Now()
			logger.Info("agentx session open", "address", cfg.address, "session", session,
				"subtrees", len(cfg.subtrees), "failures", failures, "down_for", up.Sub(down).Round(time.Millisecond).String())
			failures = 0
		})
		if ctx.Err() != nil {
			return nil
		}
		if opened != nil {
			logger.Warn("agentx session lost; reconnecting", "address", cfg.address, "session", opened.id,
				"up_for", clk.Now().Sub(up).Round(time.Millisecond).String(), "err", err, "retry_in", cfg.retry.String())
			down = clk.Now()
		} else if failures == 0 || failures%30 == 0 {
			var me *masterError
			if errors.As(err, &me) {
				logger.Error("agentx registration refused", "address", cfg.address, "err", err, "failures", failures, "retry_in", cfg.retry.String())
			} else {
				logger.Warn("agentx master not reachable; retrying", "address", cfg.address, "err", err, "failures", failures, "retry_in", cfg.retry.String())
			}
		}
		if opened == nil {
			failures++
		}
		t := clk.NewTimer(cfg.retry)
		select {
		case <-ctx.Done():
			t.Stop()
			return nil
		case <-t.C():
		}
	}
}

// masterError is an error answer of the master to our Open or Register.
type masterError struct {
	pdu  string // "open" or "register <subtree>"
	code uint16
}

func (e *masterError) Error() string {
	return fmt.Sprintf("%s: master answered %s (%d)", e.pdu, errName(e.code), e.code)
}

// openedSession is what runAgent logs of a session that was open.
type openedSession struct {
	id uint32
}

// session is one open AgentX session.
type session struct {
	conn    net.Conn
	id      uint32
	packet  uint32
	writeMu sync.Mutex
	uptime  func() uint32
}

func (s *session) write(h header, payload []byte) error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(clock.Real().Now().Add(requestTimeout)); err != nil {
		return err
	}
	_, err := s.conn.Write(packet(h, payload))
	return err
}

// request sends a PDU of ours and reads its response. It is used before
// the serving loop starts, so nothing else reads the connection.
func (s *session) request(typ uint8, payload []byte) (response, error) {
	s.packet++
	if err := s.write(header{typ: typ, session: s.id, packet: s.packet}, payload); err != nil {
		return response{}, err
	}
	if err := s.conn.SetReadDeadline(clock.Real().Now().Add(requestTimeout)); err != nil {
		return response{}, err
	}
	defer func() { _ = s.conn.SetReadDeadline(time.Time{}) }() // best effort: the serving loop sets none
	h, d, err := readPDU(s.conn)
	if err != nil {
		return response{}, err
	}
	if h.typ != pduResponse {
		return response{}, fmt.Errorf("agentx: expected a response, got PDU type %d", h.typ)
	}
	r := d.response()
	if d.err != nil {
		return r, d.err
	}
	if typ == pduOpen {
		s.id = h.session
	}
	if r.err != errNone {
		return r, &masterError{code: r.err}
	}
	return r, nil
}

// serveSession runs one session until it ends. onOpen is called once the
// session is open and registered; the returned openedSession is non-nil if
// it was. The error says why the session ended (nil when ctx is done).
func serveSession(ctx context.Context, cfg agentConfig, h Handler, onOpen func(session uint32)) (*openedSession, error) {
	var dialer net.Dialer
	dctx, cancel := context.WithTimeout(ctx, requestTimeout)
	conn, err := dialer.DialContext(dctx, cfg.network, cfg.address)
	cancel()
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	s := &session{conn: conn, uptime: h.Uptime}

	// On shutdown, also during Open and Register: say goodbye if the
	// session is open, then unblock the reader.
	var open atomic.Bool
	stop := make(chan struct{})
	defer close(stop)
	go func() {
		select {
		case <-ctx.Done():
			if open.Load() {
				s.packet++
				_ = s.write(header{typ: pduClose, session: s.id, packet: s.packet}, closePayload(closeShutdown))
			}
			conn.Close()
		case <-stop:
		}
	}()

	if _, err := s.request(pduOpen, openPayload(cfg.timeout, cfg.subtrees[0], cfg.descr)); err != nil {
		var me *masterError
		if errors.As(err, &me) {
			me.pdu = "open"
			return nil, me
		}
		return nil, fmt.Errorf("open: %w", err)
	}
	for _, t := range cfg.subtrees {
		if _, err := s.request(pduRegister, registerPayload(0, 127, t)); err != nil {
			var me *masterError
			if errors.As(err, &me) {
				me.pdu = "register " + t.String()
				return nil, me
			}
			return nil, fmt.Errorf("register %s: %w", t, err)
		}
	}
	opened := &openedSession{id: s.id}
	open.Store(true)
	onOpen(s.id)

	for {
		hd, d, err := readPDU(conn)
		if err != nil {
			if ctx.Err() != nil {
				return opened, nil
			}
			if errors.Is(err, io.EOF) {
				return opened, errors.New("master closed the connection")
			}
			return opened, err
		}
		if hd.typ == pduClose {
			return opened, errors.New("master closed the session")
		}
		payload := answer(hd, d, h, s.uptime())
		if payload == nil {
			continue
		}
		rh := header{typ: pduResponse, session: hd.session, transaction: hd.transaction, packet: hd.packet}
		if err := s.write(rh, payload); err != nil {
			return opened, err
		}
	}
}

// answer builds the Response payload for a PDU of the master, or nil for
// the PDUs that get no Response: a Response (a late answer to one of ours)
// and CleanupSet (RFC 2741 7.2.4.4). Close ends the session before this.
// goipslad registers in the default context only: a request for another
// context is answered unsupportedContext.
func answer(h header, d *decoder, hd Handler, uptime uint32) []byte {
	switch h.typ {
	case pduResponse, pduCleanupSet:
		return nil
	}
	if h.flags&flagNonDefaultContext != 0 {
		return responsePayload(uptime, errUnsupportedContext, 0, nil)
	}
	switch h.typ {
	case pduGet:
		var vbs []varbind
		for _, r := range d.searchRanges() {
			vbs = append(vbs, varbind{name: r.start, val: hd.Get(r.start)})
		}
		if d.err != nil {
			return responsePayload(uptime, errParse, 0, nil)
		}
		return responsePayload(uptime, errNone, 0, vbs)
	case pduGetNext:
		var vbs []varbind
		for _, r := range d.searchRanges() {
			vbs = append(vbs, nextVarbind(hd, r))
		}
		if d.err != nil {
			return responsePayload(uptime, errParse, 0, nil)
		}
		return responsePayload(uptime, errNone, 0, vbs)
	case pduGetBulk:
		nonRep := int(d.u16())
		maxRep := int(d.u16())
		ranges := d.searchRanges()
		if d.err != nil {
			return responsePayload(uptime, errParse, 0, nil)
		}
		return responsePayload(uptime, errNone, 0, bulk(hd, ranges, nonRep, maxRep))
	case pduTestSet:
		// Read-only MIB: refuse every SET.
		return responsePayload(uptime, errNotWritable, 1, nil)
	case pduCommitSet, pduUndoSet:
		return responsePayload(uptime, errNone, 0, nil)
	case pduPing:
		return responsePayload(uptime, errNone, 0, nil)
	}
	return responsePayload(uptime, errProcessing, 0, nil)
}

func nextVarbind(hd Handler, r searchRange) varbind {
	if name, v, ok := hd.Next(r.start, r.include, r.end); ok {
		return varbind{name: name, val: v}
	}
	return varbind{name: r.start, val: Value{typ: typeEndOfMibView}}
}

// maxBulkVarbinds caps a GetBulk response.
const maxBulkVarbinds = 1000

// bulk answers a GetBulk (RFC 2741 7.2.3.3): the non-repeaters once, then
// max-repetitions rounds over the repeaters.
func bulk(hd Handler, ranges []searchRange, nonRep, maxRep int) []varbind {
	if nonRep > len(ranges) {
		nonRep = len(ranges)
	}
	var vbs []varbind
	for _, r := range ranges[:nonRep] {
		vbs = append(vbs, nextVarbind(hd, r))
	}
	reps := append([]searchRange(nil), ranges[nonRep:]...)
	if len(reps) == 0 {
		return vbs
	}
	for i := 0; i < maxRep && len(vbs) < maxBulkVarbinds; i++ {
		done := true
		for j := range reps {
			if len(vbs) >= maxBulkVarbinds {
				return vbs // the cap counts every varbind, also within a round
			}
			vb := nextVarbind(hd, reps[j])
			vbs = append(vbs, vb)
			if vb.val.typ != typeEndOfMibView {
				reps[j].start, reps[j].include = vb.name, false
				done = false
			}
		}
		if done {
			break
		}
	}
	return vbs
}
