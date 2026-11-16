// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"strings"
	"testing"
	"time"
)

func TestParseConfigOff(t *testing.T) {
	c, err := ParseConfig(map[string]string{CheckKey: "127.0.0.1:1"}, "i-1")
	if c != nil || err != nil {
		t.Errorf("without %s: %v, %v", ServiceIDKey, c, err)
	}
}

func TestParseConfigDefaults(t *testing.T) {
	c, err := ParseConfig(map[string]string{ServiceIDKey: "srv-1", CheckKey: "tcp://127.0.0.1:4750"}, "i-0abc")
	if err != nil {
		t.Fatal(err)
	}
	want := Config{
		ServiceID: "srv-1", InstanceID: "i-0abc", Check: "tcp://127.0.0.1:4750", HTTPStatus: "200-399",
		Interval: 10 * time.Second, Timeout: 3 * time.Second, HealthyThreshold: 2, UnhealthyThreshold: 3,
	}
	if *c != want {
		t.Errorf("got %+v, want %+v", *c, want)
	}
}

func TestParseConfigSettings(t *testing.T) {
	c, err := ParseConfig(map[string]string{
		ServiceIDKey:          "srv-1",
		InstanceIDKey:         "proxy-a",
		CheckKey:              "http://127.0.0.1:8080/healthz",
		HTTPStatusKey:         "200,204",
		IntervalKey:           "30",
		TimeoutKey:            "1500ms",
		HealthyThresholdKey:   "1",
		UnhealthyThresholdKey: "5",
	}, "i-0abc")
	if err != nil {
		t.Fatal(err)
	}
	if c.InstanceID != "proxy-a" || c.Interval != 30*time.Second || c.Timeout != 1500*time.Millisecond ||
		c.HealthyThreshold != 1 || c.UnhealthyThreshold != 5 || c.HTTPStatus != "200,204" {
		t.Errorf("got %+v", *c)
	}
	// A short interval caps the default timeout.
	c, err = ParseConfig(map[string]string{ServiceIDKey: "s", CheckKey: "h:1", IntervalKey: "2s"}, "i")
	if err != nil || c.Timeout != 2*time.Second {
		t.Errorf("timeout %v, err %v", c.Timeout, err)
	}
}

func TestParseConfigErrors(t *testing.T) {
	base := func(kv ...string) map[string]string {
		m := map[string]string{ServiceIDKey: "srv-1", CheckKey: "127.0.0.1:4750"}
		for i := 0; i < len(kv); i += 2 {
			m[kv[i]] = kv[i+1]
		}
		return m
	}
	for name, tc := range map[string]struct {
		env  map[string]string
		id   string
		want string
	}{
		"no check":         {map[string]string{ServiceIDKey: "s"}, "i", CheckKey},
		"no instance":      {base(), "", InstanceIDKey},
		"bad check":        {base(CheckKey, "localhost"), "i", CheckKey},
		"bad port":         {base(CheckKey, "tcp://127.0.0.1:0"), "i", "port"},
		"https":            {base(CheckKey, "https://127.0.0.1/"), "i", "only tcp"},
		"bad status":       {base(CheckKey, "http://127.0.0.1/", HTTPStatusKey, "2xx"), "i", HTTPStatusKey},
		"bad interval":     {base(IntervalKey, "soon"), "i", IntervalKey},
		"tiny interval":    {base(IntervalKey, "10ms"), "i", IntervalKey},
		"zero interval":    {base(IntervalKey, "0"), "i", IntervalKey},
		"timeout>interval": {base(IntervalKey, "5", TimeoutKey, "6"), "i", TimeoutKey},
		"bad threshold":    {base(HealthyThresholdKey, "0"), "i", HealthyThresholdKey},
		"bad threshold 2":  {base(UnhealthyThresholdKey, "x"), "i", UnhealthyThresholdKey},
	} {
		_, err := ParseConfig(tc.env, tc.id)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want one mentioning %s", name, err, tc.want)
		}
	}
}
