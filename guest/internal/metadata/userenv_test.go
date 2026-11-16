// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package metadata

import (
	"reflect"
	"testing"
)

func TestParseUserDataEnv(t *testing.T) {
	want := map[string]string{"A": "x y", "N": "8080", "F": "1.5", "B": "true", "E": ""}
	for _, in := range []string{
		"\xef\xbb\xbf\n  {\"A\": \"x y\", \"N\": 8080, \"F\": 1.5, \"B\": true, \"E\": \"\"}\n",
		`[{"key": "A", "value": "x y"}, {"key": "N", "value": 8080}, {"key": "F", "value": 1.5},
		  {"Key": "B", "Value": true}, {"key": "E", "value": ""}]`,
		`[{"name": "A", "value": "x y"}, {"Name": "N", "Value": 8080}, {"key": "F", "value": 1.5},
		  {"NAME": "B", "value": true}, {"name": "E", "value": ""}]`,
		`[{"key": "A", "value": "old"}, {"key": "A", "value": "x y"}, {"key": "N", "value": "8080"},
		  {"key": "F", "value": "1.5"}, {"key": "B", "value": "true"}, {"key": "E", "value": ""}]`,
		"A='x y'\nN=8080\nF=1.5\nB=true\nE=\n",
	} {
		got, err := ParseUserDataEnv([]byte(in))
		if err != nil {
			t.Errorf("ParseUserDataEnv(%q): %v", in, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ParseUserDataEnv(%q) = %q, want %q", in, got, want)
		}
	}
	for _, in := range []string{"{}", "[]", " \n"} {
		if got, err := ParseUserDataEnv([]byte(in)); err != nil || len(got) != 0 {
			t.Errorf("ParseUserDataEnv(%q) = %v, %v; want empty", in, got, err)
		}
	}
}

func TestParseUserDataEnvRejects(t *testing.T) {
	for _, in := range []string{
		`{"A": "x"`,
		`{"A": "x"} {"B": "y"}`,
		`{"A": null}`,
		`{"A": {"b": 1}}`,
		`{"A": ["x"]}`,
		`{"1A": "x"}`,
		`{"A": "x\u0000y"}`,
		`[{"key": "A"}]`,
		`[{"value": "x"}]`,
		`[{"name": "A"}]`,
		`[{"key": "A", "name": "A", "value": "x"}]`,
		`[{"name": "A-B", "value": "x"}]`,
		`[{"key": "A", "value": "x", "extra": 1}]`,
		`[{"key": "A", "value": null}]`,
		`[{"key": "A-B", "value": "x"}]`,
		`["A=x"]`,
		`[1]`,
	} {
		if env, err := ParseUserDataEnv([]byte(in)); err == nil {
			t.Errorf("ParseUserDataEnv(%s) = %v, want an error", in, env)
		}
	}
}
