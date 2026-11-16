// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bazelbuild/rules_go/go/runfiles"
)

type entry struct {
	Path    string   `json:"path"`
	Src     string   `json:"src"`
	Srcs    []string `json:"srcs"`
	Content string   `json:"content"`
}

func manifest(t *testing.T, env string) map[string]entry {
	t.Helper()
	p, err := runfiles.Rlocation(os.Getenv(env))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Entries []entry `json:"entries"`
	}
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	out := map[string]entry{}
	for _, e := range m.Entries {
		out[e.Path] = e
	}
	return out
}

const bundlePath = "/etc/openssl/certs/ca-certificates.crt"

func TestCurlCABundle(t *testing.T) {
	e, ok := manifest(t, "HELLO_MANIFEST")[bundlePath]
	if !ok {
		t.Fatalf("%s missing", bundlePath)
	}
	if len(e.Srcs) != 1 || filepath.Base(e.Srcs[0]) != "cacert.pem" {
		t.Errorf("%s comes from %v, want curl's cacert.pem", bundlePath, e.Srcs)
	}
	p, err := runfiles.Rlocation(os.Getenv("CURL_BUNDLE"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "BEGIN CERTIFICATE"); n < 100 {
		t.Errorf("curl bundle has only %d certificates", n)
	}
}

func TestDefaultBundleIsNetBSDs(t *testing.T) {
	e, ok := manifest(t, "GENERIC_MANIFEST")[bundlePath]
	if !ok {
		t.Fatalf("%s missing", bundlePath)
	}
	if len(e.Srcs) < 100 || !strings.Contains(e.Srcs[0], "usr/share/certs/mozilla/server") {
		t.Errorf("%s comes from %d files starting %v, want NetBSD's Mozilla certificates", bundlePath, len(e.Srcs), e.Srcs[:min(1, len(e.Srcs))])
	}
}
