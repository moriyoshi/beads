// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"encoding/json"
	"fmt"
	"net"
	"net/netip"
	"os"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/moriyoshi/beads/guest/internal/health"
	"github.com/moriyoshi/beads/guest/internal/metadata"
	"github.com/moriyoshi/beads/guest/internal/otelconfig"
)

// ConfigPath is where netbsd_rootfs places the appliance configuration.
const ConfigPath = "/etc/appliance.json"

// metadataDir holds what init learned about the instance (platform,
// instance-id, region, user-data, ...), one file per item.
const metadataDir = "/var/run/appliance"

// Config is the appliance description written by the Bazel rules.
type Config struct {
	// Exec is the absolute path of the program to run.
	Exec string            `json:"exec"`
	Args []string          `json:"args,omitempty"`
	Env  map[string]string `json:"env,omitempty"`
	// UserDataEnv says whether user-data that defines variables (as JSON or
	// KEY=VALUE lines) adds to and overrides Env; nil means yes.
	UserDataEnv *bool `json:"user_data_env,omitempty"`

	// Network is "dhcp" (the default) or "none".
	Network string `json:"network,omitempty"`
	// Interface restricts DHCP to one interface; by default the first
	// Ethernet interface is used.
	Interface string `json:"interface,omitempty"`
	Hostname  string `json:"hostname,omitempty"`

	// Timesync is "auto" (the default) or "off". With "auto", init keeps
	// the clock in step with TimeServers, or else with the platform's own
	// time service plus any NTP servers DHCP offers.
	Timesync    string   `json:"timesync,omitempty"`
	TimeServers []string `json:"time_servers,omitempty"`

	// OnExit says what happens when the program exits: "restart" (the
	// default), "poweroff", "reboot" or "halt".
	OnExit          string `json:"on_exit,omitempty"`
	RestartDelaySec int    `json:"restart_delay_sec,omitempty"`
	// StopTimeoutSec bounds how long shutdown waits after SIGTERM before
	// sending SIGKILL.
	StopTimeoutSec int `json:"stop_timeout_sec,omitempty"`

	// Otel configures log shipping through the OpenTelemetry Collector
	// embedded in init.
	Otel otelconfig.Config `json:"otel,omitempty"`

	// platformEnv is discovered at boot and overridden by Env.
	platformEnv map[string]string
	// userEnv comes from user-data and overrides Env, except for the
	// variables reservedEnv protects.
	userEnv map[string]string
	// localAddrs is user-data's LocalAddrsKey, unparsed.
	localAddrs string
	// healthEnv holds user-data's health.Keys.
	healthEnv map[string]string
}

// LocalAddrsKey names the user-data variable that lists extra local
// IPv4 addresses for lo0, such as an AWS overlay IP. It is a setting for init,
// not a variable for the program.
const LocalAddrsKey = "APPLIANCE_LOCAL_ADDRS"

func (c *Config) addPlatformEnv(env map[string]string) {
	if c.platformEnv == nil {
		c.platformEnv = map[string]string{}
	}
	for k, v := range env {
		c.platformEnv[k] = v
	}
}

// collectorEnviron is the embedded collector's environment: a fixed base,
// then beads (otelconfig.Env), then, with otel user_data_env, the variables
// user-data gives the program, sorted after the rest. User-data cannot
// override the base or BEADS_*; variables it does not define stay unset, so
// ${env:NAME:-default} still applies. It also returns the names of the
// user-data variables passed.
func (c *Config) collectorEnviron(beads []string) (env, names []string) {
	env = append([]string{"HOME=/", "PATH=/sbin", "TMPDIR=/tmp"}, beads...)
	if !c.Otel.UserDataEnv {
		return env, nil
	}
	for k := range c.userEnv {
		if !reservedEnv(k) && !strings.HasPrefix(k, "BEADS_") {
			names = append(names, k)
		}
	}
	sort.Strings(names)
	for _, k := range names {
		env = append(env, k+"="+c.userEnv[k])
	}
	return env, names
}

// useUserDataEnv reports whether user-data may set program variables.
func (c *Config) useUserDataEnv() bool { return c.UserDataEnv == nil || *c.UserDataEnv }

// reservedEnv reports whether a variable is init's to set: the base
// environment, the instance description and the OpenTelemetry SDK settings.
// User-data cannot change these; the image's own Env can.
func reservedEnv(k string) bool {
	switch k {
	case "PATH", "HOME", "TMPDIR":
		return true
	}
	return strings.HasPrefix(k, "APPLIANCE_") || strings.HasPrefix(k, "OTEL_")
}

// setUserEnv parses user-data as variables (metadata.ParseUserDataEnv),
// takes init's own settings from it, and, if useUserDataEnv, records the
// variables for the program. It returns, sorted, the names it took and those
// it refused because they are reserved.
func (c *Config) setUserEnv(userData []byte) (names, refused []string, err error) {
	env, err := metadata.ParseUserDataEnv(userData)
	if err != nil {
		return nil, nil, err
	}
	c.localAddrs = env[LocalAddrsKey]
	delete(env, LocalAddrsKey)
	c.healthEnv = map[string]string{}
	for _, k := range health.Keys {
		if v, ok := env[k]; ok {
			c.healthEnv[k] = v
			delete(env, k)
		}
	}
	if !c.useUserDataEnv() {
		return nil, nil, nil
	}
	c.userEnv = map[string]string{}
	for k, v := range env {
		if reservedEnv(k) {
			refused = append(refused, k)
			continue
		}
		c.userEnv[k] = v
		names = append(names, k)
	}
	sort.Strings(names)
	sort.Strings(refused)
	return names, refused, nil
}

