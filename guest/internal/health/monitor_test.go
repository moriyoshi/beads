// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
)

func TestTracker(t *testing.T) {
	tr := tracker{healthy: 2, unhealthy: 3}
	var got []Status
	for _, c := range "ffttftfffttt" {
		got = append(got, tr.observe(c == 't'))
	}
	H, U := Healthy, Unhealthy
	want := []Status{"", "", "", H, H, H, H, H, U, U, H, H}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v\nwant %v", got, want)
	}
}

// script plays back probe results, then repeats the last one.
type script struct {
	mu      sync.Mutex
	results string
	n       int
}

func (s *script) Probe(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.results[min(s.n, len(s.results)-1)]
	s.n++
	if c == 't' {
		return nil
	}
	return errors.New("connection refused")
}

func (s *script) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.n
}

type recorder struct {
	mu      sync.Mutex
	got     []Status
	failing int // fail this many calls first
}

func (r *recorder) Report(_ context.Context, s Status) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.failing > 0 {
		r.failing--
		return errors.New("AccessDeniedException")
	}
	r.got = append(r.got, s)
	return nil
}

func (r *recorder) reports() []Status {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Status(nil), r.got...)
}

type logs struct {
	mu    sync.Mutex
	lines []string
}

func (l *logs) logf(f string, a ...any) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.lines = append(l.lines, fmt.Sprintf(f, a...))
}

func (l *logs) count(substr string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	n := 0
	for _, s := range l.lines {
		if strings.Contains(s, substr) {
			n++
		}
	}
	return n
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
		if cond() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestMonitor(t *testing.T) {
	p := &script{results: "ffttftffff"}
	r := &recorder{}
	l := &logs{}
	reader := sdkmetric.NewManualReader()
	m := &Monitor{
		Prober: p, Reporter: r, Interval: time.Millisecond,
		HealthyThreshold: 2, UnhealthyThreshold: 3,
		Logf: l.logf, Meter: sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)).Meter("test"),
	}
	m.Start()
	waitFor(t, "UNHEALTHY", func() bool { return len(r.reports()) == 2 })
	if got, want := r.reports(), []Status{Healthy, Unhealthy}; !reflect.DeepEqual(got, want) {
		t.Errorf("reports %v, want %v", got, want)
	}
	if m.Status() != Unhealthy {
		t.Errorf("status %s", m.Status())
	}
	// Repeated identical failures are logged once per streak.
	if n := l.count("probe failed: connection refused"); n != 3 {
		t.Errorf("logged %d probe failures, want 3:\n%s", n, strings.Join(l.lines, "\n"))
	}

	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			names[md.Name] = true
			if md.Name == "beads.health.status" {
				if v := md.Data.(metricdata.Gauge[int64]).DataPoints[0].Value; v != 0 {
					t.Errorf("beads.health.status = %d", v)
				}
			}
		}
	}
	for _, n := range []string{"beads.health.probes", "beads.health.reports", "beads.health.status"} {
		if !names[n] {
			t.Errorf("no %s metric", n)
		}
	}

	m.Stop(context.Background())
	m.Stop(context.Background())
	if got := r.reports(); len(got) != 3 || got[2] != Unhealthy {
		t.Errorf("after Stop: %v", got)
	}
	n := p.count()
	time.Sleep(10 * time.Millisecond)
	if p.count() != n {
		t.Error("still probing after Stop")
	}
}

func TestMonitorRetriesAndReasserts(t *testing.T) {
	p := &script{results: "t"}
	r := &recorder{failing: 3}
	l := &logs{}
	m := &Monitor{
		Prober: p, Reporter: r, Interval: time.Millisecond, Reassert: 20 * time.Millisecond,
		HealthyThreshold: 1, UnhealthyThreshold: 1, Logf: l.logf,
	}
	m.Start()
	waitFor(t, "reassertion", func() bool { return len(r.reports()) >= 3 })
	m.Stop(context.Background())
	for i, s := range r.reports()[:3] {
		if s != Healthy {
			t.Errorf("report %d: %s", i, s)
		}
	}
	// One log line for the three identical failures, one for the
	// recovery; reassertions are quiet.
	if n := l.count("reporting HEALTHY: AccessDeniedException"); n != 1 {
		t.Errorf("logged %d report failures", n)
	}
	if n := l.count("reported HEALTHY"); n != 1 {
		t.Errorf("logged %d successful reports", n)
	}
}

type panicky struct{ n int }

func (p *panicky) Probe(context.Context) error {
	p.n++
	if p.n == 1 {
		panic("boom")
	}
	return nil
}

func TestMonitorSurvivesPanic(t *testing.T) {
	r := &recorder{}
	l := &logs{}
	m := &Monitor{Prober: &panicky{}, Reporter: r, Interval: time.Millisecond, HealthyThreshold: 1, UnhealthyThreshold: 1, Logf: l.logf}
	m.Start()
	waitFor(t, "a report after the panic", func() bool { return len(r.reports()) > 0 })
	m.Stop(context.Background())
	if l.count("internal error: boom") != 1 {
		t.Error("the panic was not logged")
	}
}
