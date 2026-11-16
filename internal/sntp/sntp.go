// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package sntp keeps the system clock in step with NTP servers (RFC 4330
// client mode). It is a small replacement for ntpd in an image that has no
// userland: the first synchronization steps the clock, later ones slew it
// with adjtime(2) so that time never jumps backwards under a running program.
package sntp

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	packetLen = 48
	// Seconds from the NTP epoch (1900) to the Unix epoch (1970).
	ntpUnixOffset = 2208988800
	defaultPort   = "123"
)

// Sample is one server's answer.
type Sample struct {
	Server  string
	Offset  time.Duration // add this to the local clock to get the server's time
	Delay   time.Duration // round-trip network delay
	Stratum int
}

// toNTP encodes t as a 64-bit NTP timestamp. Times from 2036 on wrap into
// era 1, as the format requires.
func toNTP(t time.Time) uint64 {
	sec := uint64(t.Unix() + ntpUnixOffset)
	frac := uint64(t.Nanosecond()) << 32 / 1e9
	return sec<<32 | frac
}

// fromNTP decodes a timestamp, choosing the era (1900 or 2036) closest to
// ref.
func fromNTP(v uint64, ref time.Time) time.Time {
	sec := int64(v >> 32)
	nsec := int64((v & 0xffffffff) * 1e9 >> 32)
	t := time.Unix(sec-ntpUnixOffset, nsec)
	const era = int64(1) << 32
	for t.Sub(ref) < -68*365*24*time.Hour {
		sec += era
		t = time.Unix(sec-ntpUnixOffset, nsec)
	}
	return t
}

// hostPort adds the NTP port to a server address that lacks one.
func hostPort(server string) string {
	if _, _, err := net.SplitHostPort(server); err == nil {
		return server
	}
	return net.JoinHostPort(strings.Trim(server, "[]"), defaultPort)
}

// Query asks one server for the time. now is the local clock.
func Query(ctx context.Context, server string, now func() time.Time) (Sample, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "udp", hostPort(server))
	if err != nil {
		return Sample{}, err
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(5 * time.Second)
	}
	conn.SetDeadline(deadline)

	req := make([]byte, packetLen)
	req[0] = 0<<6 | 4<<3 | 3 // no leap warning, version 4, client mode
	t1 := now()
	xmit := toNTP(t1)
	binary.BigEndian.PutUint64(req[40:], xmit)
	if _, err := conn.Write(req); err != nil {
		return Sample{}, err
	}
	resp := make([]byte, 128)
	for {
		n, err := conn.Read(resp)
		if err != nil {
			return Sample{}, err
		}
		t4 := now()
		if n < packetLen {
			continue
		}
		r := resp[:n]
		mode := r[0] & 7
		// Replies must answer our request: the server echoes our transmit
		// timestamp as its originate timestamp.
		if mode != 4 || binary.BigEndian.Uint64(r[24:]) != xmit {
			continue
		}
		if r[0]>>6 == 3 {
			return Sample{}, fmt.Errorf("%s: server clock not synchronized", server)
		}
		stratum := int(r[1])
		if stratum == 0 {
			return Sample{}, fmt.Errorf("%s: kiss-o'-death %q", server, r[12:16])
		}
		t2 := fromNTP(binary.BigEndian.Uint64(r[32:]), t1)
		t3 := fromNTP(binary.BigEndian.Uint64(r[40:]), t1)
		return Sample{
			Server:  server,
			Offset:  (t2.Sub(t1) + t3.Sub(t4)) / 2,
			Delay:   t4.Sub(t1) - t3.Sub(t2),
			Stratum: stratum,
		}, nil
	}
}

// Clock is the system clock the Syncer adjusts.
type Clock interface {
	Now() time.Time
	// Step sets the clock forward or back by d at once.
	Step(d time.Duration) error
	// Slew asks the kernel to absorb d gradually.
	Slew(d time.Duration) error
}

// Status describes the last synchronization attempt.
type Status struct {
	Time   time.Time // local time after the adjustment
	Sample Sample
	Action string // "stepped", "slewed" or "failed"
	Err    error
}

func (s Status) String() string {
	if s.Err != nil {
		return fmt.Sprintf("failed at %s: %v", s.Time.UTC().Format(time.RFC3339), s.Err)
	}
	return fmt.Sprintf("%s %s from %s (stratum %d, delay %v) at %s",
		s.Action, signed(s.Sample.Offset.Round(time.Microsecond)), s.Sample.Server, s.Sample.Stratum,
		s.Sample.Delay.Round(time.Microsecond), s.Time.UTC().Format(time.RFC3339))
}

