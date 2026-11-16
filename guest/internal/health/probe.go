// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package health

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Prober checks the service once; a nil error means healthy.
type Prober interface {
	Probe(ctx context.Context) error
}

// NewProber parses a probe target: "tcp://host:port" or a bare "host:port"
// connects; "http://host:port/path" sends a GET and expects a status in
// httpStatus. timeout bounds each probe.
func NewProber(target, httpStatus string, timeout time.Duration) (Prober, error) {
	switch {
	case strings.HasPrefix(target, "http://"):
		u, err := url.Parse(target)
		if err != nil || u.Host == "" {
			return nil, fmt.Errorf("%s: %q is not a URL", CheckKey, target)
		}
		ok, err := parseStatus(httpStatus)
		if err != nil {
			return nil, err
		}
		return &httpProber{url: u.String(), ok: ok, client: &http.Client{
			Timeout: timeout,
			// The service itself must answer: no proxies from the
			// environment, no following redirects elsewhere.
			Transport:     &http.Transport{Proxy: nil, DisableKeepAlives: true},
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		}}, nil
	case strings.Contains(target, "://") && !strings.HasPrefix(target, "tcp://"):
		return nil, fmt.Errorf("%s: %q: only tcp:// and http:// are supported", CheckKey, target)
	}
	addr := strings.TrimPrefix(target, "tcp://")
	host, port, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return nil, fmt.Errorf("%s: %q is not host:port", CheckKey, target)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("%s: %q: bad port", CheckKey, target)
	}
	return &tcpProber{addr: addr, timeout: timeout}, nil
}

type tcpProber struct {
	addr    string
	timeout time.Duration
}

func (p *tcpProber) Probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, p.timeout)
	defer cancel()
	var d net.Dialer
	c, err := d.DialContext(ctx, "tcp", p.addr)
	if err != nil {
		return err
	}
	return c.Close()
}

type httpProber struct {
	url    string
	ok     func(int) bool
	client *http.Client
}

func (p *httpProber) Probe(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, p.url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", "beads-health")
	resp, err := p.client.Do(req)
	if err != nil {
		return err
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	if !p.ok(resp.StatusCode) {
		return fmt.Errorf("GET %s: %s", p.url, resp.Status)
	}
	return nil
}

// parseStatus parses comma-separated status codes and ranges ("200-399").
func parseStatus(s string) (func(int) bool, error) {
	type span struct{ lo, hi int }
	var spans []span
	for _, f := range strings.Split(s, ",") {
		f = strings.TrimSpace(f)
		lo, hi, isRange := strings.Cut(f, "-")
		a, err1 := strconv.Atoi(strings.TrimSpace(lo))
		b, err2 := a, error(nil)
		if isRange {
			b, err2 = strconv.Atoi(strings.TrimSpace(hi))
		}
		if err1 != nil || err2 != nil || a < 100 || b > 599 || a > b {
			return nil, fmt.Errorf("%s: %q is not a list of status codes or ranges such as 200-399", HTTPStatusKey, s)
		}
		spans = append(spans, span{a, b})
	}
	return func(code int) bool {
		for _, sp := range spans {
			if sp.lo <= code && code <= sp.hi {
				return true
			}
		}
		return false
	}, nil
}
