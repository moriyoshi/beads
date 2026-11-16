// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package sntp

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func TestTimestampRoundTrip(t *testing.T) {
	for _, tm := range []time.Time{
		time.Date(2026, 11, 15, 12, 0, 0, 123456789, time.UTC),
		time.Date(2040, 1, 1, 0, 0, 0, 0, time.UTC), // NTP era 1
		time.Date(2001, 2, 3, 4, 5, 6, 0, time.UTC),
	} {
		got := fromNTP(toNTP(tm), tm)
		if d := got.Sub(tm); d < -time.Microsecond || d > time.Microsecond {
			t.Errorf("%v round-trips to %v", tm, got)
		}
	}
}

func startServer(t *testing.T, now func() time.Time) string {
	t.Helper()
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	go Serve(conn, now)
	return conn.LocalAddr().String()
}

func TestQueryMeasuresOffset(t *testing.T) {
	srv := startServer(t, time.Now)
	// A local clock that is 25 years slow, as in a VM with a bad RTC.
	skew := -25 * 365 * 24 * time.Hour
	local := func() time.Time { return time.Now().Add(skew) }
	s, err := Query(context.Background(), srv, local)
	if err != nil {
		t.Fatal(err)
	}
	if d := s.Offset + skew; d < -50*time.Millisecond || d > 50*time.Millisecond {
		t.Errorf("offset %v, want about %v", s.Offset, -skew)
	}
	if s.Stratum != 1 || s.Delay < 0 || s.Delay > time.Second {
		t.Errorf("sample %+v", s)
	}
}

func TestQueryIgnoresStrayReplies(t *testing.T) {
	// A server that first sends a reply for some other request.
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	go func() {
		buf := make([]byte, 512)
		_, addr, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		stray := make([]byte, packetLen)
		stray[0], stray[1] = 0x24, 1
		conn.WriteTo(stray, addr) // wrong originate timestamp
		real := make([]byte, packetLen)
		real[0], real[1] = 0x24, 2
		copy(real[24:32], buf[40:48])
		copy(real[32:40], buf[40:48])
		copy(real[40:48], buf[40:48])
		conn.WriteTo(real, addr)
	}()
	s, err := Query(context.Background(), conn.LocalAddr().String(), time.Now)
	if err != nil {
		t.Fatal(err)
	}
	if s.Stratum != 2 {
		t.Errorf("accepted the stray reply: %+v", s)
	}
}

func TestHostPort(t *testing.T) {
	for in, want := range map[string]string{
		"169.254.169.123": "169.254.169.123:123",
		"10.0.2.2:11123":  "10.0.2.2:11123",
		"time.example":    "time.example:123",
		"[fd00::1]":       "[fd00::1]:123",
	} {
		if got := hostPort(in); got != want {
			t.Errorf("hostPort(%q) = %q, want %q", in, got, want)
		}
	}
}

// fakeClock records adjustments.
type fakeClock struct {
	off   time.Duration
	steps []time.Duration
	slews []time.Duration
}

func (c *fakeClock) Now() time.Time             { return time.Now().Add(c.off) }
func (c *fakeClock) Step(d time.Duration) error { c.steps = append(c.steps, d); c.off += d; return nil }
func (c *fakeClock) Slew(d time.Duration) error { c.slews = append(c.slews, d); return nil }

func TestSyncerStepsThenSlews(t *testing.T) {
	offsets := []time.Duration{3 * time.Second, 20 * time.Millisecond, 2 * time.Second}
	clk := &fakeClock{}
	var statuses []Status
	s := &Syncer{
		Servers: []string{"a"},
		Clock:   clk,
		Query: func(ctx context.Context, srv string, now func() time.Time) (Sample, error) {
			o := offsets[0]
			offsets = offsets[1:]
			return Sample{Server: srv, Offset: o, Stratum: 2}, nil
		},
		OnStatus: func(st Status) { statuses = append(statuses, st) },
	}
	for range 3 {
		s.SyncOnce(context.Background())
	}
	if len(clk.steps) != 2 || clk.steps[0] != 3*time.Second || clk.steps[1] != 2*time.Second {
		t.Errorf("steps %v; want the first sync and the 2s offset stepped", clk.steps)
	}
	if len(clk.slews) != 1 || clk.slews[0] != 20*time.Millisecond {
		t.Errorf("slews %v", clk.slews)
	}
	if statuses[0].Action != "stepped" || statuses[1].Action != "slewed" {
		t.Errorf("statuses %v", statuses)
	}
	if !strings.Contains(statuses[0].String(), "stepped +3s from a") {
		t.Errorf("status string %q", statuses[0])
	}
}

func TestSyncerPicksLowestDelay(t *testing.T) {
	clk := &fakeClock{}
	s := &Syncer{
		Servers: []string{"slow", "fast", "down"},
		Clock:   clk,
		Query: func(ctx context.Context, srv string, now func() time.Time) (Sample, error) {
			switch srv {
			case "slow":
				return Sample{Server: srv, Offset: time.Second, Delay: 200 * time.Millisecond, Stratum: 2}, nil
			case "fast":
				return Sample{Server: srv, Offset: 2 * time.Second, Delay: time.Millisecond, Stratum: 2}, nil
			}
			return Sample{}, errors.New("timeout")
		},
	}
	st := s.SyncOnce(context.Background())
	if st.Err != nil || st.Sample.Server != "fast" || clk.steps[0] != 2*time.Second {
		t.Errorf("status %+v, steps %v", st, clk.steps)
	}
}

func TestSyncerAllFail(t *testing.T) {
	clk := &fakeClock{}
	s := &Syncer{
		Servers: []string{"a", "b"},
		Clock:   clk,
		Query: func(context.Context, string, func() time.Time) (Sample, error) {
			return Sample{}, errors.New("unreachable")
		},
	}
	st := s.SyncOnce(context.Background())
	if st.Err == nil || st.Action != "failed" || len(clk.steps)+len(clk.slews) != 0 {
		t.Errorf("status %+v", st)
	}
}
