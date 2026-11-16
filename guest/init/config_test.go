// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"net"
	"reflect"
	"strings"
	"testing"
)

func TestParseConfigDefaults(t *testing.T) {
	c, err := parseConfig([]byte(`{"exec":"/app/server","args":["-x"],"env":{"PATH":"/app","A":"b"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Network != "dhcp" || c.OnExit != "restart" || c.RestartDelaySec != 1 || c.StopTimeoutSec != 10 {
		t.Errorf("defaults not applied: %+v", c)
	}
	c.addPlatformEnv(map[string]string{"APPLIANCE_PLATFORM": "ec2", "A": "platform"})
	want := []string{"A=b", "APPLIANCE_PLATFORM=ec2", "HOME=/", "PATH=/app", "TMPDIR=/tmp"}
	if got := c.environ(); !reflect.DeepEqual(got, want) {
		t.Errorf("environ() = %v, want %v", got, want)
	}
}

func TestUserDataEnv(t *testing.T) {
	c, err := parseConfig([]byte(`{"exec":"/app/server","env":{"PATH":"/app","A":"build","B":"build"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !c.useUserDataEnv() {
		t.Error("user-data env is off by default")
	}
	c.addPlatformEnv(map[string]string{"APPLIANCE_PLATFORM": "ec2", "OTEL_SERVICE_NAME": "server"})
	names, refused, err := c.setUserEnv([]byte("A=launch\nC='x y'\nPATH=/evil\nAPPLIANCE_PLATFORM=gce\nOTEL_SERVICE_NAME=evil\n"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"A", "C"}; !reflect.DeepEqual(names, want) {
		t.Errorf("names = %v, want %v", names, want)
	}
	if want := []string{"APPLIANCE_PLATFORM", "OTEL_SERVICE_NAME", "PATH"}; !reflect.DeepEqual(refused, want) {
		t.Errorf("refused = %v, want %v", refused, want)
	}
	want := []string{"A=launch", "APPLIANCE_PLATFORM=ec2", "B=build", "C=x y", "HOME=/", "OTEL_SERVICE_NAME=server", "PATH=/app", "TMPDIR=/tmp"}
	if got := c.environ(); !reflect.DeepEqual(got, want) {
		t.Errorf("environ() = %v, want %v", got, want)
	}

	if _, _, err := c.setUserEnv([]byte("#!/bin/sh\necho hi\n")); err == nil {
		t.Error("accepted a shell script")
	}
	names, refused, err = c.setUserEnv([]byte(`[{"key":"A","value":"json"},{"key":"OTEL_X","value":"y"}]`))
	if err != nil || !reflect.DeepEqual(names, []string{"A"}) || !reflect.DeepEqual(refused, []string{"OTEL_X"}) {
		t.Errorf("JSON pairs: names %v, refused %v, err %v", names, refused, err)
	}

	c, err = parseConfig([]byte(`{"exec":"/a","user_data_env":false}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.useUserDataEnv() {
		t.Error("user_data_env false not honored")
	}
}

func TestCollectorEnviron(t *testing.T) {
	beads := []string{"BEADS_PLATFORM=ec2"}
	ud := []byte(`{"LOG_GROUP":"g","BEADS_PLATFORM":"evil","HOME":"/evil","EMPTY":""}`)
	c, err := parseConfig([]byte(`{"exec":"/a"}`))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := c.setUserEnv(ud); err != nil {
		t.Fatal(err)
	}
	// Off by default: the collector sees only the fixed entries.
	env, names := c.collectorEnviron(beads)
	if want := []string{"HOME=/", "PATH=/sbin", "TMPDIR=/tmp", "BEADS_PLATFORM=ec2"}; !reflect.DeepEqual(env, want) || names != nil {
		t.Errorf("default: env %v, names %v", env, names)
	}

	c, err = parseConfig([]byte(`{"exec":"/a","otel":{"user_data_env":true}}`))
	if err != nil {
		t.Fatal(err)
	}
	// Without user-data nothing is added, so ${env:X:-default} applies.
	if env, _ := c.collectorEnviron(beads); len(env) != 4 {
		t.Errorf("no user-data: env %v", env)
	}
	if _, _, err := c.setUserEnv(ud); err != nil {
		t.Fatal(err)
	}
	env, names = c.collectorEnviron(beads)
	want := []string{"HOME=/", "PATH=/sbin", "TMPDIR=/tmp", "BEADS_PLATFORM=ec2", "EMPTY=", "LOG_GROUP=g"}
	if !reflect.DeepEqual(env, want) || !reflect.DeepEqual(names, []string{"EMPTY", "LOG_GROUP"}) {
		t.Errorf("env %v, names %v; want %v", env, names, want)
	}
}

func TestLocalAddrsFromUserData(t *testing.T) {
	c, err := parseConfig([]byte(`{"exec":"/a"}`))
	if err != nil {
		t.Fatal(err)
	}
	names, refused, err := c.setUserEnv([]byte(`{"EGRESS_PROXY_PORT":"4750","APPLIANCE_LOCAL_ADDRS":"192.168.255.1/32","APPLIANCE_X":"y"}`))
	if err != nil {
		t.Fatal(err)
	}
	// Init's setting, so neither a program variable nor refused.
	if !reflect.DeepEqual(names, []string{"EGRESS_PROXY_PORT"}) || !reflect.DeepEqual(refused, []string{"APPLIANCE_X"}) {
		t.Errorf("names %v, refused %v", names, refused)
	}
	if c.localAddrs != "192.168.255.1/32" {
		t.Errorf("localAddrs = %q", c.localAddrs)
	}

	// Still read with user_data_env off, which concerns the program only.
	c, err = parseConfig([]byte(`{"exec":"/a","user_data_env":false}`))
	if err != nil {
		t.Fatal(err)
	}
	names, _, err = c.setUserEnv([]byte("APPLIANCE_LOCAL_ADDRS=10.1.2.3\nA=b\n"))
	if err != nil || names != nil || c.userEnv != nil || c.localAddrs != "10.1.2.3" {
		t.Errorf("opted out: names %v, userEnv %v, localAddrs %q, err %v", names, c.userEnv, c.localAddrs, err)
	}
}

func TestParseLocalAddrs(t *testing.T) {
	for in, want := range map[string]string{
		"":                                "",
		"  ":                              "",
		"192.168.255.1/32":                "192.168.255.1",
		"192.168.255.1":                   "192.168.255.1",
		" 192.168.255.1/32 , 10.200.0.7 ": "192.168.255.1 10.200.0.7",
		"203.0.113.9/32,198.51.100.1/32":  "203.0.113.9 198.51.100.1",
	} {
		addrs, err := parseLocalAddrs(in)
		if err != nil {
			t.Errorf("%q: %v", in, err)
			continue
		}
		var got []string
		for _, a := range addrs {
			got = append(got, a.String())
		}
		if g := strings.Join(got, " "); g != want {
			t.Errorf("%q: got %q, want %q", in, g, want)
		}
	}
	for _, in := range []string{
		"192.168.255.1/24",
		"192.168.255.0/24",
		"192.168.255.1,",
		"192.168.255.1,,10.0.0.1",
		"192.168.255.1 10.0.0.1",
		"192.168.255.1;10.0.0.1",
		"192.168.255",
		"192.168.255.256",
		"example.com",
		"fd00::1",
		"fd00::1/128",
		"::ffff:192.168.255.1",
		"0.0.0.0",
		"127.0.0.2",
		"169.254.169.254",
		"224.0.0.1",
		"240.0.0.1",
		"255.255.255.255",
		"10.0.0.1,10.0.0.1/32",
	} {
		if addrs, err := parseLocalAddrs(in); err == nil {
			t.Errorf("%q: accepted as %v", in, addrs)
		}
	}
}

func TestParseConfigErrors(t *testing.T) {
	for _, in := range []string{
		`{"exec":"server"}`,
		`{"exec":"/a","network":"static"}`,
		`{"exec":"/a","on_exit":"explode"}`,
		`{"exec":"/a","unknown":1}`,
		`not json`,
	} {
		if _, err := parseConfig([]byte(in)); err == nil {
			t.Errorf("parseConfig(%s) succeeded", in)
		}
	}
}

func TestTimeServers(t *testing.T) {
	dhcpNTP := []net.IP{net.IPv4(10, 0, 0, 1), net.IPv4(169, 254, 169, 123)}
	for _, tc := range []struct {
		name     string
		cfg      string
		platform string
		md       []string
		dhcp     []net.IP
		want     []string
	}{
		{"ec2 plus dhcp, deduplicated", `{"exec":"/a"}`, "ec2", nil, dhcpNTP, []string{"169.254.169.123", "10.0.0.1"}},
		{"gce", `{"exec":"/a"}`, "gce", nil, nil, []string{"169.254.169.254"}},
		{"azure uses hvtimesync", `{"exec":"/a"}`, "azure", nil, nil, nil},
		{"qemu with fw_cfg and dhcp", `{"exec":"/a"}`, "qemu", []string{"10.0.2.2:12345"}, dhcpNTP[:1], []string{"10.0.2.2:12345", "10.0.0.1"}},
		{"explicit servers win", `{"exec":"/a","time_servers":["time.example"]}`, "ec2", []string{"x"}, dhcpNTP, []string{"time.example"}},
		{"off", `{"exec":"/a","timesync":"off","time_servers":["x"]}`, "ec2", nil, dhcpNTP, nil},
	} {
		c, err := parseConfig([]byte(tc.cfg))
		if err != nil {
			t.Fatal(err)
		}
		if got := c.timeServers(tc.platform, tc.md, tc.dhcp); !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if _, err := parseConfig([]byte(`{"exec":"/a","timesync":"sometimes"}`)); err == nil {
		t.Error("accepted unknown timesync mode")
	}
}

func TestOtelConfig(t *testing.T) {
	c, err := parseConfig([]byte(`{"exec":"/app/hello","otel":{"exporter":"otlp","otlp_endpoint":"http://c:4318"}}`))
	if err != nil {
		t.Fatal(err)
	}
	if c.Otel.OTLPEndpoint != "http://c:4318" || c.serviceName() != "hello" {
		t.Errorf("otel %+v, service %q", c.Otel, c.serviceName())
	}
	if _, err := parseConfig([]byte(`{"exec":"/a","otel":{"exporter":"syslog"}}`)); err == nil {
		t.Error("accepted unknown exporter")
	}
}

func TestResolvConf(t *testing.T) {
	got := resolvConf([]net.IP{net.IPv4(10, 0, 2, 3)}, "example", []string{"a.example", "b.example"})
	want := "search a.example b.example\nnameserver 10.0.2.3\n"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	if got := resolvConf(nil, "example", nil); got != "domain example\n" {
		t.Errorf("got %q", got)
	}
}
