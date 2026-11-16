// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package dhcp

import (
	"context"
	"encoding/binary"
	"net"
	"reflect"
	"testing"
	"time"
)

// replyFrame builds a server-to-client frame by reusing Frame and swapping
// the UDP ports.
func replyFrame(m *Message) []byte {
	f := Frame(net.HardwareAddr{2, 0, 0, 0, 0, 1}, m.Marshal())
	binary.BigEndian.PutUint16(f[34:], serverPort)
	binary.BigEndian.PutUint16(f[36:], clientPort)
	return f
}

// fakeServer answers DISCOVER and REQUEST like a minimal DHCP server. It
// drops the first nDrop requests to exercise retransmission.
type fakeServer struct {
	hw    net.HardwareAddr
	queue [][]byte
	nDrop int
	nak   bool
	opts  map[byte][]byte
	sent  []*Message
}

func (s *fakeServer) HardwareAddr() net.HardwareAddr { return s.hw }

func (s *fakeServer) Send(f []byte) error {
	m, err := ParseMessage(Unframe(withClientPort(f)))
	if err != nil {
		return err
	}
	s.sent = append(s.sent, m)
	if s.nDrop > 0 {
		s.nDrop--
		return nil
	}
	reply := &Message{Op: bootReply, XID: m.XID, CHAddr: m.CHAddr, YIAddr: net.IPv4(10, 0, 2, 15), Options: map[byte][]byte{
		optServerID: {10, 0, 2, 2},
	}}
	for k, v := range s.opts {
		reply.Options[k] = v
	}
	switch m.Type() {
	case Discover:
		reply.Options[optMessageType] = []byte{Offer}
	case Request:
		reply.Options[optMessageType] = []byte{Ack}
		if s.nak {
			reply.Options[optMessageType] = []byte{Nak}
			s.nak = false
		}
	}
	// Noise the client must skip: a frame for another transaction, and a
	// non-IP frame.
	other := *reply
	other.XID++
	s.queue = append(s.queue, replyFrame(&other), make([]byte, 60), replyFrame(reply))
	return nil
}

// withClientPort rewrites the destination port so Unframe accepts a
// client-sent frame.
func withClientPort(f []byte) []byte {
	g := append([]byte(nil), f...)
	binary.BigEndian.PutUint16(g[36:], clientPort)
	return g
}

func (s *fakeServer) Recv(deadline time.Time) ([]byte, error) {
	if len(s.queue) == 0 {
		return nil, nil
	}
	f := s.queue[0]
	s.queue = s.queue[1:]
	return f, nil
}

func TestAcquire(t *testing.T) {
	srv := &fakeServer{
		hw:    net.HardwareAddr{0x52, 0x54, 0, 0x12, 0x34, 0x56},
		nDrop: 1,
		nak:   true,
		opts: map[byte][]byte{
			optSubnetMask:   {255, 255, 255, 0},
			optRouter:       {10, 0, 2, 2},
			optDNS:          {10, 0, 2, 3, 8, 8, 8, 8},
			optNTPServers:   {10, 0, 2, 4},
			optMTU:          {0x05, 0xb4},
			optLeaseTime:    {0, 1, 0x51, 0x80},
			optDomainName:   []byte("example.internal"),
			optDomainSearch: {3, 'f', 'o', 'o', 7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 0, 3, 'b', 'a', 'r', 0xc0, 4},
		},
	}
	c := &Client{Link: srv, XID: 100}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	l, err := c.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !l.Addr.Equal(net.IPv4(10, 0, 2, 15)) || l.Mask.String() != "ffffff00" {
		t.Errorf("addr %s mask %s", l.Addr, l.Mask)
	}
	if !l.Broadcast.Equal(net.IPv4(10, 0, 2, 255)) {
		t.Errorf("broadcast %s", l.Broadcast)
	}
	if len(l.Routes) != 1 || !l.Routes[0].Gateway.Equal(net.IPv4(10, 0, 2, 2)) {
		t.Errorf("routes %v", l.Routes)
	}
	if len(l.DNS) != 2 || l.MTU != 1460 || l.LeaseTime != 86400*time.Second || l.Domain != "example.internal" {
		t.Errorf("lease %+v", l)
	}
	if len(l.NTP) != 1 || !l.NTP[0].Equal(net.IPv4(10, 0, 2, 4)) {
		t.Errorf("NTP %v", l.NTP)
	}
	if want := []string{"foo.example", "bar.example"}; !reflect.DeepEqual(l.Search, want) {
		t.Errorf("search %v, want %v", l.Search, want)
	}
	// dropped DISCOVER, DISCOVER, REQUEST (NAK), DISCOVER, REQUEST (ACK)
	if len(srv.sent) != 5 {
		t.Errorf("client sent %d messages, want 5", len(srv.sent))
	}
	last := srv.sent[len(srv.sent)-1]
	if !net.IP(last.Options[optRequestedIP]).Equal(net.IPv4(10, 0, 2, 15)) {
		t.Errorf("REQUEST asked for %v", last.Options[optRequestedIP])
	}
}

func TestClasslessRoutesOverrideRouter(t *testing.T) {
	// GCE-style: a /32 address with an on-link route to the gateway.
	m := &Message{YIAddr: net.IPv4(10, 128, 0, 5), Options: map[byte][]byte{
		optSubnetMask:      {255, 255, 255, 255},
		optRouter:          {10, 128, 0, 99},
		optClasslessRoutes: {32, 10, 128, 0, 1, 0, 0, 0, 0, 0, 10, 128, 0, 1},
	}}
	l, err := leaseFrom(m)
	if err != nil {
		t.Fatal(err)
	}
	if len(l.Routes) != 2 {
		t.Fatalf("routes %v", l.Routes)
	}
	if l.Routes[0].Dst.String() != "10.128.0.1/32" || !l.Routes[0].Gateway.Equal(net.IPv4zero) {
		t.Errorf("route 0 = %v via %v", l.Routes[0].Dst, l.Routes[0].Gateway)
	}
	if l.Routes[1].Dst.String() != "0.0.0.0/0" || !l.Routes[1].Gateway.Equal(net.IPv4(10, 128, 0, 1)) {
		t.Errorf("route 1 = %v via %v", l.Routes[1].Dst, l.Routes[1].Gateway)
	}
}

func TestFrameChecksum(t *testing.T) {
	f := Frame(net.HardwareAddr{1, 2, 3, 4, 5, 6}, []byte("x"))
	if ipChecksum(f[14:34]) != 0 {
		t.Error("IPv4 header checksum does not verify")
	}
}
