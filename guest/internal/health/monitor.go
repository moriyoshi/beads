// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"context"
	"fmt"
	"runtime/debug"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"
)

// Status is a Cloud Map custom health status.
type Status string

const (
	Healthy   Status = "HEALTHY"
	Unhealthy Status = "UNHEALTHY"
)

// Reporter publishes the status.
type Reporter interface {
	Report(ctx context.Context, s Status) error
}

// DefaultReassert is how often a Monitor re-sends an unchanged status.
const DefaultReassert = 5 * time.Minute

// tracker applies the thresholds: the status changes only after that many
// consecutive probes disagree with it. It is "" until the first decision.
type tracker struct {
	healthy, unhealthy int
	streak             int // consecutive probes with the result of last
	last               bool
	status             Status
}

// observe records one probe result and returns the status.
func (t *tracker) observe(ok bool) Status {
	if t.streak > 0 && ok == t.last {
		t.streak++
	} else {
		t.last, t.streak = ok, 1
	}
	switch {
	case ok && t.streak >= t.healthy:
		t.status = Healthy
	case !ok && t.streak >= t.unhealthy:
		t.status = Unhealthy
	}
	return t.status
}

// Monitor probes the service every Interval and reports the status after
// each change, and again every Reassert, retrying failed reports at the next
// probe. Nothing it does stops the appliance: errors are logged and retried.
type Monitor struct {
	Prober             Prober
	Reporter           Reporter
	Interval           time.Duration
	HealthyThreshold   int
	UnhealthyThreshold int
	// Reassert defaults to DefaultReassert. Re-sending recovers from a
	// lost report and from a registration that was recreated, which
	// resets its status.
	Reassert time.Duration
	Logf     func(format string, args ...any)
	// Meter receives the beads.health.* metrics; nil means none.
	Meter metric.Meter

	cancel   context.CancelFunc
	done     chan struct{}
	stopOnce sync.Once

	mu     sync.Mutex
	status Status // as decided by the probes

	probes, reports metric.Int64Counter
}

// NewMonitor builds a Monitor that reports to Cloud Map with the settings in
// c. It does not start it.
func NewMonitor(c *Config, r Reporter) (*Monitor, error) {
	p, err := NewProber(c.Check, c.HTTPStatus, c.Timeout)
	if err != nil {
		return nil, err
	}
	return &Monitor{
		Prober:             p,
		Reporter:           r,
		Interval:           c.Interval,
		HealthyThreshold:   c.HealthyThreshold,
		UnhealthyThreshold: c.UnhealthyThreshold,
	}, nil
}

func (m *Monitor) logf(format string, args ...any) {
	if m.Logf != nil {
		m.Logf("health: "+format, args...)
	}
}

// Status returns the status the probes last decided on, or "".
func (m *Monitor) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.status
}

func (m *Monitor) instruments() {
	meter := m.Meter
	if meter == nil {
		meter = noop.NewMeterProvider().Meter("")
	}
	var err error
	if m.probes, err = meter.Int64Counter("beads.health.probes",
		metric.WithDescription("Health probes of the local service, by result")); err != nil {
		m.logf("metrics: %v", err)
		m.probes, _ = noop.NewMeterProvider().Meter("").Int64Counter("")
	}
	if m.reports, err = meter.Int64Counter("beads.health.reports",
		metric.WithDescription("Health status reports to Cloud Map, by status and result")); err != nil {
		m.logf("metrics: %v", err)
		m.reports, _ = noop.NewMeterProvider().Meter("").Int64Counter("")
	}
	_, err = meter.Int64ObservableGauge("beads.health.status",
		metric.WithDescription("1 when the local service is healthy, 0 when unhealthy; absent until decided"),
		metric.WithInt64Callback(func(_ context.Context, o metric.Int64Observer) error {
			switch m.Status() {
			case Healthy:
				o.Observe(1)
			case Unhealthy:
				o.Observe(0)
			}
			return nil
		}))
	if err != nil {
		m.logf("metrics: %v", err)
	}
}

