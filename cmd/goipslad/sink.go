package main

// sink.go counts, per event kind, what each event sink delivers and fails,
// for goipsla_events_total.

import (
	"context"

	"goipsla/internal/event"
)

// CountSink wraps a sink so that the provider counts what it delivers and
// fails, by event kind, for goipsla_events_total. Subscribe the wrapped sink.
func (p *provider) CountSink(s event.Sink) event.Sink { return &countingSink{inner: s, p: p} }

type countingSink struct {
	inner event.Sink
	p     *provider
}

func (c *countingSink) Name() string { return c.inner.Name() }

func (c *countingSink) Deliver(ctx context.Context, ev event.Event) error {
	err := c.inner.Deliver(ctx, ev)
	result := "delivered"
	if err != nil {
		result = "failed"
	}
	c.p.countsMu.Lock()
	if c.p.counts == nil {
		c.p.counts = map[sinkCountKey]uint64{}
	}
	c.p.counts[sinkCountKey{string(ev.Kind), c.inner.Name(), result}]++
	c.p.countsMu.Unlock()
	return err
}

// sinkCountKey is the key of the per-sink delivery counts the provider keeps.
//
//declscope:package // the provider keeps the counts and its metrics view reads them
type sinkCountKey struct{ kind, sink, result string }
