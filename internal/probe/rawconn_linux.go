// The Linux raw ICMP socket behind packetConn (namespace "raw").
//
//declscope:namespace raw

package probe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"golang.org/x/sys/unix"

	"goipsla/internal/clock"
)

// rawConn is a raw ICMP or ICMPv6 socket. Reads and writes go through
// syscall.RawConn so that waiting uses the runtime's netpoller (epoll) rather
// than a blocked thread or a busy loop, and so that Close unblocks a reader
// without racing on the file descriptor.
type rawConn struct {
	v6  bool
	f   *os.File
	rc  syscall.RawConn
	clk clock.Clock // measures the send deadline

	flMu      sync.Mutex
	flowSend  bool             // IPV6_FLOWINFO_SEND enabled
	flowReady map[uint32]error // registered labels (nil error) or why registration failed
}

// openRaw opens and configures a raw socket for the family, bound to vrf if
// it is not empty.
//
//declscope:package // the production opener that New hands to the engine
func openRaw(v6 bool, vrf string, clk clock.Clock) (packetConn, error) {
	family, proto, name := unix.AF_INET, unix.IPPROTO_ICMP, "icmp4"
	if v6 {
		family, proto, name = unix.AF_INET6, unix.IPPROTO_ICMPV6, "icmp6"
	}
	fd, err := unix.Socket(family, unix.SOCK_RAW|unix.SOCK_NONBLOCK|unix.SOCK_CLOEXEC, proto)
	if err != nil {
		return nil, fmt.Errorf("open %s raw socket: %w", name, err)
	}
	if err := configureRaw(fd, v6, vrf); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("configure %s raw socket: %w", name, err)
	}
	if vrf != "" {
		name += "%" + vrf
	}
	f := os.NewFile(uintptr(fd), name)
	rc, err := f.SyscallConn()
	if err != nil {
		f.Close()
		return nil, err
	}
	return &rawConn{v6: v6, f: f, rc: rc, clk: clk, flowReady: make(map[uint32]error)}, nil
}

func configureRaw(fd int, v6 bool, vrf string) error {
	// Socket options missing from golang.org/x/sys/unix (linux/icmp.h,
	// linux/icmpv6.h).
	const (
		sockoptICMPFilter   = 1 // ICMP_FILTER, level SOL_RAW
		sockoptICMPv6Filter = 1 // ICMPV6_FILTER, level SOL_ICMPV6
		receiveBufferSize   = 1 << 20
	)
	if v6 {
		// A set bit blocks the type.
		var filt unix.ICMPv6Filter
		for i := range filt.Data {
			filt.Data[i] = 0xffffffff
		}
		for _, t := range acceptedICMPTypes(true) {
			filt.Data[t>>5] &^= 1 << (uint(t) & 31)
		}
		if err := unix.SetsockoptICMPv6Filter(fd, unix.SOL_ICMPV6, sockoptICMPv6Filter, &filt); err != nil {
			return fmt.Errorf("ICMPV6_FILTER: %w", err)
		}
	} else {
		// A set bit blocks the type.
		var pass uint32
		for _, t := range acceptedICMPTypes(false) {
			pass |= 1 << uint(t)
		}
		if err := unix.SetsockoptInt(fd, unix.SOL_RAW, sockoptICMPFilter, int(int32(^pass))); err != nil {
			return fmt.Errorf("ICMP_FILTER: %w", err)
		}
	}
	if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS_NEW, 1); err != nil {
		if err := unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_TIMESTAMPNS, 1); err != nil {
			return fmt.Errorf("SO_TIMESTAMPNS: %w", err)
		}
	}
	// Best effort: a larger buffer absorbs bursts from many operations.
	if unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUFFORCE, receiveBufferSize) != nil {
		_ = unix.SetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_RCVBUF, receiveBufferSize)
	}
	if vrf != "" {
		if err := unix.SetsockoptString(fd, unix.SOL_SOCKET, unix.SO_BINDTODEVICE, vrf); err != nil {
			return fmt.Errorf("SO_BINDTODEVICE %q: %w", vrf, err)
		}
	}
	return nil
}

func (c *rawConn) Close() error { return c.f.Close() }

