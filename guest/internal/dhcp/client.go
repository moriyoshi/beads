// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package dhcp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
)

// Link sends and receives raw Ethernet frames on one interface.
type Link interface {
	HardwareAddr() net.HardwareAddr
	Send(frame []byte) error
	// Recv returns the next frame, or (nil, nil) once deadline passes.
	Recv(deadline time.Time) ([]byte, error)
}

// Route is a classless static route (option 121). A zero Gateway means the
// destination is on-link.
type Route struct {
	Dst     net.IPNet
	Gateway net.IP
}

// Lease is the configuration a server handed out.
type Lease struct {
	Addr      net.IP
	Mask      net.IPMask
	Broadcast net.IP
	// Routes holds option 121 when the server sent it, otherwise a single
	// default route via option 3, as RFC 3442 requires.
	Routes    []Route
	DNS       []net.IP
	NTP       []net.IP // option 42
	Domain    string
	Search    []string
	Hostname  string
	MTU       int
	ServerID  net.IP
	LeaseTime time.Duration
}

func (l *Lease) String() string {
	ones, _ := l.Mask.Size()
	var gw []string
	for _, r := range l.Routes {
		if ones, _ := r.Dst.Mask.Size(); ones == 0 {
			gw = append(gw, r.Gateway.String())
		}
	}
	return fmt.Sprintf("%s/%d gw %s dns %v", l.Addr, ones, strings.Join(gw, ","), l.DNS)
}

var paramRequest = []byte{
	optSubnetMask, optRouter, optDNS, optHostname, optDomainName,
	optMTU, optBroadcast, optNTPServers, optDomainSearch, optClasslessRoutes,
}

// Client acquires a lease over a Link.
type Client struct {
	Link Link
	// XID seeds transaction IDs; callers should randomise it.
	XID uint32
	// Logf, if set, receives progress messages.
	Logf func(format string, args ...any)
}

func (c *Client) logf(format string, args ...any) {
	if c.Logf != nil {
		c.Logf(format, args...)
	}
}

func (c *Client) message(typ byte, xid uint32, extra map[byte][]byte) *Message {
	hw := c.Link.HardwareAddr()
	m := &Message{
		Op:     bootRequest,
		XID:    xid,
		Flags:  0x8000, // ask for broadcast replies; we have no address yet
		CHAddr: hw,
		Options: map[byte][]byte{
			optMessageType:  {typ},
			optParamRequest: paramRequest,
			optClientID:     append([]byte{1}, hw...),
		},
	}
	for k, v := range extra {
		m.Options[k] = v
	}
	return m
}

// exchange sends m and waits until deadline for a reply of one of the wanted
// types carrying the same transaction ID.
func (c *Client) exchange(m *Message, deadline time.Time, want ...byte) (*Message, error) {
	if err := c.Link.Send(Frame(c.Link.HardwareAddr(), m.Marshal())); err != nil {
		return nil, err
	}
	for {
		f, err := c.Link.Recv(deadline)
		if err != nil {
			return nil, err
		}
		if f == nil {
			return nil, nil
		}
		p := Unframe(f)
		if p == nil {
			continue
		}
		r, err := ParseMessage(p)
		if err != nil || r.Op != bootReply || r.XID != m.XID {
			continue
		}
		for _, w := range want {
			if r.Type() == w {
				return r, nil
			}
		}
	}
}

// Acquire runs DISCOVER/OFFER/REQUEST/ACK until it gets a lease or ctx ends.
// Retransmissions back off from 1s up to 16s.
func (c *Client) Acquire(ctx context.Context) (*Lease, error) {
	wait := time.Second
	xid := c.XID
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		xid++
		lease, err := c.attempt(ctx, xid, wait)
		if err != nil {
			return nil, err
		}
		if lease != nil {
			return lease, nil
		}
		if wait < 16*time.Second {
			wait *= 2
		}
	}
}

