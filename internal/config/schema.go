//declscope:namespace parse

package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"

	"gopkg.in/yaml.v3"
)

// This file turns the YAML node tree into the parsed* structures. Every field is a
// pointer so that "not written" can be told apart from the zero value, which
// template expansion needs. Syntax, types and single-value ranges are checked
// here, at the path where the value is written; checks that depend on the
// effective (expanded) operation live in validate.go.

type parsedFile struct {
	global     Global
	templates  map[string]*parsedOp
	operations []*parsedOp
	tracks     []parsedTrack
	actions    []parsedAction
}

type parsedTarget struct {
	path string // e.g. "operations[1].targets.101"
	id   int
	addr netip.Addr
	name string
	ok   bool // addr is valid
}

type parsedOp struct {
	path string // "operations[3]" or "templates.wan-echo"

	id         *int
	idFailed   bool
	template   *string
	target     *parsedTarget
	targets    []parsedTarget
	hasTargets bool

	opType *OpType

	sourceIP        *netip.Addr
	sourceInterface *string
	vrf             *string
	tos             *uint8
	trafficClass    *uint8
	flowLabel       *uint32

	frequency       *time.Duration
	timeout         *time.Duration
	threshold       *time.Duration
	requestDataSize *int
	dataPattern     *uint32
	verifyData      *bool
	tag             *string
	owner           *string

	interval    *time.Duration
	numPackets  *int
	oneWayDelay *bool

	history  parsedHistory
	schedule *parsedSchedule
	react    []parsedReact
	reactSet bool

	failed map[string]bool // keys whose value failed to decode
}

type parsedHistory struct {
	livesKept    *int
	bucketsKept  *int
	filter       *HistoryFilter
	hoursKept    *int
	distKept     *int
	distInterval *time.Duration
	enhanced     *parsedEnhanced
}

type parsedEnhanced struct {
	interval *time.Duration
	buckets  *int
}

type parsedStart struct {
	kind  StartKind
	after time.Duration
	at    time.Time
	daily bool // written as HH:MM[:SS]
}

type parsedSchedule struct {
	life      *time.Duration
	forever   *bool
	start     *parsedStart
	ageout    *time.Duration
	recurring *bool
}

type parsedReact struct {
	element       *string
	thresholdType *string
	count         *int
	x, y          *int
	upper, lower  *int
	action        *string
}

type parsedTrack struct {
	path      string
	id        *int
	operation *int
	mode      string
	delayUp   time.Duration
	delayDown time.Duration
}

type parsedAction struct {
	path    string
	on      []string
	track   *int
	webhook string
	exec    string
}

// parser collects errors while walking the node tree.
type parser struct {
	errs   []FieldError
	warns  []string        // Config.Warnings
	warned map[string]bool // de-duplicates warns
	now    time.Time       // for start-time HH:MM
}

// warn records a setting that is accepted but has no effect, once.
func (d *parser) warn(path, format string, args ...any) {
	w := FieldError{Path: path, Msg: fmt.Sprintf(format, args...)}.String()
	if d.warned[w] {
		return
	}
	if d.warned == nil {
		d.warned = make(map[string]bool)
	}
	d.warned[w] = true
	d.warns = append(d.warns, w)
}

func (d *parser) add(path, format string, args ...any) {
	d.errs = append(d.errs, FieldError{Path: path, Msg: fmt.Sprintf(format, args...)})
}

// mapping calls f for each key of the mapping n. f returns false for an
// unknown key. It returns false if n is not a mapping.
func (d *parser) mapping(n *yaml.Node, path string, f func(key string, v *yaml.Node, p string) bool) bool {
	n = resolveNode(n)
	if n == nil || n.Kind != yaml.MappingNode {
		d.add(path, "must be a mapping")
		return false
	}
	seen := make(map[string]bool)
	for i := 0; i+1 < len(n.Content); i += 2 {
		kn := resolveNode(n.Content[i])
		if kn == nil || kn.Kind != yaml.ScalarNode {
			d.add(path, "keys must be scalars")
			continue
		}
		k := kn.Value
		p := joinPath(path, k)
		if seen[k] {
			d.add(p, "duplicate key")
			continue
		}
		seen[k] = true
		if !f(k, n.Content[i+1], p) {
			d.add(p, "unknown key")
		}
	}
	return true
}

