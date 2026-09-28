//declscope:core // Source / Options / Run are the package API itself

// Package snmp publishes the operations of goipslad through SNMP: an AgentX
// subagent (RFC 2741) serves the ICMP parts of CISCO-RTTMON-MIB and
// CISCO-RTTMON-ICMP-MIB read-only through net-snmp's snmpd, and a trap sink
// sends rttMonNotificationV2 as SNMPv2c traps.
//
// # Wiring (cmd/goipslad)
//
// When cfg.Global.SNMP is not nil:
//
//  1. Make the daemon's provider satisfy Source (IDs, Config, State,
//     Snapshot, Reactions).
//
//  2. Start the subagent after the API server:
//
//     g.Go(func() error {
//     return snmp.Run(ctx, cfg.Global.SNMP, src, snmp.Options{Version: version, Start: startedAt, Logger: logger})
//     })
//
//     Run returns nil when ctx is done. It never fails because snmpd is down:
//     it logs a warning and connects again every 10 s.
//
//  3. Subscribe the trap sink on the event bus before bus.Run:
//
//     bus.Subscribe(snmp.NewTrapSink(cfg.Global.SNMP, src, snmp.Options{...}))
//
// Reloads need nothing: the MIB view reads Source again at most once a
// second. The AgentX address and the trap targets are read at start only.
package snmp

import (
	"context"
	"log/slog"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/react"
	"goipsla/internal/snmp/agentx"
	"goipsla/internal/stats"
)

// Source is what the SNMP views read. The daemon's provider implements it
// on top of the manager, the statistics store and the reaction engine.
type Source interface {
	IDs() []int // ascending
	Config(id int) (*config.Operation, bool)
	// State returns pending | inactive | active, the life left and whether
	// the life is forever.
	State(id int) (state string, lifeLeft time.Duration, forever bool, ok bool)
	Snapshot(id int, opts stats.SnapshotOptions) (*stats.Snapshot, bool)
	Reactions(id int) []react.ReactionState
}

// Options are shared by Run and NewTrapSink.
type Options struct {
	Version string       // goipslad version, for rttMonApplVersion
	Start   time.Time    // daemon start: the zero of every TimeStamp / TimeTicks
	Logger  *slog.Logger // nil: slog.Default()
	Clock   clock.Clock  // nil: clock.Real()
}

// withDefaults fills in the nil fields.
//
//declscope:package // the MIB tree and the trap sink take Options
func (o Options) withDefaults() Options {
	if o.Logger == nil {
		o.Logger = slog.Default()
	}
	if o.Clock == nil {
		o.Clock = clock.Real()
	}
	if o.Start.IsZero() {
		o.Start = o.Clock.Now()
	}
	return o
}

// ticks converts a time to hundredths of a second since Start (0 for zero
// or earlier times): the TimeTicks and TimeStamps of the MIB and the
// sysUpTime of the traps.
//
//declscope:package // the MIB tree and the trap sink count time with it
func (o Options) ticks(t time.Time) uint32 {
	if t.IsZero() || t.Before(o.Start) {
		return 0
	}
	return uint32(t.Sub(o.Start) / (10 * time.Millisecond))
}

// Run serves the MIB through the AgentX master at cfg.AgentX until ctx is
// done. It returns an error only for an invalid address.
func Run(ctx context.Context, cfg *config.SNMPConfig, src Source, opts Options) error {
	opts = opts.withDefaults()
	return agentx.RunSubagent(ctx, cfg.AgentX, "goipslad "+opts.Version, []agentx.OID{rttMonMIB}, newMIB(src, opts), opts.Clock, opts.Logger)
}
