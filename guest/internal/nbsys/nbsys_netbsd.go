// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package nbsys wraps the NetBSD system interfaces the init shim needs and
// golang.org/x/sys/unix does not cover: mount(2), reboot(2), interface
// ioctls, routing sockets and bpf(4).
//
// Struct layouts follow NetBSD's headers on LP64 targets.
package nbsys

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"time"
	"unsafe"

	"golang.org/x/net/route"
	"golang.org/x/sys/unix"
)

const (
	sysMount50 = 410 // __mount50(type, path, flags, data, data_len)

	// sys/reboot.h
	RBHalt      = 0x008
	RBPowerdown = RBHalt | 0x800
	RBAutoboot  = 0

	ifNameSize = 16
	ifreqSize  = 144 // IFNAMSIZ + sizeof(struct sockaddr_storage)

	// _IOW('i', 127, struct ifreq)
	siocSIFMTU = 0x80000000 | ifreqSize<<16 | 'i'<<8 | 127
)

func mount(fstype, dir string, flags int, data unsafe.Pointer, dataLen uintptr) error {
	t, err := unix.BytePtrFromString(fstype)
	if err != nil {
		return err
	}
	d, err := unix.BytePtrFromString(dir)
	if err != nil {
		return err
	}
	_, _, e := unix.Syscall6(sysMount50, uintptr(unsafe.Pointer(t)), uintptr(unsafe.Pointer(d)), uintptr(flags), uintptr(data), dataLen, 0)
	if e != 0 {
		return fmt.Errorf("mount %s on %s: %w", fstype, dir, e)
	}
	return nil
}

// struct tmpfs_args (sys/fs/tmpfs/tmpfs_args.h), version 1.
type tmpfsArgs struct {
	version  int32
	_        int32
	nodesMax uint64
	sizeMax  int64
	rootUID  uint32
	rootGID  uint32
	rootMode uint32
	_        uint32
}

// MountTmpfs mounts a tmpfs on dir. A zero size means the kernel default
// (bounded by available memory).
func MountTmpfs(dir string, mode uint32, size int64) error {
	args := tmpfsArgs{version: 1, sizeMax: size, rootMode: mode}
	return mount("tmpfs", dir, unix.MNT_NOSUID|unix.MNT_NODEV, unsafe.Pointer(&args), unsafe.Sizeof(args))
}

// MountKernfs mounts kernfs(5) on dir.
func MountKernfs(dir string) error {
	return mount("kernfs", dir, 0, nil, 0)
}

// Reboot calls reboot(2) with howto, e.g. RBPowerdown. It returns only on
// failure.
func Reboot(howto int) error {
	_, _, e := unix.Syscall(unix.SYS_REBOOT, uintptr(howto), 0, 0)
	return e
}

// SetHostname sets kern.hostname.
func SetHostname(name string) error {
	mib := [2]int32{1, 10} // CTL_KERN, KERN_HOSTNAME
	b := []byte(name)
	if len(b) == 0 {
		return nil
	}
	_, _, e := unix.Syscall6(unix.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), 2, 0, 0, uintptr(unsafe.Pointer(&b[0])), uintptr(len(b)))
	if e != 0 {
		return fmt.Errorf("sysctl kern.hostname: %w", e)
	}
	return nil
}

type ifreq [ifreqSize]byte

func newIfreq(name string) (*ifreq, error) {
	if len(name) >= ifNameSize {
		return nil, fmt.Errorf("interface name %q too long", name)
	}
	var r ifreq
	copy(r[:], name)
	return &r, nil
}

func ioctl(fd int, req uint, arg unsafe.Pointer) error {
	_, _, e := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(req), uintptr(arg))
	if e != 0 {
		return e
	}
	return nil
}

func withInetSocket(f func(fd int) error) error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	return f(fd)
}

// IfUp sets IFF_UP on an interface.
func IfUp(name string) error {
	r, err := newIfreq(name)
	if err != nil {
		return err
	}
	return withInetSocket(func(fd int) error {
		if err := ioctl(fd, unix.SIOCGIFFLAGS, unsafe.Pointer(r)); err != nil {
			return fmt.Errorf("SIOCGIFFLAGS %s: %w", name, err)
		}
		flags := binary.NativeEndian.Uint16(r[ifNameSize:]) | unix.IFF_UP
		binary.NativeEndian.PutUint16(r[ifNameSize:], flags)
		if err := ioctl(fd, unix.SIOCSIFFLAGS, unsafe.Pointer(r)); err != nil {
			return fmt.Errorf("SIOCSIFFLAGS %s: %w", name, err)
		}
		return nil
	})
}