// sequence calls f for each item of the sequence n.
func (d *parser) sequence(n *yaml.Node, path string, f func(i int, v *yaml.Node, p string)) bool {
	n = resolveNode(n)
	if n == nil || n.Kind != yaml.SequenceNode {
		d.add(path, "must be a list")
		return false
	}
	for i, v := range n.Content {
		f(i, v, indexPath(path, i))
	}
	return true
}

func (d *parser) scalar(n *yaml.Node, path string) (*yaml.Node, bool) {
	n = resolveNode(n)
	if isNullNode(n) {
		d.add(path, "missing value")
		return nil, false
	}
	if n.Kind != yaml.ScalarNode {
		d.add(path, "must be a single value")
		return nil, false
	}
	return n, true
}

func (d *parser) str(n *yaml.Node, path string) (string, bool) {
	s, ok := d.scalar(n, path)
	if !ok {
		return "", false
	}
	return s.Value, true
}

func (d *parser) nonEmpty(n *yaml.Node, path string) (string, bool) {
	s, ok := d.str(n, path)
	if ok && s == "" {
		d.add(path, "must not be empty")
		return "", false
	}
	return s, ok
}

func (d *parser) maxLen(n *yaml.Node, path string, limit int) (string, bool) {
	s, ok := d.str(n, path)
	if ok && len([]rune(s)) > limit {
		d.add(path, "must be at most %d characters", limit)
		return "", false
	}
	return s, ok
}

// label decodes a one-line text of at most limit characters. Control
// characters (U+0000..U+001F, U+007F) are rejected: tag and owner reach
// syslog lines, exec environment variables and SNMP DisplayStrings, where a
// line break would forge a new line.
func (d *parser) label(n *yaml.Node, path string, limit int) (string, bool) {
	s, ok := d.maxLen(n, path, limit)
	if !ok {
		return "", false
	}
	for _, r := range s {
		if r < 0x20 || r == 0x7F {
			d.add(path, "must not contain control characters (found %U)", r)
			return "", false
		}
	}
	return s, true
}

// ifName decodes a Linux interface name (source-interface, and vrf, which
// names a VRF device): 1..15 bytes (IFNAMSIZ - 1) without "/", white space
// or control characters.
func (d *parser) ifName(n *yaml.Node, path string) (string, bool) {
	s, ok := d.nonEmpty(n, path)
	if !ok {
		return "", false
	}
	if len(s) > 15 {
		d.add(path, "must be at most 15 bytes (a Linux interface name)")
		return "", false
	}
	for _, r := range s {
		if r <= 0x20 || r == 0x7F || r == '/' || unicode.IsSpace(r) {
			d.add(path, "must not contain \"/\", white space or control characters")
			return "", false
		}
	}
	return s, true
}

func (d *parser) boolean(n *yaml.Node, path string) (bool, bool) {
	s, ok := d.scalar(n, path)
	if !ok {
		return false, false
	}
	var b bool
	if s.Tag != "!!bool" || s.Decode(&b) != nil {
		d.add(path, "must be true or false")
		return false, false
	}
	return b, true
}

func (d *parser) integer(n *yaml.Node, path string, l intLimit) (int64, bool) {
	s, ok := d.scalar(n, path)
	if !ok {
		return 0, false
	}
	var v int64
	if s.Tag != "!!int" || s.Decode(&v) != nil {
		d.add(path, "must be an integer")
		return 0, false
	}
	if v < l.min || v > l.max {
		d.add(path, "must be between %d and %d", l.min, l.max)
		return 0, false
	}
	return v, true
}

func (d *parser) enum(n *yaml.Node, path string, allowed ...string) (string, bool) {
	s, ok := d.str(n, path)
	if !ok {
		return "", false
	}
	for _, a := range allowed {
		if s == a {
			return s, true
		}
	}
	d.add(path, "must be one of %s", strings.Join(allowed, ", "))
	return "", false
}

// parseDuration parses a duration that must carry a unit.
func parseDuration(s string) (time.Duration, string) {
	if digitsPattern.MatchString(strings.TrimSpace(s)) {
		return 0, "duration needs a unit such as 60s or 5000ms"
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return 0, "must be a duration such as 60s or 5000ms"
	}
	return v, ""
}

