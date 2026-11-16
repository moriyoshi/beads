// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package fat32

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

type memDisk []byte

func (m memDisk) WriteAt(p []byte, off int64) (int, error) {
	return copy(m[off:], p), nil
}

// reader is a minimal FAT32 reader used to check what Build wrote.
type reader struct {
	img       []byte
	fatStart  int
	dataStart int
	rootClus  uint32
}

func newReader(t *testing.T, img []byte) *reader {
	t.Helper()
	if img[510] != 0x55 || img[511] != 0xAA || string(img[82:90]) != "FAT32   " {
		t.Fatal("not a FAT32 boot sector")
	}
	rsv := int(binary.LittleEndian.Uint16(img[14:]))
	nf := int(img[16])
	fsz := int(binary.LittleEndian.Uint32(img[36:]))
	return &reader{
		img:       img,
		fatStart:  rsv * sectorSize,
		dataStart: (rsv + nf*fsz) * sectorSize,
		rootClus:  binary.LittleEndian.Uint32(img[44:]),
	}
}

func (r *reader) chain(c uint32) []byte {
	var out []byte
	for c >= 2 && c < 0x0FFFFFF8 {
		off := r.dataStart + int(c-2)*clusterSize
		out = append(out, r.img[off:off+clusterSize]...)
		c = binary.LittleEndian.Uint32(r.img[r.fatStart+int(c)*4:]) & 0x0FFFFFFF
	}
	return out
}

func (r *reader) lookup(t *testing.T, p string) []byte {
	t.Helper()
	clus := r.rootClus
	parts := strings.Split(p, "/")
	for i, comp := range parts {
		want, err := shortName(comp)
		if err != nil {
			t.Fatal(err)
		}
		dir := r.chain(clus)
		found := false
		for off := 0; off+dirEntrySize <= len(dir) && dir[off] != 0; off += dirEntrySize {
			e := dir[off : off+dirEntrySize]
			if !bytes.Equal(e[:11], want[:]) {
				continue
			}
			c := uint32(binary.LittleEndian.Uint16(e[20:]))<<16 | uint32(binary.LittleEndian.Uint16(e[26:]))
			if i == len(parts)-1 {
				size := binary.LittleEndian.Uint32(e[28:])
				return r.chain(c)[:size]
			}
			if e[11]&attrDirectory == 0 {
				t.Fatalf("%s: %s is not a directory", p, comp)
			}
			clus, found = c, true
			break
		}
		if !found {
			t.Fatalf("%s: %s not found", p, comp)
		}
	}
	return nil
}

func TestBuildAndReadBack(t *testing.T) {
	sectors := MinSectors() + 1000
	img := make(memDisk, int(sectors)*sectorSize)
	big := bytes.Repeat([]byte("0123456789abcdef"), 4096) // spans many clusters
	files := []File{
		{Path: "EFI/BOOT/BOOTX64.EFI", Data: big},
		{Path: "EFI/NetBSD/boot.cfg", Data: []byte("timeout=0\n")},
		{Path: "EMPTY", Data: nil},
	}
	if err := Build(img, 0, sectors, files, Options{HiddenSectors: 2048, VolumeID: 0x1234, Label: "ESP"}); err != nil {
		t.Fatal(err)
	}
	r := newReader(t, img)
	if got := r.lookup(t, "EFI/BOOT/BOOTX64.EFI"); !bytes.Equal(got, big) {
		t.Errorf("BOOTX64.EFI: got %d bytes, want %d", len(got), len(big))
	}
	if got := string(r.lookup(t, "EFI/NETBSD/BOOT.CFG")); got != "timeout=0\n" {
		t.Errorf("BOOT.CFG = %q", got)
	}
	if got := r.lookup(t, "EMPTY"); len(got) != 0 {
		t.Errorf("EMPTY has %d bytes", len(got))
	}
	if binary.LittleEndian.Uint32(img[28:]) != 2048 {
		t.Error("hidden sectors not recorded")
	}
	if !bytes.Equal(img[0:sectorSize], img[6*sectorSize:7*sectorSize]) {
		t.Error("backup boot sector differs")
	}
}

func TestDeterministic(t *testing.T) {
	sectors := MinSectors()
	a := make(memDisk, int(sectors)*sectorSize)
	b := make(memDisk, int(sectors)*sectorSize)
	files := []File{{Path: "B.TXT", Data: []byte("b")}, {Path: "A.TXT", Data: []byte("a")}}
	if err := Build(a, 0, sectors, files, Options{}); err != nil {
		t.Fatal(err)
	}
	files[0], files[1] = files[1], files[0]
	if err := Build(b, 0, sectors, files, Options{}); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(a, b) {
		t.Fatal("output depends on input order")
	}
}

func TestRejects(t *testing.T) {
	sectors := MinSectors()
	for _, tc := range []struct {
		name    string
		sectors uint32
		files   []File
	}{
		{"too small", sectors - 1, nil},
		{"long name", sectors, []File{{Path: "LONGFILENAME.TXT"}}},
		{"bad char", sectors, []File{{Path: "A+B.TXT"}}},
		{"file as dir", sectors, []File{{Path: "A"}, {Path: "A/B"}}},
	} {
		img := make(memDisk, int(max(tc.sectors, sectors))*sectorSize)
		if err := Build(img, 0, tc.sectors, tc.files, Options{}); err == nil {
			t.Errorf("%s: expected error", tc.name)
		}
	}
}
