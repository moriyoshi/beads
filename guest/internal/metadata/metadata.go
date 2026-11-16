// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package metadata identifies the cloud an appliance runs on and reads its
// instance metadata service.
package metadata

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Platform is where the appliance is running.
type Platform string

const (
	Unknown Platform = "unknown"
	QEMU    Platform = "qemu"
	EC2     Platform = "ec2"
	GCE     Platform = "gce"
	Azure   Platform = "azure"
)

// Detect maps SMBIOS system vendor and product strings to a platform.
func Detect(vendor, product string) Platform {
	v := strings.ToLower(vendor)
	p := strings.ToLower(product)
	switch {
	case strings.Contains(v, "amazon") || strings.HasPrefix(p, "amazon ec2"):
		return EC2
	case strings.Contains(v, "google"):
		return GCE
	case strings.Contains(v, "microsoft") && strings.Contains(p, "virtual machine"):
		return Azure
	case strings.Contains(v, "qemu") || strings.Contains(p, "qemu") || strings.Contains(p, "kvm"):
		return QEMU
	}
	return Unknown
}

// Info is what the appliance learns about its instance. Values are empty
// when the platform does not provide them.
type Info struct {
	Platform   Platform
	InstanceID string
	Region     string
	Zone       string
	LocalIPv4  string
	Hostname   string
	UserData   []byte
	// TimeServers are NTP servers the platform's metadata names, used
	// alongside the platform's built-in time service.
	TimeServers []string
	// HasCredentials reports whether the instance can authenticate to its
	// platform's services (an EC2 instance profile).
	HasCredentials bool
	// OTLPEndpoint is an OTLP/HTTP endpoint for logs named by platform
	// metadata (QEMU fw_cfg).
	OTLPEndpoint string
}

// Files returns the files the init shim publishes under /var/run/appliance,
// keyed by name.
func (i *Info) Files() map[string][]byte {
	f := map[string][]byte{"platform": []byte(string(i.Platform) + "\n")}
	add := func(name, v string) {
		if v != "" {
			f[name] = []byte(v + "\n")
		}
	}
	add("instance-id", i.InstanceID)
	add("region", i.Region)
	add("zone", i.Zone)
	add("local-ipv4", i.LocalIPv4)
	add("hostname", i.Hostname)
	if i.UserData != nil {
		f["user-data"] = i.UserData
	}
	return f
}

// Env returns environment variables describing the instance for the
// appliance program.
func (i *Info) Env() map[string]string {
	env := map[string]string{"APPLIANCE_PLATFORM": string(i.Platform)}
	if i.InstanceID != "" {
		env["APPLIANCE_INSTANCE_ID"] = i.InstanceID
	}
	if i.Region != "" {
		env["APPLIANCE_REGION"] = i.Region
	}
	return env
}

// errNotFound marks metadata keys the service does not have, such as
// user-data on an instance launched without any.
var errNotFound = errors.New("not found")

// statusError is an HTTP error status from the metadata service.
type statusError string

func (e statusError) Error() string { return string(e) }

// EC2Client reads the EC2 instance metadata service with IMDSv2 session
// tokens, which work whether or not the instance requires them.
type EC2Client struct {
	// BaseURL defaults to http://169.254.169.254.
	BaseURL string
	HTTP    *http.Client
	token   string
}

func (c *EC2Client) base() string {
	if c.BaseURL != "" {
		return strings.TrimRight(c.BaseURL, "/")
	}
	return "http://169.254.169.254"
}

func (c *EC2Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return &http.Client{Timeout: 3 * time.Second}
}

func (c *EC2Client) do(ctx context.Context, method, path string, hdr map[string]string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.base()+path, nil)
	if err != nil {
		return nil, err
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return nil, errNotFound
	case resp.StatusCode != http.StatusOK:
		return nil, statusError(fmt.Sprintf("%s %s: %s", method, path, resp.Status))
	}
	return b, nil
}

func (c *EC2Client) get(ctx context.Context, path string) ([]byte, error) {
	if c.token == "" {
		tok, err := c.do(ctx, http.MethodPut, "/latest/api/token", map[string]string{
			"X-aws-ec2-metadata-token-ttl-seconds": "21600",
		})
		if err != nil {
			return nil, fmt.Errorf("IMDSv2 token: %w", err)
		}
		c.token = strings.TrimSpace(string(tok))
	}
	return c.do(ctx, http.MethodGet, path, map[string]string{"X-aws-ec2-metadata-token": c.token})
}

// Fetch reads the instance's identity and user data, retrying transport
// errors until ctx ends: right after boot the NIC may still be renegotiating
// (ENA resets when its MTU changes) or ARP may not have resolved yet.
func (c *EC2Client) Fetch(ctx context.Context) (*Info, error) {
	for wait := 500 * time.Millisecond; ; wait = min(2*wait, 4*time.Second) {
		info, err := c.fetchOnce(ctx)
		if err == nil || ctx.Err() != nil {
			return info, err
		}
		var status statusError
		if errors.As(err, &status) {
			return nil, err // the service answered; retrying will not help
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(wait):
		}
	}
}

func (c *EC2Client) fetchOnce(ctx context.Context) (*Info, error) {
	info := &Info{Platform: EC2}
	fields := []struct {
		path string
		dst  *string
	}{
		{"/latest/meta-data/instance-id", &info.InstanceID},
		{"/latest/meta-data/placement/region", &info.Region},
		{"/latest/meta-data/placement/availability-zone", &info.Zone},
		{"/latest/meta-data/local-ipv4", &info.LocalIPv4},
		{"/latest/meta-data/local-hostname", &info.Hostname},
	}
	for _, f := range fields {
		b, err := c.get(ctx, f.path)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		*f.dst = strings.TrimSpace(string(b))
	}
	// An instance profile shows up as iam/info; without one it is 404.
	if _, err := c.get(ctx, "/latest/meta-data/iam/info"); err == nil {
		info.HasCredentials = true
	} else if !errors.Is(err, errNotFound) {
		return nil, err
	}
	ud, err := c.get(ctx, "/latest/user-data")
	switch {
	case err == nil:
		info.UserData = ud
	case !errors.Is(err, errNotFound):
		return nil, err
	}
	return info, nil
}
