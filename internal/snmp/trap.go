//declscope:namespace trap

package snmp

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/gosnmp/gosnmp"

	"goipsla/internal/config"
	"goipsla/internal/event"
	"goipsla/internal/snmp/agentx"
)

// This file sends rttMonNotificationV2 for the threshold reactions whose
// action is trap or trap-and-syslog, as SNMPv2c traps to every
// global.snmp.traps target.

// trapOIDs are the varbind names of rttMonNotificationV2 (without the
// instance) and the trap's own OID.
var trapOIDs = struct {
	sysUpTime, trapOID, notification           agentx.OID
	longTag, historyAddress                    agentx.OID
	reactVar, occurred, value, rising, falling agentx.OID
	echoLSPSelector                            agentx.OID
}{
	sysUpTime:       agentx.ParseOID("1.3.6.1.2.1.1.3.0"),
	trapOID:         agentx.ParseOID("1.3.6.1.6.3.1.1.4.1.0"),
	notification:    mibOID("2.0.8"),       // rttMonNotificationV2
	longTag:         mibOID("1.2.1.1.12"),  // rttMonCtrlAdminLongTag
	historyAddress:  mibOID("1.4.1.1.5"),   // rttMonHistoryCollectionAddress
	reactVar:        mibOID("1.2.19.1.2"),  // rttMonReactVar
	occurred:        mibOID("1.2.19.1.10"), // rttMonReactOccurred
	value:           mibOID("1.2.19.1.9"),  // rttMonReactValue
	rising:          mibOID("1.2.19.1.5"),  // rttMonReactThresholdRising
	falling:         mibOID("1.2.19.1.6"),  // rttMonReactThresholdFalling
	echoLSPSelector: mibOID("1.2.2.1.33"),  // rttMonEchoAdminLSPSelector
}

const trapTimeout = 5 * time.Second

// TrapSink is the event.Sink that sends the traps.
type TrapSink struct {
	targets []config.TrapTarget
	src     Source
	opts    Options
	// send delivers one trap; tests replace it.
	send func(ctx context.Context, target config.TrapTarget, vbs []gosnmp.SnmpPDU) error
}

// NewTrapSink returns the sink for the trap targets of cfg. src resolves the
// reaction row (rttMonReactConfigIndex) of an event.
func NewTrapSink(cfg *config.SNMPConfig, src Source, opts Options) *TrapSink {
	opts = opts.withDefaults()
	s := &TrapSink{targets: cfg.Traps, src: src, opts: opts}
	s.send = s.sendUDP
	return s
}

// Name implements event.Sink.
func (*TrapSink) Name() string { return "snmp-trap" }

// Wants reports whether ev becomes a trap: threshold-* events whose action is
// trap or trap-and-syslog. Tracking changes are not RTTMON notifications.
func (*TrapSink) Wants(ev event.Event) bool {
	return ev.Kind.IsThreshold() && (ev.Action == config.ActionTrap || ev.Action == config.ActionTrapAndSyslog)
}

// Deliver implements event.Sink. The targets are sent to in parallel; a
// failure for one is reported (the Bus logs it), with no retry. When ctx is done,
// Deliver returns at once without waiting for targets that do not answer
// (their sends stop on their own: see sendUDP), so that a stopping Bus is
// not held up by unreachable trap receivers.
func (s *TrapSink) Deliver(ctx context.Context, ev event.Event) error {
	if !s.Wants(ev) || len(s.targets) == 0 {
		return nil
	}
	vbs := s.varbinds(ev)
	type result struct {
		host string
		err  error
	}
	results := make(chan result, len(s.targets)) // buffered: late senders never block
	for _, t := range s.targets {
		go func() { results <- result{t.Host, s.send(ctx, t, vbs)} }()
	}
	var errs []error
	for range s.targets {
		select {
		case r := <-results:
			// The Bus logs the delivery failure (rate-limited, with every
			// host's error joined); here only at debug.
			if r.err != nil {
				s.opts.Logger.Debug("snmp trap failed", "host", r.host, "op", ev.OpID, "err", r.err)
				errs = append(errs, fmt.Errorf("%s: %w", r.host, r.err))
			} else {
				s.opts.Logger.Debug("snmp trap sent", "host", r.host, "op", ev.OpID, "kind", string(ev.Kind))
			}
		case <-ctx.Done():
			return errors.Join(append(errs, fmt.Errorf("snmp trap: %w", ctx.Err()))...)
		}
	}
	return errors.Join(errs...)
}

