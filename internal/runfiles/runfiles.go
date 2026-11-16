// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package runfiles finds files that Bazel rules pass to tools as runfiles
// locations ("<repo>/<path>").
package runfiles

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Resolve returns an absolute path for p, which is either a path that exists
// as given or a runfiles location.
func Resolve(p string) (string, error) {
	if _, err := os.Stat(p); err == nil {
		return filepath.Abs(p)
	}
	var dirs []string
	for _, k := range []string{"RUNFILES_DIR", "TEST_SRCDIR"} {
		if v := os.Getenv(k); v != "" {
			dirs = append(dirs, v)
		}
	}
	// Bazel symlinks rule executables, so look next to argv[0] as invoked
	// before the resolved executable.
	dirs = append(dirs, os.Args[0]+".runfiles")
	if exe, err := os.Executable(); err == nil {
		dirs = append(dirs, exe+".runfiles")
	}
	for _, d := range dirs {
		c := filepath.Join(d, p)
		if _, err := os.Stat(c); err == nil {
			return c, nil
		}
	}
	if mf := os.Getenv("RUNFILES_MANIFEST_FILE"); mf != "" {
		if b, err := os.ReadFile(mf); err == nil {
			for _, line := range strings.Split(string(b), "\n") {
				if k, v, ok := strings.Cut(line, " "); ok && k == p {
					return v, nil
				}
			}
		}
	}
	return "", fmt.Errorf("cannot find %s directly or in runfiles", p)
}