func (c *Client) attempt(ctx context.Context, xid uint32, wait time.Duration) (*Lease, error) {
	deadline := time.Now().Add(wait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	offer, err := c.exchange(c.message(Discover, xid, nil), deadline, Offer)
	if err != nil || offer == nil {
		return nil, err
	}
	sid := offer.Options[optServerID]
	c.logf("offer %s from %s", offer.YIAddr, net.IP(sid))

	deadline = time.Now().Add(wait)
	req := c.message(Request, xid, map[byte][]byte{
		optRequestedIP: offer.YIAddr.To4(),
		optServerID:    sid,
	})
	ack, err := c.exchange(req, deadline, Ack, Nak)
	if err != nil || ack == nil {
		return nil, err
	}
	if ack.Type() == Nak {
		c.logf("server sent NAK")
		return nil, nil
	}
	return leaseFrom(ack)
}

func leaseFrom(m *Message) (*Lease, error) {
	l := &Lease{Addr: m.YIAddr.To4()}
	if l.Addr == nil || l.Addr.IsUnspecified() {
		return nil, errors.New("dhcp: ACK without an address")
	}
	if v := m.Options[optSubnetMask]; len(v) == 4 {
		l.Mask = net.IPMask(v)
	} else {
		l.Mask = l.Addr.DefaultMask()
	}
	if v := m.Options[optBroadcast]; len(v) == 4 {
		l.Broadcast = net.IP(v)
	} else {
		b := make(net.IP, 4)
		for i := range b {
			b[i] = l.Addr[i] | ^l.Mask[i]
		}
		l.Broadcast = b
	}
	if v, ok := m.Options[optClasslessRoutes]; ok {
		routes, err := parseClasslessRoutes(v)
		if err != nil {
			return nil, err
		}
		l.Routes = routes
	} else if v := m.Options[optRouter]; len(v) >= 4 {
		l.Routes = []Route{{Dst: net.IPNet{IP: net.IPv4zero.To4(), Mask: net.CIDRMask(0, 32)}, Gateway: net.IP(v[:4])}}
	}
	for v := m.Options[optDNS]; len(v) >= 4; v = v[4:] {
		l.DNS = append(l.DNS, net.IP(v[:4]))
	}
	for v := m.Options[optNTPServers]; len(v) >= 4; v = v[4:] {
		l.NTP = append(l.NTP, net.IP(v[:4]))
	}
	l.Domain = strings.TrimRight(string(m.Options[optDomainName]), "\x00")
	l.Hostname = strings.TrimRight(string(m.Options[optHostname]), "\x00")
	if v := m.Options[optDomainSearch]; len(v) > 0 {
		l.Search = parseDomainList(v)
	}
	if v := m.Options[optMTU]; len(v) == 2 {
		l.MTU = int(binary.BigEndian.Uint16(v))
	}
	if v := m.Options[optServerID]; len(v) == 4 {
		l.ServerID = net.IP(v)
	}
	if v := m.Options[optLeaseTime]; len(v) == 4 {
		l.LeaseTime = time.Duration(binary.BigEndian.Uint32(v)) * time.Second
	}
	return l, nil
}

// parseClasslessRoutes decodes RFC 3442 option 121.
func parseClasslessRoutes(b []byte) ([]Route, error) {
	var out []Route
	for len(b) > 0 {
		width := int(b[0])
		if width > 32 {
			return nil, fmt.Errorf("dhcp: bad classless route width %d", width)
		}
		n := (width + 7) / 8
		if len(b) < 1+n+4 {
			return nil, errors.New("dhcp: truncated classless route")
		}
		dst := make(net.IP, 4)
		copy(dst, b[1:1+n])
		out = append(out, Route{
			Dst:     net.IPNet{IP: dst, Mask: net.CIDRMask(width, 32)},
			Gateway: net.IP(append([]byte(nil), b[1+n:1+n+4]...)),
		})
		b = b[1+n+4:]
	}
	return out, nil
}

// parseDomainList decodes the DNS-encoded name list of option 119,
// following compression pointers within the option.
func parseDomainList(b []byte) []string {
	var out []string
	for off := 0; off < len(b); {
		name, next, ok := readName(b, off)
		if !ok {
			break
		}
		if name != "" {
			out = append(out, name)
		}
		off = next
	}
	return out
}

func readName(b []byte, off int) (name string, next int, ok bool) {
	var labels []string
	next = -1
	for hops := 0; hops < 64; hops++ {
		if off >= len(b) {
			return "", 0, false
		}
		n := int(b[off])
		switch {
		case n == 0:
			if next < 0 {
				next = off + 1
			}
			return strings.Join(labels, "."), next, true
		case n&0xc0 == 0xc0:
			if off+1 >= len(b) {
				return "", 0, false
			}
			if next < 0 {
				next = off + 2
			}
			off = int(binary.BigEndian.Uint16(b[off:]) & 0x3fff)
		default:
			if off+1+n > len(b) {
				return "", 0, false
			}
			labels = append(labels, string(b[off+1:off+1+n]))
			off += 1 + n
		}
	}
	return "", 0, false
}