// duration decodes a duration within l: in [l.min, l.max] and a whole
// multiple of l.unit.
func (d *parser) duration(n *yaml.Node, path string, l durationLimit) (time.Duration, bool) {
	s, ok := d.scalar(n, path)
	if !ok {
		return 0, false
	}
	if s.Tag == "!!int" || s.Tag == "!!float" {
		d.add(path, "duration needs a unit such as 60s or 5000ms")
		return 0, false
	}
	v, msg := parseDuration(s.Value)
	if msg != "" {
		d.add(path, "%s", msg)
		return 0, false
	}
	return d.checkDuration(v, path, l)
}

func (d *parser) checkDuration(v time.Duration, path string, l durationLimit) (time.Duration, bool) {
	if v%l.unit != 0 {
		name := "seconds"
		if l.unit == time.Millisecond {
			name = "milliseconds"
		}
		d.add(path, "must be a whole number of %s", name)
		return 0, false
	}
	if v < l.min || v > l.max {
		d.add(path, "must be between %s and %s", l.format(l.min), l.format(l.max))
		return 0, false
	}
	return v, true
}

// parseAddr parses an IP literal. IPv4-mapped IPv6 addresses become IPv4.
func parseAddr(s string) (netip.Addr, string) {
	a, err := netip.ParseAddr(s)
	if err != nil {
		if hostnamePattern.MatchString(s) && strings.ContainsAny(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ-") {
			return netip.Addr{}, "hostnames are not supported yet"
		}
		return netip.Addr{}, "must be an IP address"
	}
	if a.Zone() != "" {
		return netip.Addr{}, "zoned addresses are not supported; use source-interface"
	}
	a = a.Unmap()
	if a.IsUnspecified() {
		return netip.Addr{}, "must not be the unspecified address"
	}
	return a, ""
}

func (d *parser) addr(n *yaml.Node, path string) (netip.Addr, string, bool) {
	s, ok := d.str(n, path)
	if !ok {
		return netip.Addr{}, "", false
	}
	a, msg := parseAddr(s)
	if msg != "" {
		d.add(path, "%s", msg)
		return netip.Addr{}, s, false
	}
	return a, s, true
}

func parsedPtr[T any](v T) *T { return &v }

// ---- top level ----

func (d *parser) file(root *yaml.Node) *parsedFile {
	f := &parsedFile{global: defaultGlobal(), templates: make(map[string]*parsedOp)}
	if isNullNode(resolveNode(root)) {
		return f
	}
	if n := resolveNode(root); n.Kind != yaml.MappingNode {
		d.add("", "the file must be a mapping (global, templates, operations, tracks, actions)")
		return f
	}
	var tmplNode, opsNode, tracksNode, actionsNode *yaml.Node
	d.mapping(root, "", func(k string, v *yaml.Node, p string) bool {
		switch k {
		case "global":
			d.global(v, p, &f.global)
		case "templates":
			tmplNode = v
		case "operations":
			opsNode = v
		case "tracks":
			tracksNode = v
		case "actions":
			actionsNode = v
		default:
			return false
		}
		return true
	})
	if tmplNode != nil && !isNullNode(resolveNode(tmplNode)) {
		d.mapping(tmplNode, "templates", func(k string, v *yaml.Node, p string) bool {
			if k == "" {
				d.add(p, "template name must not be empty")
				return true
			}
			f.templates[k] = d.op(v, p, true)
			return true
		})
	}
	if opsNode != nil && !isNullNode(resolveNode(opsNode)) {
		d.sequence(opsNode, "operations", func(_ int, v *yaml.Node, p string) {
			f.operations = append(f.operations, d.op(v, p, false))
		})
	}
	if tracksNode != nil && !isNullNode(resolveNode(tracksNode)) {
		d.sequence(tracksNode, "tracks", func(_ int, v *yaml.Node, p string) {
			f.tracks = append(f.tracks, d.track(v, p))
		})
	}
	if actionsNode != nil && !isNullNode(resolveNode(actionsNode)) {
		d.sequence(actionsNode, "actions", func(_ int, v *yaml.Node, p string) {
			f.actions = append(f.actions, d.action(v, p))
		})
	}
	return f
}