// SetMTU sets an interface's MTU.
func SetMTU(name string, mtu int) error {
	r, err := newIfreq(name)
	if err != nil {
		return err
	}
	binary.NativeEndian.PutUint32(r[ifNameSize:], uint32(mtu))
	return withInetSocket(func(fd int) error {
		if err := ioctl(fd, siocSIFMTU, unsafe.Pointer(r)); err != nil {
			return fmt.Errorf("SIOCSIFMTU %s: %w", name, err)
		}
		return nil
	})
}

// putSockaddrIn writes a struct sockaddr_in.
func putSockaddrIn(b []byte, ip net.IP) {
	b[0] = 16
	b[1] = unix.AF_INET
	copy(b[4:8], ip.To4())
}

// AddAddr adds an IPv4 address to an interface (SIOCAIFADDR).
func AddAddr(name string, ip net.IP, mask net.IPMask, bcast net.IP) error {
	// struct in_aliasreq: name, addr, dstaddr/broadaddr, mask.
	var r [ifNameSize + 3*16]byte
	if len(name) >= ifNameSize {
		return fmt.Errorf("interface name %q too long", name)
	}
	copy(r[:], name)
	putSockaddrIn(r[16:], ip)
	if bcast != nil {
		putSockaddrIn(r[32:], bcast)
	}
	putSockaddrIn(r[48:], net.IP(mask))
	return withInetSocket(func(fd int) error {
		if err := ioctl(fd, unix.SIOCAIFADDR, unsafe.Pointer(&r)); err != nil {
			return fmt.Errorf("SIOCAIFADDR %s %s: %w", name, ip, err)
		}
		return nil
	})
}

// AddRoute adds an IPv4 route. A nil or unspecified gateway makes the route
// on-link through the interface with index ifIndex.
func AddRoute(dst net.IPNet, gw net.IP, ifIndex int) error {
	var d, m [4]byte
	copy(d[:], dst.IP.To4())
	copy(m[:], net.IP(dst.Mask).To4())
	addrs := make([]route.Addr, unix.RTAX_NETMASK+1)
	addrs[unix.RTAX_DST] = &route.Inet4Addr{IP: d}
	addrs[unix.RTAX_NETMASK] = &route.Inet4Addr{IP: m}
	flags := unix.RTF_UP | unix.RTF_STATIC
	if gw == nil || gw.IsUnspecified() {
		addrs[unix.RTAX_GATEWAY] = &route.LinkAddr{Index: ifIndex}
	} else {
		var g [4]byte
		copy(g[:], gw.To4())
		addrs[unix.RTAX_GATEWAY] = &route.Inet4Addr{IP: g}
		flags |= unix.RTF_GATEWAY
	}
	if ones, _ := dst.Mask.Size(); ones == 32 {
		flags |= unix.RTF_HOST
	}
	msg := route.RouteMessage{
		Version: unix.RTM_VERSION,
		Type:    unix.RTM_ADD,
		Flags:   flags,
		ID:      uintptr(os.Getpid()),
		Seq:     1,
		Addrs:   addrs,
	}
	b, err := msg.Marshal()
	if err != nil {
		return err
	}
	fd, err := unix.Socket(unix.AF_ROUTE, unix.SOCK_RAW, unix.AF_UNSPEC)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if _, err := unix.Write(fd, b); err != nil {
		return fmt.Errorf("route add %s via %v: %w", dst.String(), gw, err)
	}
	return nil
}

// BPF is a bpf(4) descriptor bound to one interface. It implements
// dhcp.Link.
type BPF struct {
	fd      int
	hw      net.HardwareAddr
	buf     []byte
	pending [][]byte
}

// OpenBPF opens /dev/bpf on the named interface.
func OpenBPF(ifName string, hw net.HardwareAddr) (*BPF, error) {
	fd, err := unix.Open("/dev/bpf", unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/bpf: %w", err)
	}
	b := &BPF{fd: fd, hw: hw}
	fail := func(what string, err error) (*BPF, error) {
		unix.Close(fd)
		return nil, fmt.Errorf("bpf %s: %w", what, err)
	}
	blen := uint32(32768)
	if err := ioctl(fd, unix.BIOCSBLEN, unsafe.Pointer(&blen)); err != nil {
		return fail("BIOCSBLEN", err)
	}
	r, err := newIfreq(ifName)
	if err != nil {
		return fail("ifreq", err)
	}
	if err := ioctl(fd, unix.BIOCSETIF, unsafe.Pointer(r)); err != nil {
		return fail("BIOCSETIF", err)
	}
	one := uint32(1)
	if err := ioctl(fd, unix.BIOCIMMEDIATE, unsafe.Pointer(&one)); err != nil {
		return fail("BIOCIMMEDIATE", err)
	}
	if err := ioctl(fd, unix.BIOCSHDRCMPLT, unsafe.Pointer(&one)); err != nil {
		return fail("BIOCSHDRCMPLT", err)
	}
	b.buf = make([]byte, blen)
	return b, nil
}

