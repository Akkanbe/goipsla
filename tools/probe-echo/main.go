// Command probe-echo sends ICMP Echo Requests through internal/probe and
// prints each outcome. It is a development aid for checking the engine in the
// staging environment:
//
//	probe-echo [flags] target [target...]
//	probe-echo --jitter [--interval 20ms] [--num-packets 10] [flags] target [target...]
//
// With --jitter it runs icmp-jitter bursts of ICMP Timestamp Requests
// through op.NewJitter and prints the burst's JitterResult one item per
// line. --interval is then the packet interval and --gap the delay between
// bursts.
//
// It needs CAP_NET_RAW. The exit status is 0 when every attempt got a reply,
// 1 otherwise, and 2 on usage or setup errors.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/netip"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"time"

	"goipsla/internal/clock"
	"goipsla/internal/config"
	"goipsla/internal/op"
	"goipsla/internal/probe"
)

func main() {
	os.Exit(run())
}

// ipLen is the on-wire IP packet length for a request-data-size: IP header,
// ICMP header, 8 bytes that Cisco counts outside request-data-size, and the
// request data (64 bytes for the IPv4 default of 28).
func ipLen(target netip.Addr, size int) int {
	if target.Unmap().Is4() {
		return 20 + 8 + 8 + size
	}
	return 40 + 8 + 8 + size
}