func signed(d time.Duration) string {
	if d >= 0 {
		return "+" + d.String()
	}
	return d.String()
}

// Syncer keeps a Clock in step with a list of servers.
type Syncer struct {
	Servers []string
	Clock   Clock
	// StepThreshold is the offset above which the clock is stepped rather
	// than slewed. The first synchronization always steps. Default 0.5s.
	StepThreshold time.Duration
	// MinPoll and MaxPoll bound the polling interval, which doubles while
	// the clock stays within 10ms. Defaults 64s and 1024s.
	MinPoll, MaxPoll time.Duration
	// QueryTimeout bounds each server query (default 5s).
	QueryTimeout time.Duration
	// Query defaults to the package's Query.
	Query func(ctx context.Context, server string, now func() time.Time) (Sample, error)
	Logf  func(format string, args ...any)
	// OnStatus, if set, is called after every attempt.
	OnStatus func(Status)

	synced bool
}

func (s *Syncer) logf(format string, args ...any) {
	if s.Logf != nil {
		s.Logf(format, args...)
	}
}

// sample queries every server concurrently and returns the answer with the
// lowest delay, which has the smallest error bound.
func (s *Syncer) sample(ctx context.Context) (Sample, error) {
	q := s.Query
	if q == nil {
		q = Query
	}
	var (
		mu      sync.Mutex
		samples []Sample
		errs    []error
		wg      sync.WaitGroup
	)
	for _, srv := range s.Servers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			timeout := s.QueryTimeout
			if timeout == 0 {
				timeout = 5 * time.Second
			}
			qctx, cancel := context.WithTimeout(ctx, timeout)
			defer cancel()
			smp, err := q(qctx, srv, s.Clock.Now)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				errs = append(errs, fmt.Errorf("%s: %w", srv, err))
				return
			}
			samples = append(samples, smp)
		}()
	}
	wg.Wait()
	if len(samples) == 0 {
		if len(errs) == 0 {
			return Sample{}, errors.New("no time servers configured")
		}
		return Sample{}, errors.Join(errs...)
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i].Delay < samples[j].Delay })
	return samples[0], nil
}

// SyncOnce takes one sample and applies it.
func (s *Syncer) SyncOnce(ctx context.Context) Status {
	threshold := s.StepThreshold
	if threshold == 0 {
		threshold = 500 * time.Millisecond
	}
	smp, err := s.sample(ctx)
	if err != nil {
		st := Status{Time: s.Clock.Now(), Action: "failed", Err: err}
		s.report(st)
		return st
	}
	off := smp.Offset
	action := "slewed"
	if !s.synced || off > threshold || off < -threshold {
		action = "stepped"
		err = s.Clock.Step(off)
	} else {
		err = s.Clock.Slew(off)
	}
	st := Status{Time: s.Clock.Now(), Sample: smp, Action: action, Err: err}
	if err != nil {
		st.Action = "failed"
	} else {
		s.synced = true
	}
	s.report(st)
	return st
}

func (s *Syncer) report(st Status) {
	// Routine slews are not worth a console line each.
	if st.Action != "slewed" || st.Sample.Offset > 100*time.Millisecond || st.Sample.Offset < -100*time.Millisecond {
		s.logf("time: %s", st)
	}
	if s.OnStatus != nil {
		s.OnStatus(st)
	}
}

// Run synchronizes until ctx ends, backing off while the clock is stable and
// polling quickly again after failures or large corrections.
func (s *Syncer) Run(ctx context.Context) {
	minPoll, maxPoll := s.MinPoll, s.MaxPoll
	if minPoll == 0 {
		minPoll = 64 * time.Second
	}
	if maxPoll == 0 {
		maxPoll = 1024 * time.Second
	}
	poll := minPoll
	for {
		select {
		case <-ctx.Done():
			return
		case <-time.After(poll):
		}
		st := s.SyncOnce(ctx)
		off := st.Sample.Offset
		switch {
		case st.Err != nil, st.Action == "stepped":
			poll = minPoll
		case off < 10*time.Millisecond && off > -10*time.Millisecond:
			poll = min(2*poll, maxPoll)
		}
	}
}