func (b *BPF) HardwareAddr() net.HardwareAddr { return b.hw }

func (b *BPF) Send(frame []byte) error {
	_, err := unix.Write(b.fd, frame)
	return err
}

func (b *BPF) Recv(deadline time.Time) ([]byte, error) {
	for len(b.pending) == 0 {
		ms := int(time.Until(deadline).Milliseconds())
		if ms <= 0 {
			return nil, nil
		}
		fds := []unix.PollFd{{Fd: int32(b.fd), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, ms)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if n == 0 {
			return nil, nil
		}
		nr, err := unix.Read(b.fd, b.buf)
		if err != nil {
			return nil, err
		}
		b.split(b.buf[:nr])
	}
	f := b.pending[0]
	b.pending = b.pending[1:]
	return f, nil
}

// split breaks a bpf read buffer into frames. Each frame is preceded by a
// struct bpf_hdr (16-byte timestamp, caplen, datalen, hdrlen) and padded to
// BPF_WORDALIGN (sizeof(long)).
func (b *BPF) split(buf []byte) {
	const align = int(unsafe.Sizeof(uintptr(0)))
	for len(buf) >= 26 {
		caplen := int(binary.NativeEndian.Uint32(buf[16:]))
		hdrlen := int(binary.NativeEndian.Uint16(buf[24:]))
		if hdrlen+caplen > len(buf) {
			return
		}
		b.pending = append(b.pending, append([]byte(nil), buf[hdrlen:hdrlen+caplen]...))
		next := (hdrlen + caplen + align - 1) &^ (align - 1)
		if next >= len(buf) {
			return
		}
		buf = buf[next:]
	}
}

func (b *BPF) Close() error { return unix.Close(b.fd) }

// Power events (sys/power.h).
const (
	powerEventMsgSize      = 32
	powerEventSwitchChange = 0
	pswitchTypePower       = 0
	pswitchTypeReset       = 3
	pswitchEventPressed    = 0
)

// PowerEvent is a power switch event read from /dev/power.
type PowerEvent struct {
	// Name is the switch device (e.g. "acpibut0"), empty when the kernel
	// lacks COMPAT_40 and only reports that some switch changed.
	Name string
	// Reset is true for a reset button; anything else is a power-off
	// request.
	Reset bool
}

// Power is /dev/power held open by its single reader. While it is open the
// kernel queues power switch events instead of acting on them itself.
type Power struct{ fd int }

// OpenPower opens /dev/power.
func OpenPower() (*Power, error) {
	fd, err := unix.Open("/dev/power", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/power: %w", err)
	}
	return &Power{fd}, nil
}

// Next blocks until a power switch is pressed. Releases and environmental
// sensor events are skipped.
func (p *Power) Next() (PowerEvent, error) {
	var buf [powerEventMsgSize]byte
	for {
		n, err := unix.Read(p.fd, buf[:])
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return PowerEvent{}, err
		}
		if n != len(buf) || binary.NativeEndian.Uint32(buf[0:]) != powerEventSwitchChange {
			continue
		}
		// struct pswitch_state { char name[16]; int32 type; int32 state; }
		name := string(bytes.TrimRight(buf[4:20], "\x00"))
		typ := binary.NativeEndian.Uint32(buf[20:])
		state := binary.NativeEndian.Uint32(buf[24:])
		if name == "" {
			// No details without COMPAT_40. The CLOUD kernel's only
			// switches (acpibut, hvshutdown) are power buttons.
			return PowerEvent{}, nil
		}
		if state != pswitchEventPressed {
			continue
		}
		switch typ {
		case pswitchTypePower:
			return PowerEvent{Name: name}, nil
		case pswitchTypeReset:
			return PowerEvent{Name: name, Reset: true}, nil
		}
	}
}

// SystemClock is the kernel's real-time clock. It implements sntp.Clock.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// Step moves the clock by d at once, cancelling any adjustment still being
// slewed in so that it does not land on top of the new time.
func (SystemClock) Step(d time.Duration) error {
	var zero unix.Timeval
	unix.Adjtime(&zero, nil)
	tv := unix.NsecToTimeval(time.Now().Add(d).UnixNano())
	if err := unix.Settimeofday(&tv); err != nil {
		return fmt.Errorf("settimeofday: %w", err)
	}
	return nil
}

