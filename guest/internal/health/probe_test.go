// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestTCPProbe(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := l.Addr().String()
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			c.Close()
		}
	}()
	for _, target := range []string{addr, "tcp://" + addr} {
		p, err := NewProber(target, "", time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := p.Probe(context.Background()); err != nil {
			t.Errorf("%s: %v", target, err)
		}
	}
	l.Close()
	p, _ := NewProber(addr, "", time.Second)
	if err := p.Probe(context.Background()); err == nil {
		t.Error("probe of a closed port succeeded")
	}
}

func TestHTTPProbe(t *testing.T) {
	code := http.StatusOK
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/redirect" {
			http.Redirect(w, r, "/elsewhere", http.StatusFound)
			return
		}
		w.WriteHeader(code)
	}))
	defer srv.Close()
	p, err := NewProber(srv.URL+"/healthz", "200-299", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.Probe(context.Background()); err != nil {
		t.Errorf("200: %v", err)
	}
	code = http.StatusServiceUnavailable
	if err := p.Probe(context.Background()); err == nil || !strings.Contains(err.Error(), "503") {
		t.Errorf("503: %v", err)
	}
	// Redirects are not followed; 302 counts as itself.
	p, _ = NewProber(srv.URL+"/redirect", "302", time.Second)
	if err := p.Probe(context.Background()); err != nil {
		t.Errorf("302: %v", err)
	}
}

func TestParseStatus(t *testing.T) {
	ok, err := parseStatus("200-299, 404")
	if err != nil {
		t.Fatal(err)
	}
	for code, want := range map[int]bool{199: false, 200: true, 299: true, 300: false, 404: true} {
		if ok(code) != want {
			t.Errorf("%d: %v", code, !want)
		}
	}
	for _, bad := range []string{"", "2xx", "300-200", "99", "200-600", "200,"} {
		if _, err := parseStatus(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