func (d *parser) global(n *yaml.Node, path string, g *Global) {
	if isNullNode(resolveNode(n)) {
		return
	}
	d.mapping(n, path, func(k string, v *yaml.Node, p string) bool {
		switch k {
		case "api-socket":
			if s, ok := d.nonEmpty(v, p); ok {
				switch {
				case !filepath.IsAbs(s):
					d.add(p, "must be an absolute path")
				case len(s) > 107:
					d.add(p, "must be at most 107 bytes (the limit of a Unix socket path)")
				default:
					g.APISocket = s
				}
			}
		case "api-socket-group":
			if s, ok := d.str(v, p); ok {
				if s != "" && !groupPattern.MatchString(s) {
					d.add(p, "must be a group name or a numeric gid")
				} else {
					g.APISocketGroup = s
				}
			}
		case "metrics-listen":
			if s, ok := d.str(v, p); ok {
				if s != "" {
					if msg := parseListen(s); msg != "" {
						d.add(p, "%s", msg)
						return true
					}
				}
				g.MetricsListen = s
			}
		case "log":
			d.mapping(v, p, func(k string, v *yaml.Node, p string) bool {
				switch k {
				case "format":
					if s, ok := d.enum(v, p, "text", "json"); ok {
						g.Log.Format = s
					}
				case "level":
					if s, ok := d.enum(v, p, "debug", "info", "warn", "error"); ok {
						g.Log.Level = s
					}
				default:
					return false
				}
				return true
			})
		case "syslog":
			sc := &SyslogConfig{Facility: defaultSyslogFacility}
			if d.mapping(v, p, func(k string, v *yaml.Node, p string) bool {
				if k != "facility" {
					return false
				}
				if s, ok := d.enum(v, p, syslogFacilities...); ok {
					sc.Facility = s
				}
				return true
			}) {
				g.Syslog = sc
			}
		case "snmp":
			if sc := d.snmp(v, p); sc != nil {
				g.SNMP = sc
			}
		default:
			return false
		}
		return true
	})
}

// snmp decodes global.snmp; nil when it is not a mapping (reported).
func (d *parser) snmp(n *yaml.Node, path string) *SNMPConfig {
	sc := &SNMPConfig{AgentX: defaultAgentX}
	if d.mapping(n, path, func(k string, v *yaml.Node, p string) bool {
		switch k {
		case "agentx":
			if s, ok := d.nonEmpty(v, p); ok {
				if _, _, err := ParseAgentXAddress(s); err != nil {
					d.add(p, "must be /path, unix:/path, tcp:host:port or host:port")
				} else {
					sc.AgentX = s
				}
			}
		case "traps":
			d.sequence(v, p, func(_ int, v *yaml.Node, p string) {
				var t TrapTarget
				d.mapping(v, p, func(k string, v *yaml.Node, p string) bool {
					switch k {
					case "host":
						if s, ok := d.nonEmpty(v, p); ok {
							if _, _, err := ParseTrapTarget(s); err != nil {
								d.add(p, "%s", err)
							} else {
								t.Host = s
							}
						}
					case "community":
						if s, ok := d.nonEmpty(v, p); ok {
							t.Community = s
						}
					case "version":
						// SNMPv2c only for now; the key exists so that
						// a later v3 does not change the file format.
						d.enum(v, p, "v2c")
					default:
						return false
					}
					return true
				})
				if t.Host == "" && !d.hasErrorAt(joinPath(p, "host")) {
					d.add(joinPath(p, "host"), "required")
				}
				if t.Community == "" && !d.hasErrorAt(joinPath(p, "community")) {
					d.add(joinPath(p, "community"), "required")
				}
				sc.Traps = append(sc.Traps, t)
			})
		default:
			return false
		}
		return true
	}) {
		return sc
	}
	return nil
}

func (d *parser) hasErrorAt(path string) bool {
	for _, e := range d.errs {
		if e.Path == path {
			return true
		}
	}
	return false
}

func parseListen(s string) string {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return "must be host:port"
	}
	if host != "" && !parsedHost(host) {
		return "host must be an IP address or a host name"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "port must be between 1 and 65535"
	}
	return ""
}

// ---- operations and templates ----