// configIndex is the reaction row of ev (rttMonReactConfigIndex). The index
// the engine recorded when it raised the event is used; only an event
// without one (0) is looked up in the current rows, falling back to 1.
func (s *TrapSink) configIndex(ev event.Event) uint32 {
	if ev.ReactionIndex > 0 {
		return uint32(ev.ReactionIndex)
	}
	if s.src != nil {
		for i, r := range s.src.Reactions(ev.OpID) {
			if r.Element == ev.Element {
				return uint32(i + 1)
			}
		}
	}
	return 1
}

// varbinds builds sysUpTime.0, snmpTrapOID.0 and the objects of
// rttMonNotificationV2.
func (s *TrapSink) varbinds(ev event.Event) []gosnmp.SnmpPDU {
	id := uint32(ev.OpID)
	ci := s.configIndex(ev)
	occurred := 2 // false
	if ev.Kind == event.ThresholdExceeded {
		occurred = 1
	}
	var addr []byte
	if a, err := trapAddress(ev.Target); err == nil {
		addr = a
	}
	uptime := s.opts.ticks(s.opts.Clock.Now())
	value := ev.Value
	if value < 0 {
		value = 0
	}
	name := func(o agentx.OID) string { return "." + o.String() }
	return []gosnmp.SnmpPDU{
		{Name: name(trapOIDs.sysUpTime), Type: gosnmp.TimeTicks, Value: uptime},
		{Name: name(trapOIDs.trapOID), Type: gosnmp.ObjectIdentifier, Value: name(trapOIDs.notification)},
		{Name: name(trapOIDs.longTag.Join(id)), Type: gosnmp.OctetString, Value: []byte(octetPrefix(ev.Tag, 128))},
		// The history bucket of the attempt is not known to the event; the
		// instance is (id, 1, 1, 1) and the value is the target address.
		{Name: name(trapOIDs.historyAddress.Join(id, 1, 1, 1)), Type: gosnmp.OctetString, Value: addr},
		{Name: name(trapOIDs.reactVar.Join(id, ci)), Type: gosnmp.Integer, Value: int(reactVars[ev.Element])},
		{Name: name(trapOIDs.occurred.Join(id, ci)), Type: gosnmp.Integer, Value: occurred},
		{Name: name(trapOIDs.value.Join(id, ci)), Type: gosnmp.Integer, Value: int(min(value, 2147483647))},
		{Name: name(trapOIDs.rising.Join(id, ci)), Type: gosnmp.Integer, Value: ev.Upper},
		{Name: name(trapOIDs.falling.Join(id, ci)), Type: gosnmp.Integer, Value: ev.Lower},
		{Name: name(trapOIDs.echoLSPSelector.Join(id)), Type: gosnmp.OctetString, Value: []byte{}},
	}
}

// trapAddress is the rttMonHistoryCollectionAddress value of an IP address.
func trapAddress(s string) ([]byte, error) {
	ip := net.ParseIP(s)
	if ip == nil {
		return nil, fmt.Errorf("not an IP address: %q", s)
	}
	if v4 := ip.To4(); v4 != nil {
		return v4, nil
	}
	return ip, nil
}

func (s *TrapSink) sendUDP(ctx context.Context, t config.TrapTarget, vbs []gosnmp.SnmpPDU) error {
	host, port, err := config.ParseTrapTarget(t.Host) // checked at load; kept for a hand-built config
	if err != nil {
		return err
	}
	g := &gosnmp.GoSNMP{
		Target:    host,
		Port:      uint16(port),
		Community: t.Community,
		Version:   gosnmp.Version2c,
		Timeout:   trapTimeout,
		Retries:   0,
		Context:   ctx, // bounds the dial, including name resolution
		// No Logger: gosnmp's debug log prints the community.
	}
	if err := g.Connect(); err != nil {
		return err
	}
	defer g.Conn.Close()
	// Cut a blocked write short when ctx ends.
	stop := context.AfterFunc(ctx, func() { _ = g.Conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()
	_, err = g.SendTrap(gosnmp.SnmpTrap{Variables: vbs})
	return err
}
