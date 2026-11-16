// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package dhcp is a small DHCPv4 client that speaks raw Ethernet frames, so
// it can configure an interface that has no address yet (on NetBSD, through
// bpf(4)). Packet handling is OS-independent; the caller supplies the link.
package dhcp

import (
	"encoding/binary"
	"errors"
	"net"
)

// Message types (option 53).
const (
	Discover = 1
	Offer    = 2
	Request  = 3
	Decline  = 4
	Ack      = 5
	Nak      = 6
	Release  = 7
)

// Option codes used by the client.
const (
	optSubnetMask      = 1
	optRouter          = 3
	optDNS             = 6
	optHostname        = 12
	optDomainName      = 15
	optMTU             = 26
	optBroadcast       = 28
	optNTPServers      = 42
	optRequestedIP     = 50
	optLeaseTime       = 51
	optMessageType     = 53
	optServerID        = 54
	optParamRequest    = 55
	optRenewalTime     = 58
	optClientID        = 61
	optDomainSearch    = 119
	optClasslessRoutes = 121
	optEnd             = 255
	optPad             = 0
)

const (
	bootRequest = 1
	bootReply   = 2
	magicCookie = 0x63825363
	clientPort  = 68
	serverPort  = 67
	fixedLen    = 236 // BOOTP header up to the magic cookie
)

// Message is a decoded DHCP packet.
type Message struct {
	Op      byte
	XID     uint32
	Flags   uint16
	CIAddr  net.IP
	YIAddr  net.IP
	SIAddr  net.IP
	CHAddr  net.HardwareAddr
	Options map[byte][]byte
}

// Type returns the option-53 message type, or 0.
func (m *Message) Type() byte {
	if v := m.Options[optMessageType]; len(v) == 1 {
		return v[0]
	}
	return 0
}

// Marshal encodes m as a BOOTP/DHCP payload. Options are written in
// ascending code order so output is deterministic.
func (m *Message) Marshal() []byte {
	b := make([]byte, fixedLen+4, 300)
	b[0] = m.Op
	b[1] = 1 // htype ethernet
	b[2] = 6 // hlen
	binary.BigEndian.PutUint32(b[4:], m.XID)
	binary.BigEndian.PutUint16(b[10:], m.Flags)
	copy(b[12:16], m.CIAddr.To4())
	copy(b[16:20], m.YIAddr.To4())
	copy(b[20:24], m.SIAddr.To4())
	copy(b[28:44], m.CHAddr)
	binary.BigEndian.PutUint32(b[fixedLen:], magicCookie)
	for code := 1; code < 255; code++ {
		v, ok := m.Options[byte(code)]
		if !ok {
			continue
		}
		for len(v) > 255 {
			b = append(b, byte(code), 255)
			b = append(b, v[:255]...)
			v = v[255:]
		}
		b = append(b, byte(code), byte(len(v)))
		b = append(b, v...)
	}
	b = append(b, optEnd)
	for len(b) < 300 { // some servers drop packets shorter than a BOOTP minimum
		b = append(b, optPad)
	}
	return b
}

// ParseMessage decodes a BOOTP/DHCP payload. Repeated options are
// concatenated as RFC 3396 requires.
func ParseMessage(b []byte) (*Message, error) {
	if len(b) < fixedLen+4 {
		return nil, errors.New("dhcp: short packet")
	}
	if binary.BigEndian.Uint32(b[fixedLen:]) != magicCookie {
		return nil, errors.New("dhcp: missing magic cookie")
	}
	m := &Message{
		Op:      b[0],
		XID:     binary.BigEndian.Uint32(b[4:]),
		Flags:   binary.BigEndian.Uint16(b[10:]),
		CIAddr:  net.IP(append([]byte(nil), b[12:16]...)),
		YIAddr:  net.IP(append([]byte(nil), b[16:20]...)),
		SIAddr:  net.IP(append([]byte(nil), b[20:24]...)),
		CHAddr:  net.HardwareAddr(append([]byte(nil), b[28:34]...)),
		Options: map[byte][]byte{},
	}
	opts := b[fixedLen+4:]
	for len(opts) > 0 {
		code := opts[0]
		if code == optEnd {
			break
		}
		if code == optPad {
			opts = opts[1:]
			continue
		}
		if len(opts) < 2 || len(opts) < 2+int(opts[1]) {
			return nil, errors.New("dhcp: truncated option")
		}
		n := int(opts[1])
		m.Options[code] = append(m.Options[code], opts[2:2+n]...)
		opts = opts[2+n:]
	}
	return m, nil
}

func ipChecksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(binary.BigEndian.Uint16(b[i:]))
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum > 0xffff {
		sum = sum>>16 + sum&0xffff
	}
	return ^uint16(sum)
}

// Frame wraps a DHCP payload sent from 0.0.0.0:68 to 255.255.255.255:67 in
// UDP, IPv4 and Ethernet broadcast headers.
func Frame(src net.HardwareAddr, payload []byte) []byte {
	const ethLen, ipLen, udpLen = 14, 20, 8
	f := make([]byte, ethLen+ipLen+udpLen+len(payload))
	copy(f[0:6], []byte{0xff, 0xff, 0xff, 0xff, 0xff, 0xff})
	copy(f[6:12], src)
	binary.BigEndian.PutUint16(f[12:], 0x0800)

	ip := f[ethLen : ethLen+ipLen]
	ip[0] = 0x45
	binary.BigEndian.PutUint16(ip[2:], uint16(ipLen+udpLen+len(payload)))
	ip[8] = 64 // TTL
	ip[9] = 17 // UDP
	copy(ip[16:20], net.IPv4bcast.To4())
	binary.BigEndian.PutUint16(ip[10:], ipChecksum(ip))

	udp := f[ethLen+ipLen : ethLen+ipLen+udpLen]
	binary.BigEndian.PutUint16(udp[0:], clientPort)
	binary.BigEndian.PutUint16(udp[2:], serverPort)
	binary.BigEndian.PutUint16(udp[4:], uint16(udpLen+len(payload)))
	// A zero UDP checksum means "none" over IPv4.
	copy(f[ethLen+ipLen+udpLen:], payload)
	return f
}

// Unframe extracts the DHCP payload from an Ethernet frame if it is an IPv4
// UDP datagram addressed to the client port; otherwise it returns nil.
func Unframe(f []byte) []byte {
	if len(f) < 14+20+8 || binary.BigEndian.Uint16(f[12:]) != 0x0800 {
		return nil
	}
	ip := f[14:]
	ihl := int(ip[0]&0x0f) * 4
	if ip[0]>>4 != 4 || ihl < 20 || len(ip) < ihl+8 || ip[9] != 17 {
		return nil
	}
	if binary.BigEndian.Uint16(ip[6:])&0x3fff != 0 { // fragmented
		return nil
	}
	total := int(binary.BigEndian.Uint16(ip[2:]))
	if total > len(ip) || total < ihl+8 {
		return nil
	}
	udp := ip[ihl:total]
	if binary.BigEndian.Uint16(udp[2:]) != clientPort {
		return nil
	}
	ulen := int(binary.BigEndian.Uint16(udp[4:]))
	if ulen < 8 || ulen > len(udp) {
		return nil
	}
	return udp[8:ulen]
}