func (d *parser) op(n *yaml.Node, path string, inTemplate bool) *parsedOp {
	o := &parsedOp{path: path, failed: make(map[string]bool)}
	// fail marks key as failed when an error was added while decoding it.
	fail := func(key string, mark int) {
		if len(d.errs) > mark {
			o.failed[key] = true
		}
	}
	d.mapping(n, path, func(k string, v *yaml.Node, p string) bool {
		mark := len(d.errs)
		defer fail(k, mark)
		switch k {
		case "id", "target", "targets", "template":
			if inTemplate {
				d.add(p, "not allowed in a template")
				return true
			}
		}
		switch k {
		case "id":
			if x, ok := d.integer(v, p, idLimit); ok {
				o.id = parsedPtr(int(x))
			} else {
				o.idFailed = true
			}
		case "template":
			if s, ok := d.nonEmpty(v, p); ok {
				o.template = &s
			}
		case "target":
			a, s, ok := d.addr(v, p)
			o.target = &parsedTarget{path: p, addr: a, name: s, ok: ok}
		case "targets":
			o.hasTargets = true
			d.targets(v, p, o)
		case "type":
			if s, ok := d.enum(v, p, string(ICMPEcho), string(ICMPJitter)); ok {
				o.opType = parsedPtr(OpType(s))
			}
		case "source-ip":
			if a, _, ok := d.addr(v, p); ok {
				o.sourceIP = &a
			}
		case "source-interface":
			if s, ok := d.ifName(v, p); ok {
				o.sourceInterface = &s
			}
		case "vrf":
			if s, ok := d.ifName(v, p); ok {
				o.vrf = &s
			}
		case "tos":
			if x, ok := d.integer(v, p, dsByteLimit); ok {
				o.tos = parsedPtr(uint8(x))
			}
		case "traffic-class":
			if x, ok := d.integer(v, p, dsByteLimit); ok {
				o.trafficClass = parsedPtr(uint8(x))
			}
		case "flow-label":
			if x, ok := d.integer(v, p, flowLabelLimit); ok {
				o.flowLabel = parsedPtr(uint32(x))
			}
		case "frequency":
			if x, ok := d.duration(v, p, frequencyLimit); ok {
				o.frequency = &x
			}
		case "timeout":
			// 0 is inside the MIB range but meaningless for a measurement
			// (every reply would be a late reply), so the lower bound is 1ms.
			if x, ok := d.duration(v, p, unboundedMillisLimit); ok {
				switch {
				case x < timeoutLimit.min:
					d.add(p, "must be at least %s", timeoutLimit.format(timeoutLimit.min))
				case x > timeoutLimit.max:
					d.add(p, "must be between %s and %s", timeoutLimit.format(timeoutLimit.min), timeoutLimit.format(timeoutLimit.max))
				default:
					o.timeout = &x
				}
			}
		case "threshold":
			if x, ok := d.duration(v, p, thresholdLimit); ok {
				o.threshold = &x
			}
		case "request-data-size":
			if x, ok := d.integer(v, p, dataSizeLimit); ok {
				o.requestDataSize = parsedPtr(int(x))
			}
		case "data-pattern":
			if x, ok := d.integer(v, p, dataPatternLimit); ok {
				o.dataPattern = parsedPtr(uint32(x))
			}
		case "verify-data":
			if b, ok := d.boolean(v, p); ok {
				o.verifyData = &b
			}
		case "tag":
			if s, ok := d.label(v, p, tagLengthLimit); ok {
				o.tag = &s
			}
		case "owner":
			if s, ok := d.label(v, p, ownerLengthLimit); ok {
				o.owner = &s
			}
		case "interval":
			if x, ok := d.duration(v, p, intervalLimit); ok {
				o.interval = &x
			}
		case "one-way-delay":
			if b, ok := d.boolean(v, p); ok {
				o.oneWayDelay = &b
			}
		case "num-packets":
			if x, ok := d.integer(v, p, numPacketsLimit); ok {
				o.numPackets = parsedPtr(int(x))
			}
		case "history":
			d.history(v, p, &o.history)
		case "schedule":
			o.schedule = d.schedule(v, p)
		case "react":
			o.reactSet = true
			if isNullNode(resolveNode(v)) {
				return true // "react:" with no items clears the template's list
			}
			d.sequence(v, p, func(_ int, v *yaml.Node, p string) {
				o.react = append(o.react, d.react(v, p))
			})
		default:
			return false
		}
		return true
	})
	return o
}