func (c *rawConn) ReadFrom(buf []byte) (int, netip.Addr, time.Time, error) {
	const controlMessageCapacity = 128 // room for the receive timestamp
	var (
		n, oobn int
		from    unix.Sockaddr
		rerr    error
		oob     [controlMessageCapacity]byte
	)
	err := c.rc.Read(func(fd uintptr) bool {
		n, oobn, _, from, rerr = unix.Recvmsg(int(fd), buf, oob[:], 0)
		return rerr != unix.EAGAIN && rerr != unix.EWOULDBLOCK
	})
	if err != nil {
		if errors.Is(err, os.ErrClosed) {
			return 0, netip.Addr{}, time.Time{}, errConnClosed
		}
		return 0, netip.Addr{}, time.Time{}, err
	}
	if rerr != nil {
		return 0, netip.Addr{}, time.Time{}, rerr
	}
	var addr netip.Addr
	switch sa := from.(type) {
	case *unix.SockaddrInet4:
		addr = netip.AddrFrom4(sa.Addr)
	case *unix.SockaddrInet6:
		addr = netip.AddrFrom16(sa.Addr)
		if sa.ZoneId != 0 {
			addr = addr.WithZone(fmt.Sprint(sa.ZoneId))
		}
	}
	return n, addr, rawRxTimestamp(oob[:oobn]), nil
}

// rawRxTimestamp returns the SO_TIMESTAMPNS(_NEW) timestamp in a control
// message buffer, or the zero time. Both carry a 64-bit timespec on 64-bit
// platforms (SO_TIMESTAMPNS_NEW always does).
func rawRxTimestamp(oob []byte) time.Time {
	msgs, err := unix.ParseSocketControlMessage(oob)
	if err != nil {
		return time.Time{}
	}
	for _, m := range msgs {
		if m.Header.Level != unix.SOL_SOCKET {
			continue
		}
		switch m.Header.Type {
		case unix.SO_TIMESTAMPNS_NEW:
			if len(m.Data) >= 16 {
				sec := int64(binary.NativeEndian.Uint64(m.Data))
				nsec := int64(binary.NativeEndian.Uint64(m.Data[8:]))
				return time.Unix(sec, nsec)
			}
		case unix.SO_TIMESTAMPNS_OLD:
			var ts unix.Timespec
			if len(m.Data) >= int(unsafe.Sizeof(ts)) {
				ts = *(*unix.Timespec)(unsafe.Pointer(&m.Data[0]))
				return time.Unix(int64(ts.Sec), int64(ts.Nsec))
			}
		}
	}
	return time.Time{}
}

func (c *rawConn) WriteTo(msg []byte, dst netip.Addr, ctl connControl) error {
	var flowErr error
	if c.v6 && ctl.FlowLabel != 0 {
		if err := c.ensureFlowLabel(ctl.FlowLabel, dst); err != nil {
			flowErr = &connFlowLabelError{label: ctl.FlowLabel, err: err}
			ctl.FlowLabel = 0
		}
	}
	var (
		name    unsafe.Pointer
		namelen uint32
		oob     []byte
	)
	if c.v6 {
		sa := &unix.RawSockaddrInet6{
			Family:   unix.AF_INET6,
			Addr:     dst.As16(),
			Scope_id: uint32(ctl.ScopeID),
		}
		if ctl.FlowLabel != 0 && c.flowInfoSend() {
			var fi [4]byte
			binary.BigEndian.PutUint32(fi[:], ctl.FlowLabel&0xfffff)
			sa.Flowinfo = binary.NativeEndian.Uint32(fi[:])
		}
		name, namelen = unsafe.Pointer(sa), unix.SizeofSockaddrInet6
		if ctl.Src.IsValid() || ctl.IfIndex != 0 {
			pi := &unix.Inet6Pktinfo{Ifindex: uint32(ctl.IfIndex)}
			if ctl.Src.IsValid() {
				pi.Addr = ctl.Src.As16()
			}
			oob = appendRawCmsg(oob, unix.IPPROTO_IPV6, unix.IPV6_PKTINFO,
				unsafe.Slice((*byte)(unsafe.Pointer(pi)), unix.SizeofInet6Pktinfo))
		}
		if ctl.TOS != 0 {
			oob = appendRawCmsgInt(oob, unix.IPPROTO_IPV6, unix.IPV6_TCLASS, int32(ctl.TOS))
		}
	} else {
		sa := &unix.RawSockaddrInet4{Family: unix.AF_INET, Addr: dst.As4()}
		name, namelen = unsafe.Pointer(sa), unix.SizeofSockaddrInet4
		if ctl.Src.IsValid() || ctl.IfIndex != 0 {
			pi := &unix.Inet4Pktinfo{Ifindex: int32(ctl.IfIndex)}
			if ctl.Src.IsValid() {
				pi.Spec_dst = ctl.Src.As4()
			}
			oob = appendRawCmsg(oob, unix.IPPROTO_IP, unix.IP_PKTINFO,
				unsafe.Slice((*byte)(unsafe.Pointer(pi)), unix.SizeofInet4Pktinfo))
		}
		if ctl.TOS != 0 {
			oob = appendRawCmsgInt(oob, unix.IPPROTO_IP, unix.IP_TOS, int32(ctl.TOS))
		}
	}

	iov := unix.Iovec{Base: &msg[0]}
	iov.SetLen(len(msg))
	hdr := unix.Msghdr{Name: (*byte)(name), Namelen: namelen, Iov: &iov, Iovlen: 1}
	if len(oob) > 0 {
		hdr.Control = &oob[0]
		hdr.SetControllen(len(oob))
	}
	var serr error
	err := c.rc.Write(func(fd uintptr) bool {
		for {
			_, _, e := unix.Syscall(unix.SYS_SENDMSG, fd, uintptr(unsafe.Pointer(&hdr)), 0)
			if e != unix.EAGAIN && e != unix.EWOULDBLOCK {
				if e != 0 {
					serr = e
				}
				return true
			}
			// The send buffer is full, which is rare for ICMP. Without a
			// deadline, wait in the netpoller; with one, poll for
			// writability until the deadline (blocking this thread only in
			// this unusual case).
			if ctl.Deadline.IsZero() {
				return false
			}
			rem := ctl.Deadline.Sub(c.clk.Now())
			if rem <= 0 {
				serr = errConnSendTimeout
				return true
			}
			fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLOUT}}
			if _, perr := unix.Poll(fds, int((rem+time.Millisecond-1)/time.Millisecond)); perr != nil && perr != unix.EINTR {
				serr = perr
				return true
			}
		}
	})
	if err != nil {
		if errors.Is(err, os.ErrClosed) {
			return ErrClosed
		}
		return err
	}
	if serr == errConnSendTimeout {
		return serr
	}
	if serr != nil {
		return fmt.Errorf("sendmsg to %s: %w", dst, serr)
	}
	return flowErr
}