// Start runs the monitor in the background until Stop.
func (m *Monitor) Start() {
	m.instruments()
	ctx, cancel := context.WithCancel(context.Background())
	m.cancel = cancel
	m.done = make(chan struct{})
	go func() {
		defer close(m.done)
		for ctx.Err() == nil {
			if m.runSafely(ctx) {
				return
			}
			// It panicked; start over after a pause rather than
			// stop reporting for good.
			select {
			case <-ctx.Done():
			case <-time.After(m.Interval):
			}
		}
	}()
}

// runSafely runs the loop, turning a panic, which in PID 1 would panic the
// kernel, into a log message. It reports whether the loop ended normally.
func (m *Monitor) runSafely(ctx context.Context) (ok bool) {
	defer func() {
		if r := recover(); r != nil {
			m.logf("internal error: %v\n%s", r, debug.Stack())
		}
	}()
	m.run(ctx)
	return true
}

func (m *Monitor) run(ctx context.Context) {
	t := tracker{healthy: m.HealthyThreshold, unhealthy: m.UnhealthyThreshold}
	reassert := m.Reassert
	if reassert <= 0 {
		reassert = DefaultReassert
	}
	var (
		reported   Status // last status Cloud Map accepted
		reportedAt time.Time
		probeErr   = "" // last probe error, "" after a success
		reportErr  = "" // last report error, "" after a success
	)
	tick := time.NewTicker(m.Interval)
	defer tick.Stop()
	for {
		// Probe.
		err := m.Prober.Probe(ctx)
		if ctx.Err() != nil {
			return
		}
		result := "success"
		if err != nil {
			result = "failure"
			if msg := err.Error(); msg != probeErr {
				m.logf("probe failed: %v", err)
				probeErr = msg
			}
		} else if probeErr != "" {
			m.logf("probe succeeded")
			probeErr = ""
		}
		m.probes.Add(ctx, 1, metric.WithAttributes(attribute.String("result", result)))
		prev := m.Status()
		st := t.observe(err == nil)
		if st != prev {
			m.mu.Lock()
			m.status = st
			m.mu.Unlock()
			if prev == "" {
				m.logf("service is %s", st)
			} else {
				m.logf("service is %s (was %s)", st, prev)
			}
		}

		// Report.
		if st != "" && (st != reported || time.Since(reportedAt) >= reassert) {
			if err := m.report(ctx, st); err != nil {
				if ctx.Err() != nil {
					return
				}
				if msg := err.Error(); msg != reportErr {
					m.logf("reporting %s: %v; retrying every %v", st, err, m.Interval)
					reportErr = msg
				}
			} else {
				if st != reported || reportErr != "" {
					m.logf("reported %s", st)
				}
				reported, reportedAt, reportErr = st, time.Now(), ""
			}
		}

		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

func (m *Monitor) report(ctx context.Context, st Status) (err error) {
	ctx, cancel := context.WithTimeout(ctx, max(m.Interval, 10*time.Second))
	defer cancel()
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("internal error: %v", r)
		}
		result := "ok"
		if err != nil {
			result = "error"
		}
		m.reports.Add(context.Background(), 1, metric.WithAttributes(
			attribute.String("status", string(st)), attribute.String("result", result)))
	}()
	return m.Reporter.Report(ctx, st)
}

// Stop ends probing and, as the service is about to go away, reports
// UNHEALTHY, giving up when ctx ends. Later calls do nothing.
func (m *Monitor) Stop(ctx context.Context) {
	m.stopOnce.Do(func() {
		if m.cancel == nil {
			return // never started
		}
		m.cancel()
		select {
		case <-m.done:
		case <-ctx.Done():
		}
		m.mu.Lock()
		m.status = Unhealthy
		m.mu.Unlock()
		if err := m.report(ctx, Unhealthy); err != nil {
			m.logf("reporting %s on shutdown: %v", Unhealthy, err)
			return
		}
		m.logf("reported %s on shutdown", Unhealthy)
	})
}
