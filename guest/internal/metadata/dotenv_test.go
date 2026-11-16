// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package metadata

import (
	"reflect"
	"testing"
)

func TestParseDotenv(t *testing.T) {
	in := "\xef\xbb\xbf# settings\r\n" +
		"LISTEN=:8080\r\n" +
		"\n" +
		"export GREETING=\"hello, \\\"world\\\"\\n\" # trailing\n" +
		"PATTERN='$x # kept'\n" +
		"SPACED = a b  # comment\n" +
		"HASH=a#b\n" +
		"EMPTY=\n" +
		"export=1\n" +
		"LISTEN=:9090\n"
	got, err := ParseDotenv([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"LISTEN":   ":9090",
		"GREETING": "hello, \"world\"\n",
		"PATTERN":  "$x # kept",
		"SPACED":   "a b",
		"HASH":     "a#b",
		"EMPTY":    "",
		"export":   "1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestParseDotenvEmpty(t *testing.T) {
	got, err := ParseDotenv([]byte("\n# nothing\n"))
	if err != nil || len(got) != 0 {
		t.Errorf("got %v, %v", got, err)
	}
}

func TestParseDotenvRejects(t *testing.T) {
	for _, in := range []string{
		"#!/bin/sh\necho hello\n",
		"#cloud-config\npackages:\n  - curl\n",
		`{"a": 1}`,
		"1A=x",
		"A-B=x",
		"A='open",
		`A="open`,
		`A="x" y`,
		"A=\"\\\"",
		"A=x\x00y",
	} {
		if env, err := ParseDotenv([]byte(in)); err == nil {
			t.Errorf("ParseDotenv(%q) = %v, want an error", in, env)
		}
	}
}
