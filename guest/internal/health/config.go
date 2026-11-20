// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package health probes a local service and reports the result to AWS Cloud
// Map as the custom health status of the instance's registration, so that a
// service that stops answering drops out of the Cloud Map DNS records.
package health

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// User-data settings. They are settings for init, not variables for the
// program. Health reporting is off unless ServiceIDKey is set.
const (
	ServiceIDKey          = "APPLIANCE_CLOUDMAP_SERVICE_ID"
	InstanceIDKey         = "APPLIANCE_CLOUDMAP_INSTANCE_ID"
	CheckKey              = "APPLIANCE_HEALTH_CHECK"
	HTTPStatusKey         = "APPLIANCE_HEALTH_HTTP_STATUS"
	IntervalKey           = "APPLIANCE_HEALTH_INTERVAL"
	TimeoutKey            = "APPLIANCE_HEALTH_TIMEOUT"
	HealthyThresholdKey   = "APPLIANCE_HEALTH_HEALTHY_THRESHOLD"
	UnhealthyThresholdKey = "APPLIANCE_HEALTH_UNHEALTHY_THRESHOLD"
)

// Keys lists every setting above.
var Keys = []string{
	ServiceIDKey, InstanceIDKey, CheckKey, HTTPStatusKey, IntervalKey,
	TimeoutKey, HealthyThresholdKey, UnhealthyThresholdKey,
}

// Defaults.
const (
	DefaultHTTPStatus         = "200-399"
	DefaultInterval           = 10 * time.Second
	DefaultTimeout            = 3 * time.Second
	DefaultHealthyThreshold   = 2
	DefaultUnhealthyThreshold = 3
)

// Config is the parsed form of the settings.
type Config struct {
	ServiceID  string
	InstanceID string
	// Check is the probe target: "tcp://host:port" (or a bare
	// "host:port") or "http://host:port/path".
	Check string
	// HTTPStatus lists the HTTP status codes that count as healthy, as
	// comma-separated codes or ranges ("200", "200-299,404").
	HTTPStatus         string
	Interval, Timeout  time.Duration
	HealthyThreshold   int
	UnhealthyThreshold int
}

// ParseConfig reads the settings from env. It returns nil, nil when
// ServiceIDKey is unset. defaultInstanceID (the EC2 instance ID) stands in
// for an unset InstanceIDKey.
func ParseConfig(env map[string]string, defaultInstanceID string) (*Config, error) {
	get := func(k string) string { return strings.TrimSpace(env[k]) }
	c := &Config{
		ServiceID:          get(ServiceIDKey),
		InstanceID:         get(InstanceIDKey),
		Check:              get(CheckKey),
		HTTPStatus:         get(HTTPStatusKey),
		Interval:           DefaultInterval,
		Timeout:            DefaultTimeout,
		HealthyThreshold:   DefaultHealthyThreshold,
		UnhealthyThreshold: DefaultUnhealthyThreshold,
	}
	if c.ServiceID == "" {
		return nil, nil
	}
	if c.InstanceID == "" {
		c.InstanceID = defaultInstanceID
	}
	if c.InstanceID == "" {
		return nil, fmt.Errorf("%s: no instance ID known; set %s", ServiceIDKey, InstanceIDKey)
	}
	if c.Check == "" {
		return nil, fmt.Errorf("%s needs %s", ServiceIDKey, CheckKey)
	}
	if c.HTTPStatus == "" {
		c.HTTPStatus = DefaultHTTPStatus
	}
	if _, err := NewProber(c.Check, c.HTTPStatus, time.Second); err != nil {
		return nil, err
	}
	for _, d := range []struct {
		key string
		dst *time.Duration
	}{{IntervalKey, &c.Interval}, {TimeoutKey, &c.Timeout}} {
		if s := get(d.key); s != "" {
			v, err := parseDuration(s)
			if err != nil {
				return nil, fmt.Errorf("%s: %w", d.key, err)
			}
			*d.dst = v
		}
	}
	if c.Interval < time.Second {
		return nil, fmt.Errorf("%s: %v is under a second", IntervalKey, c.Interval)
	}
	if c.Timeout > c.Interval {
		if get(TimeoutKey) != "" {
			return nil, fmt.Errorf("%s: %v is longer than %s (%v)", TimeoutKey, c.Timeout, IntervalKey, c.Interval)
		}
		c.Timeout = c.Interval
	}
	for _, n := range []struct {
		key string
		dst *int
	}{{HealthyThresholdKey, &c.HealthyThreshold}, {UnhealthyThresholdKey, &c.UnhealthyThreshold}} {
		if s := get(n.key); s != "" {
			v, err := strconv.Atoi(s)
			if err != nil || v < 1 || v > 100 {
				return nil, fmt.Errorf("%s: %q is not a number from 1 to 100", n.key, s)
			}
			*n.dst = v
		}
	}
	return c, nil
}

// parseDuration takes a Go duration ("10s", "1m30s") or a whole number of
// seconds, as JSON user-data gives numbers.
func parseDuration(s string) (time.Duration, error) {
	if n, err := strconv.Atoi(s); err == nil {
		if n <= 0 {
			return 0, fmt.Errorf("%q is not positive", s)
		}
		return time.Duration(n) * time.Second, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("%q is not a duration such as 10s", s)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%q is not positive", s)
	}
	return d, nil
}

// String summarizes the configuration for the console.
func (c *Config) String() string {
	s := fmt.Sprintf("Cloud Map service %s instance %s; probe %s every %v (timeout %v)",
		c.ServiceID, c.InstanceID, c.Check, c.Interval, c.Timeout)
	if strings.HasPrefix(c.Check, "http://") {
		s += " expecting " + c.HTTPStatus
	}
	return s + fmt.Sprintf("; healthy after %d, unhealthy after %d", c.HealthyThreshold, c.UnhealthyThreshold)
}
