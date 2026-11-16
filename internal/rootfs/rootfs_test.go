// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package rootfs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSpec(t *testing.T) {
	tr := NewTree()
	if err := AddBase(tr, "amd64"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Add(Entry{Path: "/app/server", Type: TypeFile, Mode: 0o555, Content: "x"}); err != nil {
		t.Fatal(err)
	}
	var b strings.Builder
	if err := tr.WriteSpec(&b); err != nil {
		t.Fatal(err)
	}
	spec := b.String()
	for _, want := range []string{
		". type=dir mode=0755 uid=0 gid=0\n",
		"./app/server type=file mode=0555 uid=0 gid=0\n",
		"./dev/console type=char mode=0600 uid=0 gid=0 device=netbsd,0,0\n",
		"./dev/null type=char mode=0666 uid=0 gid=0 device=netbsd,2,2\n",
		"./etc/resolv.conf type=link mode=0755 uid=0 gid=0 link=/var/run/resolv.conf\n",
		"./tmp type=dir mode=1777 uid=0 gid=0\n",
	} {
		if !strings.Contains(spec, want) {
			t.Errorf("spec lacks %q", want)
		}
	}
	// Parents must precede children for makefs.
	if strings.Index(spec, "./dev type=") > strings.Index(spec, "./dev/console") {
		t.Error("parent directory listed after child")
	}
}

func TestAddRules(t *testing.T) {
	tr := NewTree()
	if err := tr.Add(Entry{Path: "/a/b/c", Type: TypeFile}); err != nil {
		t.Fatal(err)
	}
	for _, e := range []Entry{
		{Path: "/a/b/c", Type: TypeFile},     // duplicate
		{Path: "/a/b/c/d", Type: TypeFile},   // parent is a file
		{Path: "relative", Type: TypeFile},   // not absolute
		{Path: "/a/../b", Type: TypeFile},    // not clean
		{Path: "/has space", Type: TypeFile}, // needs escaping
		{Path: "/x", Type: "fifo"},           // unsupported type
	} {
		if err := tr.Add(e); err == nil {
			t.Errorf("Add(%+v) succeeded, want error", e)
		}
	}
	if err := tr.Add(Entry{Path: "/a", Type: TypeDir, Mode: 0o700}); err != nil {
		t.Errorf("re-adding a directory: %v", err)
	}
}

func TestStage(t *testing.T) {
	src := filepath.Join(t.TempDir(), "src")
	if err := os.WriteFile(src, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	tr := NewTree()
	if err := AddBase(tr, "amd64"); err != nil {
		t.Fatal(err)
	}
	if err := tr.Add(Entry{Path: "/app/bin", Type: TypeFile, Src: src}); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := tr.Stage(dir); err != nil {
		t.Fatal(err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "app/bin")); err != nil || string(b) != "payload" {
		t.Errorf("app/bin = %q, %v", b, err)
	}
	if b, err := os.ReadFile(filepath.Join(dir, "etc/hosts")); err != nil || !strings.Contains(string(b), "localhost") {
		t.Errorf("etc/hosts = %q, %v", b, err)
	}
	if _, err := os.Lstat(filepath.Join(dir, "dev/console")); !os.IsNotExist(err) {
		t.Errorf("device node staged on host: %v", err)
	}
}