func (d *parser) targets(n *yaml.Node, path string, o *parsedOp) {
	n = resolveNode(n)
	if n == nil || n.Kind != yaml.MappingNode {
		d.add(path, "must be a mapping of id to target address")
		return
	}
	if len(n.Content) == 0 {
		d.add(path, "must not be empty")
		return
	}
	seen := make(map[int]bool)
	for i := 0; i+1 < len(n.Content); i += 2 {
		kn := resolveNode(n.Content[i])
		var key string
		if kn != nil && kn.Kind == yaml.ScalarNode {
			key = kn.Value
		}
		p := joinPath(path, key)
		id, ok := d.integer(kn, p, idLimit)
		if !ok {
			continue
		}
		if seen[int(id)] {
			d.add(p, "duplicate key")
			continue
		}
		seen[int(id)] = true
		a, s, ok := d.addr(n.Content[i+1], p)
		o.targets = append(o.targets, parsedTarget{path: p, id: int(id), addr: a, name: s, ok: ok})
	}
}

func (d *parser) history(n *yaml.Node, path string, h *parsedHistory) {
	d.mapping(n, path, func(k string, v *yaml.Node, p string) bool {
		switch k {
		case "lives-kept":
			if x, ok := d.integer(v, p, livesKeptLimit); ok {
				h.livesKept = parsedPtr(int(x))
			}
		case "buckets-kept":
			if x, ok := d.integer(v, p, bucketsKeptLimit); ok {
				h.bucketsKept = parsedPtr(int(x))
			}
		case "filter":
			if s, ok := d.enum(v, p, string(FilterNone), string(FilterAll), string(FilterOverThreshold), string(FilterFailures)); ok {
				h.filter = parsedPtr(HistoryFilter(s))
			}
		case "hours-of-statistics-kept":
			if x, ok := d.integer(v, p, hoursKeptLimit); ok {
				h.hoursKept = parsedPtr(int(x))
			}
		case "distributions-of-statistics-kept":
			if x, ok := d.integer(v, p, distBucketsLimit); ok {
				h.distKept = parsedPtr(int(x))
			}
		case "statistics-distribution-interval":
			if x, ok := d.duration(v, p, distIntervalLimit); ok {
				h.distInterval = &x
			}
		case "enhanced":
			e := &parsedEnhanced{}
			if d.mapping(v, p, func(k string, v *yaml.Node, p string) bool {
				switch k {
				case "interval":
					if x, ok := d.duration(v, p, enhancedIntervalLimit); ok {
						e.interval = &x
					}
				case "buckets":
					if x, ok := d.integer(v, p, enhancedBucketsLimit); ok {
						e.buckets = parsedPtr(int(x))
					}
				default:
					return false
				}
				return true
			}) {
				h.enhanced = e
			}
		default:
			return false
		}
		return true
	})
}

func (d *parser) schedule(n *yaml.Node, path string) *parsedSchedule {
	s := &parsedSchedule{}
	d.mapping(n, path, func(k string, v *yaml.Node, p string) bool {
		switch k {
		case "life":
			if sn, ok := d.scalar(v, p); ok && sn.Value == "forever" {
				s.forever = parsedPtr(true)
			} else if ok {
				if x, ok := d.duration(sn, p, lifeLimit); ok {
					s.life = &x
					s.forever = parsedPtr(false)
				}
			}
		case "start-time":
			if str, ok := d.str(v, p); ok {
				st, msg := parseStart(str, d.now)
				if msg != "" {
					d.add(p, "%s", msg)
				} else {
					s.start = st
				}
			}
		case "ageout":
			if x, ok := d.duration(v, p, ageoutLimit); ok {
				s.ageout = &x
			}
		case "recurring":
			if b, ok := d.boolean(v, p); ok {
				s.recurring = &b
			}
		default:
			return false
		}
		return true
	})
	return s
}

// parseClock parses HH:MM[:SS] into hours, minutes and seconds.
func parseClock(s string) (h, m, sec int, ok bool) {
	g := clockPattern.FindStringSubmatch(s)
	if g == nil {
		return 0, 0, 0, false
	}
	h, _ = strconv.Atoi(g[1])
	m, _ = strconv.Atoi(g[2])
	if g[3] != "" {
		sec, _ = strconv.Atoi(g[3])
	}
	if h > 23 || m > 59 || sec > 59 {
		return 0, 0, 0, false
	}
	return h, m, sec, true
}