// parseLocalAddrs parses a LocalAddrsKey value: comma-separated IPv4
// addresses, each bare or with /32. Addresses that would shadow something
// else (loopback, link-local, multicast, ...) and duplicates are errors.
func parseLocalAddrs(s string) ([]netip.Addr, error) {
	if strings.TrimSpace(s) == "" {
		return nil, nil
	}
	var addrs []netip.Addr
	seen := map[netip.Addr]bool{}
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		if f == "" {
			return nil, fmt.Errorf("%s: empty entry in %q", LocalAddrsKey, s)
		}
		a, err := netip.ParseAddr(f)
		if err != nil {
			p, perr := netip.ParsePrefix(f)
			if perr != nil {
				return nil, fmt.Errorf("%s: %q is not an IPv4 address or /32", LocalAddrsKey, f)
			}
			if p.Bits() != 32 || !p.Addr().Is4() {
				return nil, fmt.Errorf("%s: %q: only IPv4 /32 addresses are allowed", LocalAddrsKey, f)
			}
			a = p.Addr()
		}
		switch {
		case !a.Is4():
			return nil, fmt.Errorf("%s: %q: only IPv4 /32 addresses are allowed", LocalAddrsKey, f)
		case a.IsUnspecified(), a.IsLoopback(), a.IsLinkLocalUnicast(), a.IsMulticast(),
			a == netip.AddrFrom4([4]byte{255, 255, 255, 255}), a.As4()[0] >= 240:
			return nil, fmt.Errorf("%s: %q is not a usable unicast address", LocalAddrsKey, f)
		case seen[a]:
			return nil, fmt.Errorf("%s: %s is listed twice", LocalAddrsKey, a)
		}
		seen[a] = true
		addrs = append(addrs, a)
	}
	return addrs, nil
}

func parseConfig(b []byte) (*Config, error) {
	var c Config
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath, err)
	}
	if !strings.HasPrefix(c.Exec, "/") {
		return nil, fmt.Errorf("%s: exec must be an absolute path, got %q", ConfigPath, c.Exec)
	}
	switch c.Network {
	case "":
		c.Network = "dhcp"
	case "dhcp", "none":
	default:
		return nil, fmt.Errorf("%s: unknown network mode %q", ConfigPath, c.Network)
	}
	if err := c.Otel.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", ConfigPath, err)
	}
	switch c.Timesync {
	case "":
		c.Timesync = "auto"
	case "auto", "off":
	default:
		return nil, fmt.Errorf("%s: unknown timesync %q", ConfigPath, c.Timesync)
	}
	switch c.OnExit {
	case "":
		c.OnExit = "restart"
	case "restart", "poweroff", "reboot", "halt":
	default:
		return nil, fmt.Errorf("%s: unknown on_exit %q", ConfigPath, c.OnExit)
	}
	if c.RestartDelaySec <= 0 {
		c.RestartDelaySec = 1
	}
	if c.StopTimeoutSec <= 0 {
		c.StopTimeoutSec = 10
	}
	return &c, nil
}

func loadConfig() (*Config, error) {
	b, err := os.ReadFile(ConfigPath)
	if err != nil {
		return nil, err
	}
	return parseConfig(b)
}

func (c *Config) restartDelay() time.Duration { return time.Duration(c.RestartDelaySec) * time.Second }
func (c *Config) stopTimeout() time.Duration  { return time.Duration(c.StopTimeoutSec) * time.Second }

// environ is the program's environment: a fixed base, then what init
// discovered about the instance, then the configured variables, then those
// from user-data; sorted for stable output.
func (c *Config) environ() []string {
	env := map[string]string{
		"PATH":   "/app:/sbin",
		"HOME":   "/",
		"TMPDIR": "/tmp",
	}
	for k, v := range c.platformEnv {
		env[k] = v
	}
	for k, v := range c.Env {
		env[k] = v
	}
	for k, v := range c.userEnv {
		env[k] = v
	}
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

// platformTimeServers are the link-local time services clouds provide. Azure
// is absent on purpose: the kernel's hvtimesync(4) keeps the clock in step
// with the Hyper-V host.
var platformTimeServers = map[string][]string{
	"ec2": {"169.254.169.123"}, // Amazon Time Sync Service
	"gce": {"169.254.169.254"}, // metadata server
}

// timeServers decides which NTP servers init synchronizes with: the
// configured ones, or else the platform's time service, servers its
// metadata names, and servers DHCP offers.
func (c *Config) timeServers(platform string, metadataNTP []string, dhcpNTP []net.IP) []string {
	if c.Timesync == "off" {
		return nil
	}
	if len(c.TimeServers) > 0 {
		return c.TimeServers
	}
	var out []string
	seen := map[string]bool{}
	add := func(s string) {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	for _, s := range platformTimeServers[platform] {
		add(s)
	}
	for _, s := range metadataNTP {
		add(s)
	}
	for _, ip := range dhcpNTP {
		add(ip.String())
	}
	return out
}

// serviceName is the OpenTelemetry service.name: configured, or else the
// program's file name.
func (c *Config) serviceName() string {
	if c.Otel.ServiceName != "" {
		return c.Otel.ServiceName
	}
	return path.Base(c.Exec)
}

// resolvConf renders resolv.conf(5).
func resolvConf(dns []net.IP, domain string, search []string) string {
	var b strings.Builder
	if len(search) > 0 {
		fmt.Fprintf(&b, "search %s\n", strings.Join(search, " "))
	} else if domain != "" {
		fmt.Fprintf(&b, "domain %s\n", domain)
	}
	for _, ip := range dns {
		fmt.Fprintf(&b, "nameserver %s\n", ip)
	}
	return b.String()
}
