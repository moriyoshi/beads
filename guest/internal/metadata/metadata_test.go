// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package metadata

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestDetect(t *testing.T) {
	for _, tc := range []struct {
		vendor, product string
		want            Platform
	}{
		{"Amazon EC2", "m6i.large", EC2},
		{"Xen", "Amazon EC2", EC2}, // some instance types report Xen as vendor
		{"Google", "Google Compute Engine", GCE},
		{"Microsoft Corporation", "Virtual Machine", Azure},
		{"QEMU", "Standard PC (Q35 + ICH9, 2009)", QEMU},
		{"Dell Inc.", "PowerEdge", Unknown},
	} {
		if got := Detect(tc.vendor, tc.product); got != tc.want {
			t.Errorf("Detect(%q, %q) = %s, want %s", tc.vendor, tc.product, got, tc.want)
		}
	}
}

// fakeIMDS implements the IMDSv2 token protocol and a few keys.
func fakeIMDS(t *testing.T, withUserData bool) *httptest.Server {
	return fakeIMDSWithRole(t, withUserData, false)
}

func fakeIMDSWithRole(t *testing.T, withUserData, withRole bool) *httptest.Server {
	const token = "tok123"
	data := map[string]string{
		"/latest/meta-data/instance-id":                 "i-0123456789abcdef0",
		"/latest/meta-data/placement/region":            "ap-northeast-1",
		"/latest/meta-data/placement/availability-zone": "ap-northeast-1a",
		"/latest/meta-data/local-ipv4":                  "10.0.1.23",
	}
	if withUserData {
		data["/latest/user-data"] = "#!config\nkey=value\n"
	}
	if withRole {
		data["/latest/meta-data/iam/info"] = `{"Code":"Success","InstanceProfileArn":"arn:aws:iam::1:instance-profile/x"}`
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/latest/api/token" {
			if r.Method != http.MethodPut || r.Header.Get("X-aws-ec2-metadata-token-ttl-seconds") == "" {
				http.Error(w, "bad token request", http.StatusBadRequest)
				return
			}
			w.Write([]byte(token))
			return
		}
		if r.Header.Get("X-aws-ec2-metadata-token") != token {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		v, ok := data[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(v))
	}))
}

func TestEC2Fetch(t *testing.T) {
	srv := fakeIMDS(t, true)
	defer srv.Close()
	info, err := (&EC2Client{BaseURL: srv.URL}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.InstanceID != "i-0123456789abcdef0" || info.Region != "ap-northeast-1" || info.Zone != "ap-northeast-1a" || info.LocalIPv4 != "10.0.1.23" {
		t.Errorf("info = %+v", info)
	}
	if info.Hostname != "" {
		t.Errorf("missing key should stay empty, got %q", info.Hostname)
	}
	if string(info.UserData) != "#!config\nkey=value\n" {
		t.Errorf("user-data = %q", info.UserData)
	}
	files := info.Files()
	if string(files["platform"]) != "ec2\n" || string(files["instance-id"]) != "i-0123456789abcdef0\n" {
		t.Errorf("files = %q", files)
	}
	if _, ok := files["hostname"]; ok {
		t.Error("empty hostname should not produce a file")
	}
	if env := info.Env(); env["APPLIANCE_INSTANCE_ID"] != "i-0123456789abcdef0" || env["APPLIANCE_PLATFORM"] != "ec2" {
		t.Errorf("env = %v", env)
	}
}

func TestEC2FetchRetriesUntilReachable(t *testing.T) {
	srv := fakeIMDS(t, false)
	defer srv.Close()
	fails := 2
	c := &EC2Client{BaseURL: srv.URL, HTTP: &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		if fails > 0 {
			fails--
			return nil, fmt.Errorf("connect: host is down")
		}
		return http.DefaultTransport.RoundTrip(r)
	})}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	info, err := c.Fetch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.InstanceID != "i-0123456789abcdef0" {
		t.Errorf("info = %+v", info)
	}
}

func TestEC2FetchGivesUpOnHTTPErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()
	start := time.Now()
	if _, err := (&EC2Client{BaseURL: srv.URL}).Fetch(context.Background()); err == nil {
		t.Fatal("expected error")
	}
	if time.Since(start) > 2*time.Second {
		t.Error("retried an HTTP error status")
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestEC2FetchInstanceProfile(t *testing.T) {
	for _, role := range []bool{false, true} {
		srv := fakeIMDSWithRole(t, false, role)
		info, err := (&EC2Client{BaseURL: srv.URL}).Fetch(context.Background())
		srv.Close()
		if err != nil {
			t.Fatal(err)
		}
		if info.HasCredentials != role {
			t.Errorf("role %v: HasCredentials = %v", role, info.HasCredentials)
		}
	}
}

func TestEC2FetchWithoutUserData(t *testing.T) {
	srv := fakeIMDS(t, false)
	defer srv.Close()
	info, err := (&EC2Client{BaseURL: srv.URL}).Fetch(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if info.UserData != nil {
		t.Errorf("user-data = %q, want none", info.UserData)
	}
	if _, ok := info.Files()["user-data"]; ok {
		t.Error("user-data file without user data")
	}
}