const startParseError = "must be now, pending, after <duration>, HH:MM[:SS] or an RFC 3339 time"

// parseStart parses start-time. HH:MM[:SS] resolves to the next time the
// local clock shows that time, as seen from now.
func parseStart(s string, now time.Time) (*parsedStart, string) {
	s = strings.TrimSpace(s)
	switch {
	case s == "now":
		return &parsedStart{kind: StartNow}, ""
	case s == "pending":
		return &parsedStart{kind: StartPending}, ""
	case strings.HasPrefix(s, "after "):
		arg := strings.TrimSpace(strings.TrimPrefix(s, "after "))
		var v time.Duration
		if h, m, sec, ok := parseHMS(arg); ok {
			v = time.Duration(h)*time.Hour + time.Duration(m)*time.Minute + time.Duration(sec)*time.Second
		} else {
			var msg string
			if v, msg = parseDuration(arg); msg != "" {
				return nil, "after " + msg
			}
		}
		if v < 0 || v%time.Second != 0 || v > lifeLimit.max {
			return nil, "after must be a whole number of seconds between 0s and 2147483647s"
		}
		return &parsedStart{kind: StartAfter, after: v}, ""
	}
	if h, m, sec, ok := parseClock(s); ok {
		at := time.Date(now.Year(), now.Month(), now.Day(), h, m, sec, 0, now.Location())
		if !at.After(now) {
			at = time.Date(now.Year(), now.Month(), now.Day()+1, h, m, sec, 0, now.Location())
		}
		return &parsedStart{kind: StartAt, at: at, daily: true}, ""
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return &parsedStart{kind: StartAt, at: t}, ""
	}
	return nil, startParseError
}

// parseHMS parses Cisco's "after hh:mm:ss" (hours may exceed 23).
func parseHMS(s string) (h, m, sec int, ok bool) {
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, 0, 0, false
	}
	var v [3]int
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil || n < 0 || len(p) > 6 {
			return 0, 0, 0, false
		}
		v[i] = n
	}
	if v[1] > 59 || v[2] > 59 {
		return 0, 0, 0, false
	}
	return v[0], v[1], v[2], true
}

func (d *parser) react(n *yaml.Node, path string) parsedReact {
	var r parsedReact
	d.mapping(n, path, func(k string, v *yaml.Node, p string) bool {
		switch k {
		case "element":
			if s, ok := d.nonEmpty(v, p); ok {
				r.element = &s
			}
		case "threshold-type":
			if s, ok := d.enum(v, p, ThresholdNever, ThresholdImmediate, ThresholdConsecutive, ThresholdXofY, ThresholdAverage); ok {
				r.thresholdType = &s
			}
		case "count":
			if x, ok := d.integer(v, p, thresholdCountLimit); ok {
				r.count = parsedPtr(int(x))
			}
		case "x":
			if x, ok := d.integer(v, p, thresholdCountLimit); ok {
				r.x = parsedPtr(int(x))
			}
		case "y":
			if x, ok := d.integer(v, p, thresholdCountLimit); ok {
				r.y = parsedPtr(int(x))
			}
		case "upper":
			if x, ok := d.integer(v, p, thresholdValueLimit); ok {
				r.upper = parsedPtr(int(x))
			}
		case "lower":
			if x, ok := d.integer(v, p, thresholdValueLimit); ok {
				r.lower = parsedPtr(int(x))
			}
		case "action":
			if s, ok := d.enum(v, p, actionNone, ActionSyslog, ActionTrap, ActionTrapAndSyslog); ok {
				r.action = &s
			}
		default:
			return false
		}
		return true
	})
	if r.element == nil && !d.hasErrorAt(joinPath(path, "element")) {
		d.add(joinPath(path, "element"), "required")
	}
	return r
}

// ---- tracks and actions ----