// Slew has the kernel absorb d gradually (adjtime(2)), replacing any
// adjustment still in progress.
func (SystemClock) Slew(d time.Duration) error {
	tv := unix.NsecToTimeval(d.Nanoseconds())
	if err := unix.Adjtime(&tv, nil); err != nil {
		return fmt.Errorf("adjtime: %w", err)
	}
	return nil
}

// _IOW('q', 0, uint16_t) (dev/ic/qemufwcfgio.h)
const fwcfgioSetIndex = 0x80000000 | 2<<16 | 'q'<<8 | 0

// FWCfg is QEMU's firmware configuration device, /dev/qemufwcfg. It
// implements metadata.FWCfg.
type FWCfg struct{ fd int }

// OpenFWCfg opens /dev/qemufwcfg; it fails off QEMU, where the device does
// not attach.
func OpenFWCfg() (*FWCfg, error) {
	fd, err := unix.Open("/dev/qemufwcfg", unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open /dev/qemufwcfg: %w", err)
	}
	return &FWCfg{fd}, nil
}

func (f *FWCfg) Select(item uint16) error {
	return ioctl(f.fd, fwcfgioSetIndex, unsafe.Pointer(&item))
}

func (f *FWCfg) Read(n int) ([]byte, error) {
	buf := make([]byte, n)
	for off := 0; off < n; {
		m, err := unix.Read(f.fd, buf[off:])
		if err != nil {
			return nil, err
		}
		if m == 0 {
			return nil, fmt.Errorf("qemufwcfg: short read")
		}
		off += m
	}
	return buf, nil
}

func (f *FWCfg) Close() error { return unix.Close(f.fd) }

func rawSysctl(mib []int32, old *byte, oldlen *uintptr, new *byte, newlen uintptr) error {
	_, _, e := unix.Syscall6(unix.SYS___SYSCTL, uintptr(unsafe.Pointer(&mib[0])), uintptr(len(mib)),
		uintptr(unsafe.Pointer(old)), uintptr(unsafe.Pointer(oldlen)), uintptr(unsafe.Pointer(new)), newlen)
	if e != 0 {
		return e
	}
	return nil
}

// sysctlNodes lists the nodes under mib with a CTL_QUERY (the same
// protocol golang.org/x/sys/unix uses internally for names).
func sysctlNodes(mib []int32) ([]unix.Sysctlnode, error) {
	mib = append(append([]int32(nil), mib...), unix.CTL_QUERY)
	q := unix.Sysctlnode{Flags: unix.SYSCTL_VERS_1}
	sz := unsafe.Sizeof(q)
	var olen uintptr
	if err := rawSysctl(mib, nil, &olen, (*byte)(unsafe.Pointer(&q)), sz); err != nil {
		return nil, err
	}
	nodes := make([]unix.Sysctlnode, olen/sz)
	if len(nodes) == 0 {
		return nil, nil
	}
	if err := rawSysctl(mib, (*byte)(unsafe.Pointer(&nodes[0])), &olen, (*byte)(unsafe.Pointer(&q)), sz); err != nil {
		return nil, err
	}
	return nodes[:olen/sz], nil
}

// sysctlMIB resolves a dotted sysctl name such as "net.inet.ip.dad_count".
func sysctlMIB(name string) ([]int32, error) {
	var mib []int32
	for _, part := range strings.Split(name, ".") {
		nodes, err := sysctlNodes(mib)
		if err != nil {
			return nil, fmt.Errorf("sysctl %s: %w", name, err)
		}
		found := false
		for _, n := range nodes {
			b := make([]byte, 0, len(n.Name))
			for _, c := range n.Name {
				if c == 0 {
					break
				}
				b = append(b, byte(c))
			}
			if string(b) == part {
				mib = append(mib, n.Num)
				found = true
				break
			}
		}
		if !found {
			return nil, fmt.Errorf("sysctl %s: no node %q", name, part)
		}
	}
	return mib, nil
}

// SetSysctlInt sets an integer sysctl by name.
func SetSysctlInt(name string, v int32) error {
	mib, err := sysctlMIB(name)
	if err != nil {
		return err
	}
	if err := rawSysctl(mib, nil, nil, (*byte)(unsafe.Pointer(&v)), unsafe.Sizeof(v)); err != nil {
		return fmt.Errorf("sysctl -w %s=%d: %w", name, v, err)
	}
	return nil
}