func run() int {
	fs := flag.NewFlagSet("probe-echo", flag.ContinueOnError)
	var (
		source    = fs.String("source", "", "source address")
		iface     = fs.String("interface", "", "outgoing interface name")
		vrf       = fs.String("vrf", "", "VRF device name")
		tos       = fs.String("tos", "0", "IPv4 ToS / IPv6 Traffic Class (decimal or 0x hex)")
		flowLabel = fs.String("flow-label", "0", "IPv6 flow label (decimal or 0x hex)")
		size      = fs.Int("size", probe.MinDataSize, "request-data-size in bytes (Cisco semantics: the IP packet is 36+size bytes for IPv4, 56+size for IPv6)")
		pattern   = fs.String("pattern", "0xABCDABCD", "data-pattern (32-bit hex)")
		verify    = fs.Bool("verify", false, "verify the echoed data")
		timeout   = fs.Duration("timeout", 5*time.Second, "timeout per attempt")
		count     = fs.Int("count", 1, "attempts per target")
		interval  = fs.Duration("interval", time.Second, "delay between attempts to a target; with --jitter, the packet interval (default 20ms)")
		jitter    = fs.Bool("jitter", false, "run icmp-jitter bursts (ICMP Timestamp, IPv4 only)")
		numPkts   = fs.Int("num-packets", 10, "with --jitter, packets per burst")
		threshold = fs.Duration("threshold", 5*time.Second, "with --jitter, threshold for the mean RTT and NumOverThreshold")
		gap       = fs.Duration("gap", time.Second, "with --jitter, delay between bursts to a target")
		packets   = fs.Bool("packets", false, "with --jitter, also print every packet")
		opID      = fs.Uint("op", 1, "operation ID of the first target (incremented per target)")
		debug     = fs.Bool("debug", false, "debug logging")
	)
	fs.Usage = func() {
		fmt.Fprintln(fs.Output(), "usage: probe-echo [flags] target [target...]")
		fs.PrintDefaults()
	}
	if err := fs.Parse(os.Args[1:]); err != nil {
		return 2
	}
	if fs.NArg() == 0 {
		fs.Usage()
		return 2
	}
	if *jitter {
		explicit := false
		fs.Visit(func(f *flag.Flag) { explicit = explicit || f.Name == "interval" })
		if !explicit {
			*interval = 20 * time.Millisecond
		}
	}
	tosV, err1 := strconv.ParseUint(*tos, 0, 8)
	flV, err2 := strconv.ParseUint(*flowLabel, 0, 20)
	patV, err3 := strconv.ParseUint(strings.TrimPrefix(strings.TrimPrefix(*pattern, "0x"), "0X"), 16, 32)
	for _, err := range []error{err1, err2, err3} {
		if err != nil {
			fmt.Fprintln(os.Stderr, "probe-echo:", err)
			return 2
		}
	}
	var src netip.Addr
	if *source != "" {
		var err error
		if src, err = netip.ParseAddr(*source); err != nil {
			fmt.Fprintln(os.Stderr, "probe-echo:", err)
			return 2
		}
	}
	targets := make([]netip.Addr, 0, fs.NArg())
	for _, s := range fs.Args() {
		a, err := netip.ParseAddr(s)
		if err != nil {
			fmt.Fprintln(os.Stderr, "probe-echo:", err)
			return 2
		}
		targets = append(targets, a)
	}

	level := slog.LevelInfo
	if *debug {
		level = slog.LevelDebug
	}
	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	eng, err := probe.New(probe.Options{
		Logger: logger,
		OnLateReply: func(op, seq uint32, sentAt time.Time) {
			fmt.Printf("op=%d seq=%d late or duplicate reply (sent %s)\n", op, seq, sentAt.Format("15:04:05.000"))
		},
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "probe-echo:", err)
		return 2
	}
	defer eng.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	var (
		mu     sync.Mutex
		failed bool
		wg     sync.WaitGroup
	)
	for i, target := range targets {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if *jitter {
				cfg := &config.Operation{
					ID:              int(*opID) + i,
					Type:            config.ICMPJitter,
					Target:          target,
					SourceIP:        src,
					SourceInterface: *iface,
					VRF:             *vrf,
					TOS:             uint8(tosV),
					Timeout:         *timeout,
					Threshold:       *threshold,
					Interval:        *interval,
					NumPackets:      *numPkts,
				}
				ok := runJitter(ctx, cfg, eng, *count, *gap, *packets, &mu)
				if !ok {
					mu.Lock()
					failed = true
					mu.Unlock()
				}
				return
			}
			for seq := 1; seq <= *count; seq++ {
				if seq > 1 {
					select {
					case <-ctx.Done():
						return
					case <-time.After(*interval):
					}
				}
				req := probe.Request{
					OpID:      uint32(*opID) + uint32(i),
					Seq:       uint32(seq),
					Target:    target,
					Source:    src,
					Interface: *iface,
					VRF:       *vrf,
					TOS:       uint8(tosV),
					FlowLabel: uint32(flV),
					DataSize:  *size,
					Pattern:   uint32(patV),
					Verify:    *verify,
					Timeout:   *timeout,
				}
				r := eng.Echo(ctx, req)
				line := fmt.Sprintf("target=%s op=%d seq=%d size=%d ip_len=%d outcome=%s",
					target, req.OpID, seq, *size, ipLen(target, *size), r.Outcome)
				if r.Outcome == probe.OutcomeReply || r.Outcome == probe.OutcomeVerifyError {
					line += fmt.Sprintf(" rtt_ms=%.3f", float64(r.RTT)/float64(time.Millisecond))
				}
				if r.Detail != "" {
					line += fmt.Sprintf(" detail=%q", r.Detail)
				}
				if r.Err != nil {
					line += fmt.Sprintf(" err=%q", r.Err)
				}
				mu.Lock()
				fmt.Println(line)
				if r.Outcome != probe.OutcomeReply {
					failed = true
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if failed {
		return 1
	}
	return 0
}

// capture records the last JitterReply for --packets.
type capture struct {
	probe.Engine
	last probe.JitterReply
}

func (c *capture) Jitter(ctx context.Context, req probe.JitterRequest) probe.JitterReply {
	c.last = c.Engine.Jitter(ctx, req)
	return c.last
}

// runJitter runs count bursts to cfg.Target and prints them. It reports
// whether every burst returned ok.
func runJitter(ctx context.Context, cfg *config.Operation, eng probe.Engine, count int, gap time.Duration, packets bool, mu *sync.Mutex) bool {
	capt := &capture{Engine: eng}
	runner := op.NewJitter(cfg, capt, clock.Real())
	ok := true
	for seq := 1; seq <= count; seq++ {
		if seq > 1 {
			select {
			case <-ctx.Done():
				return ok
			case <-time.After(gap):
			}
		}
		res := runner.Run(ctx, uint32(seq), time.Now())
		var b strings.Builder
		fmt.Fprintf(&b, "target=%s op=%d seq=%d code=%s rtt_ms=%.3f", cfg.Target, cfg.ID, seq, res.Code,
			float64(res.RTT)/float64(time.Millisecond))
		if res.Detail != "" {
			fmt.Fprintf(&b, " detail=%q", res.Detail)
		}
		b.WriteByte('\n')
		if j := res.Jitter; j != nil {
			for _, kv := range jitterLines(j) {
				fmt.Fprintf(&b, "  %s=%s\n", kv[0], kv[1])
			}
		}
		if packets {
			for _, pk := range capt.last.Packets {
				fmt.Fprintf(&b, "  packet %d: outcome=%s", pk.Index, pk.Outcome)
				if pk.Outcome == probe.OutcomeReply {
					fmt.Fprintf(&b, " rtt_ms=%.3f O=%d R=%d T=%d arrival=%d", float64(pk.RTT)/float64(time.Millisecond),
						pk.Originate, pk.Receive, pk.Transmit, pk.ArrivalPos)
				}
				if pk.Late {
					b.WriteString(" late")
				}
				if pk.Detail != "" {
					fmt.Fprintf(&b, " detail=%q", pk.Detail)
				}
				if pk.Err != nil {
					fmt.Fprintf(&b, " err=%q", pk.Err)
				}
				b.WriteByte('\n')
			}
		}
		mu.Lock()
		fmt.Print(b.String())
		mu.Unlock()
		if res.Code != op.RCOK {
			ok = false
		}
	}
	return ok
}

func side(s op.JitterSide) string {
	return fmt.Sprintf("num %d sum %d sum2 %d min/avg/max %d/%.3f/%d", s.Num, s.SumMs, s.Sum2Ms, s.MinMs, s.AvgMs(), s.MaxMs)
}

// jitterLines lists the fields of a JitterResult in display order.
func jitterLines(j *op.JitterResult) [][2]string {
	u := func(v uint64) string { return strconv.FormatUint(v, 10) }
	avg := 0.0
	if j.NumRTT > 0 {
		avg = float64(j.RTTSumMs) / float64(j.NumRTT)
	}
	return [][2]string{
		{"num_packets", strconv.Itoa(j.NumPackets)},
		{"sent", strconv.Itoa(j.Sent)},
		{"skipped", strconv.Itoa(j.Skipped)},
		{"num_rtt", u(j.NumRTT)},
		{"rtt_min_avg_max_ms", fmt.Sprintf("%d/%.3f/%d", j.RTTMinMs, avg, j.RTTMaxMs)},
		{"rtt_sum_ms", u(j.RTTSumMs)},
		{"rtt_sum2_ms", u(j.RTTSum2Ms)},
		{"num_over_threshold", u(j.NumOverThreshold)},
		{"pos_sd", side(j.PosSD)},
		{"neg_sd", side(j.NegSD)},
		{"pos_ds", side(j.PosDS)},
		{"neg_ds", side(j.NegDS)},
		{"avg_jitter_ms", fmt.Sprintf("%.3f", j.AvgJitterMs())},
		{"avg_sd_jitter_ms", fmt.Sprintf("%.3f", j.AvgSDJitterMs())},
		{"avg_ds_jitter_ms", fmt.Sprintf("%.3f", j.AvgDSJitterMs())},
		{"pkt_loss", u(j.PktLoss)},
		{"successive_loss_min_max", fmt.Sprintf("%d/%d", j.MinSucPktLoss, j.MaxSucPktLoss)},
		{"pkt_late_arrival", u(j.PktLateArrival)},
		{"out_of_seq_sd", u(j.PktOutSeqSD)},
		{"out_of_seq_ds", u(j.PktOutSeqDS)},
		{"out_of_seq_both", u(j.PktOutSeqBoth)},
		{"one_way", strconv.FormatBool(j.OneWay)},
		{"num_ow", u(j.NumOW)},
		{"ow_sd", side(j.OWSD)},
		{"ow_ds", side(j.OWDS)},
	}
}