func (d *parser) track(n *yaml.Node, path string) parsedTrack {
	t := parsedTrack{path: path, mode: TrackState}
	d.mapping(n, path, func(k string, v *yaml.Node, p string) bool {
		switch k {
		case "id":
			if x, ok := d.integer(v, p, idLimit); ok {
				t.id = parsedPtr(int(x))
			}
		case "operation":
			if x, ok := d.integer(v, p, idLimit); ok {
				t.operation = parsedPtr(int(x))
			}
		case "mode":
			if s, ok := d.enum(v, p, TrackState, TrackReachability); ok {
				t.mode = s
			}
		case "delay":
			d.mapping(v, p, func(k string, v *yaml.Node, p string) bool {
				switch k {
				case "up":
					if x, ok := d.duration(v, p, trackDelayLimit); ok {
						t.delayUp = x
					}
				case "down":
					if x, ok := d.duration(v, p, trackDelayLimit); ok {
						t.delayDown = x
					}
				default:
					return false
				}
				return true
			})
		default:
			return false
		}
		return true
	})
	if t.id == nil && !d.hasErrorAt(joinPath(path, "id")) {
		d.add(joinPath(path, "id"), "required")
	}
	if t.operation == nil && !d.hasErrorAt(joinPath(path, "operation")) {
		d.add(joinPath(path, "operation"), "required")
	}
	return t
}

func (d *parser) action(n *yaml.Node, path string) parsedAction {
	a := parsedAction{path: path}
	onSet := false
	d.mapping(n, path, func(k string, v *yaml.Node, p string) bool {
		switch k {
		case "on":
			onSet = true
			seen := make(map[string]bool)
			d.sequence(v, p, func(_ int, v *yaml.Node, p string) {
				s, ok := d.enum(v, p, actionEvents...)
				if !ok {
					return
				}
				if seen[s] {
					d.add(p, "duplicate event %s", s)
					return
				}
				seen[s] = true
				a.on = append(a.on, s)
			})
			if len(a.on) == 0 && !d.hasErrorPrefix(p) {
				d.add(p, "must list at least one event")
			}
		case "track":
			if x, ok := d.integer(v, p, idLimit); ok {
				a.track = parsedPtr(int(x))
			}
		case "webhook":
			if s, ok := d.nonEmpty(v, p); ok {
				u, err := url.Parse(s)
				if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
					d.add(p, "must be an http or https URL")
				} else {
					a.webhook = s
				}
			}
		case "exec":
			if s, ok := d.nonEmpty(v, p); ok {
				if !filepath.IsAbs(s) {
					d.add(p, "must be an absolute path")
				} else {
					a.exec = s
				}
			}
		default:
			return false
		}
		return true
	})
	if !onSet {
		d.add(joinPath(path, "on"), "required")
	}
	if a.webhook == "" && a.exec == "" && !d.hasErrorPrefix(joinPath(path, "webhook")) && !d.hasErrorPrefix(joinPath(path, "exec")) {
		d.add(path, "webhook or exec is required")
	}
	return a
}

func (d *parser) hasErrorPrefix(path string) bool {
	for _, e := range d.errs {
		if e.Path == path || strings.HasPrefix(e.Path, path+".") || strings.HasPrefix(e.Path, path+"[") {
			return true
		}
	}
	return false
}

// parse is the parser: decode the one YAML document of data, expand the
// templates and check everything, with t as "now" for start-time HH:MM.
//
//declscope:package // Parse, the package API, enters the parser here
func parse(data []byte, t time.Time) (*Config, error) {
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var doc yaml.Node
	if err := dec.Decode(&doc); err != nil && !errors.Is(err, io.EOF) {
		return nil, &ValidationError{Errors: []FieldError{{Msg: err.Error()}}}
	}
	var extra yaml.Node
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		msg := "multiple YAML documents are not supported"
		if err != nil {
			msg = err.Error()
		}
		return nil, &ValidationError{Errors: []FieldError{{Msg: msg}}}
	}
	root := &doc
	if doc.Kind == yaml.DocumentNode && len(doc.Content) > 0 {
		root = doc.Content[0]
	} else if doc.Kind == 0 || doc.Kind == yaml.DocumentNode {
		root = nil // empty file
	}

	d := &parser{now: t}
	f := d.file(root)
	cfg := d.expand(f)
	if len(d.errs) > 0 {
		return nil, &ValidationError{Errors: d.errs}
	}
	cfg.Warnings = d.warns
	return cfg, nil
}
