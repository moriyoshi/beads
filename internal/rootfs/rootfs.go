// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package rootfs turns a list of files into a NetBSD root file system image.
//
// The tree is staged in a scratch directory and described by an mtree(8)
// spec carrying ownership, modes, symlinks and device nodes, so makefs(8) can
// build the image without root privileges and nothing needs to run MAKEDEV
// at boot.
package rootfs

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Type is the kind of an Entry.
type Type string

const (
	TypeFile Type = "file"
	TypeDir  Type = "dir"
	TypeLink Type = "link"
	TypeChar Type = "char"
)

// Entry is one node of the root file system.
type Entry struct {
	Path string `json:"path"`
	Type Type   `json:"type"`
	Mode uint32 `json:"mode"`
	// Src is the host file a TypeFile entry is copied from. Srcs, if set,
	// are concatenated instead (e.g. into a CA bundle). Content is used when
	// both are empty.
	Src     string   `json:"src,omitempty"`
	Srcs    []string `json:"srcs,omitempty"`
	Content string   `json:"content,omitempty"`
	// Target is the destination of a TypeLink entry.
	Target string `json:"target,omitempty"`
	// Major and Minor identify a TypeChar device.
	Major uint32 `json:"major,omitempty"`
	Minor uint32 `json:"minor,omitempty"`
}

// Tree is a set of entries keyed by absolute path.
type Tree struct {
	entries map[string]Entry
}

// NewTree returns an empty tree containing only the root directory.
func NewTree() *Tree {
	return &Tree{entries: map[string]Entry{"/": {Path: "/", Type: TypeDir, Mode: 0o755}}}
}

func validPath(p string) error {
	if !strings.HasPrefix(p, "/") || path.Clean(p) != p {
		return fmt.Errorf("rootfs: %q is not a clean absolute path", p)
	}
	for _, c := range p {
		// Keep to characters that need no escaping in an mtree spec.
		if !('a' <= c && c <= 'z' || 'A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.ContainsRune("/._-+,:@", c)) {
			return fmt.Errorf("rootfs: %q contains unsupported character %q", p, c)
		}
	}
	return nil
}

// Add inserts e, creating parent directories with mode 0755 as needed. Adding
// a path twice is an error unless both are identical directories.
func (t *Tree) Add(e Entry) error {
	if err := validPath(e.Path); err != nil {
		return err
	}
	switch e.Type {
	case TypeFile, TypeDir, TypeLink, TypeChar:
	default:
		return fmt.Errorf("rootfs: %s: unknown type %q", e.Path, e.Type)
	}
	if old, ok := t.entries[e.Path]; ok {
		if old.Type == TypeDir && e.Type == TypeDir {
			if e.Mode != 0 {
				old.Mode = e.Mode
				t.entries[e.Path] = old
			}
			return nil
		}
		return fmt.Errorf("rootfs: %s added twice", e.Path)
	}
	for dir := path.Dir(e.Path); dir != "/"; dir = path.Dir(dir) {
		if old, ok := t.entries[dir]; ok {
			if old.Type != TypeDir {
				return fmt.Errorf("rootfs: %s: parent %s is not a directory", e.Path, dir)
			}
			continue
		}
		t.entries[dir] = Entry{Path: dir, Type: TypeDir, Mode: 0o755}
	}
	if e.Mode == 0 {
		switch e.Type {
		case TypeDir:
			e.Mode = 0o755
		case TypeFile:
			e.Mode = 0o444
		case TypeLink:
			e.Mode = 0o755
		case TypeChar:
			e.Mode = 0o600
		}
	}
	t.entries[e.Path] = e
	return nil
}

// Entries returns all entries sorted by path.
func (t *Tree) Entries() []Entry {
	out := make([]Entry, 0, len(t.entries))
	for _, e := range t.entries {
		out = append(out, e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

func specPath(p string) string {
	if p == "/" {
		return "."
	}
	return "." + p
}

// WriteSpec writes an mtree(8) spec in full-path form. Every node is owned by
// root:wheel.
func (t *Tree) WriteSpec(w io.Writer) error {
	if _, err := fmt.Fprintln(w, "#mtree 2.0"); err != nil {
		return err
	}
	for _, e := range t.Entries() {
		line := fmt.Sprintf("%s type=%s mode=%04o uid=0 gid=0", specPath(e.Path), e.Type, e.Mode)
		switch e.Type {
		case TypeLink:
			if err := validPath(path.Clean("/" + e.Target)); err != nil {
				return fmt.Errorf("rootfs: %s: bad link target: %w", e.Path, err)
			}
			line += " link=" + e.Target
		case TypeChar:
			line += fmt.Sprintf(" device=netbsd,%d,%d", e.Major, e.Minor)
		}
		if _, err := fmt.Fprintln(w, line); err != nil {
			return err
		}
	}
	return nil
}

// Stage materialises directories and regular files under dir. Links and
// device nodes exist only in the spec; makefs creates them.
func (t *Tree) Stage(dir string) error {
	for _, e := range t.Entries() {
		dst := filepath.Join(dir, filepath.FromSlash(e.Path))
		switch e.Type {
		case TypeDir:
			if err := os.MkdirAll(dst, 0o755); err != nil {
				return err
			}
		case TypeFile:
			if err := stageFile(dst, e); err != nil {
				return fmt.Errorf("rootfs: %s: %w", e.Path, err)
			}
		}
	}
	return nil
}

func stageFile(dst string, e Entry) error {
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	srcs := e.Srcs
	if e.Src != "" {
		srcs = append([]string{e.Src}, srcs...)
	}
	if len(srcs) == 0 {
		_, err = io.WriteString(out, e.Content)
		return err
	}
	for _, src := range srcs {
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		_, err = io.Copy(out, in)
		in.Close()
		if err != nil {
			return err
		}
	}
	return out.Close()
}

// MakefsOptions controls the makefs invocation.
type MakefsOptions struct {
	Makefs string // path to makefs (nbmakefs when built by build.sh)
	// SizeBytes fixes the image size; zero lets makefs pick the minimum plus
	// FreePercent of free blocks and inodes.
	SizeBytes   int64
	FreePercent int
	// Epoch is the timestamp stamped on every node, for reproducible images.
	Epoch int64
}

// Build stages the tree in a scratch directory and runs makefs to produce an
// FFSv2 image at out. The spec is also written to specOut if non-empty.
func (t *Tree) Build(out, specOut string, opt MakefsOptions) error {
	scratch, err := os.MkdirTemp("", "rootfs-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(scratch)

	stage := filepath.Join(scratch, "root")
	if err := t.Stage(stage); err != nil {
		return err
	}
	spec := filepath.Join(scratch, "spec")
	f, err := os.Create(spec)
	if err != nil {
		return err
	}
	if err := t.WriteSpec(f); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if specOut != "" {
		if err := copyFile(spec, specOut); err != nil {
			return err
		}
	}

	args := []string{"-t", "ffs", "-B", "le", "-o", "version=2", "-F", spec, "-T", strconv.FormatInt(opt.Epoch, 10)}
	if opt.SizeBytes > 0 {
		args = append(args, "-s", strconv.FormatInt(opt.SizeBytes, 10))
	} else {
		pct := opt.FreePercent
		if pct <= 0 {
			pct = 10
		}
		args = append(args, "-b", fmt.Sprintf("%d%%", pct), "-f", fmt.Sprintf("%d%%", pct))
	}
	args = append(args, out, stage)
	cmd := exec.Command(opt.Makefs, args...)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rootfs: %s %s: %w", opt.Makefs, strings.Join(args, " "), err)
	}
	return nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}