func appendRawCmsg(b []byte, level, typ int, data []byte) []byte {
	space := unix.CmsgSpace(len(data))
	off := len(b)
	b = append(b, make([]byte, space)...)
	h := (*unix.Cmsghdr)(unsafe.Pointer(&b[off]))
	h.Level = int32(level)
	h.Type = int32(typ)
	h.SetLen(unix.CmsgLen(len(data)))
	copy(b[off+unix.CmsgLen(0):], data)
	return b
}

func appendRawCmsgInt(b []byte, level, typ int, v int32) []byte {
	var d [4]byte
	binary.NativeEndian.PutUint32(d[:], uint32(v))
	return appendRawCmsg(b, level, typ, d[:])
}

func (c *rawConn) flowInfoSend() bool {
	c.flMu.Lock()
	defer c.flMu.Unlock()
	return c.flowSend
}

// ensureFlowLabel leases label on this socket with IPV6_FLOWLABEL_MGR, which
// Linux requires before a non-zero flow label may be sent. The lease is kept
// until the socket closes. The outcome is remembered per label.
func (c *rawConn) ensureFlowLabel(label uint32, dst netip.Addr) error {
	// From linux/in6.h, missing from golang.org/x/sys/unix.
	const (
		sockoptFlowLabelMgr   = 32 // IPV6_FLOWLABEL_MGR
		sockoptFlowInfoSend   = 33 // IPV6_FLOWINFO_SEND
		flowLabelActionGet    = 0  // IPV6_FL_A_GET
		flowLabelFlagCreate   = 1  // IPV6_FL_F_CREATE
		flowLabelShareProcess = 2  // IPV6_FL_S_PROCESS
		sizeofFlowLabelReq    = 32 // struct in6_flowlabel_req
	)
	c.flMu.Lock()
	defer c.flMu.Unlock()
	if err, ok := c.flowReady[label]; ok {
		return err
	}
	err := c.rc.Control(func(fd uintptr) {
		if !c.flowSend {
			if err := unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6, sockoptFlowInfoSend, 1); err != nil {
				c.flowReady[label] = fmt.Errorf("IPV6_FLOWINFO_SEND: %w", err)
				return
			}
			c.flowSend = true
		}
		var req [sizeofFlowLabelReq]byte
		a := dst.As16()
		copy(req[0:16], a[:])
		binary.BigEndian.PutUint32(req[16:], label&0xfffff)
		req[20] = flowLabelActionGet
		req[21] = flowLabelShareProcess
		binary.NativeEndian.PutUint16(req[22:], flowLabelFlagCreate)
		if err := unix.SetsockoptString(int(fd), unix.IPPROTO_IPV6, sockoptFlowLabelMgr, string(req[:])); err != nil {
			c.flowReady[label] = fmt.Errorf("IPV6_FLOWLABEL_MGR: %w", err)
			return
		}
		c.flowReady[label] = nil
	})
	if err != nil {
		return err
	}
	return c.flowReady[label]
}
