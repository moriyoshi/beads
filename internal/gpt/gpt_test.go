// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package gpt

import (
	"bytes"
	"encoding/binary"
	"hash/crc32"
	"testing"
)

type memDisk []byte

func (m memDisk) WriteAt(p []byte, off int64) (int, error) {
	return copy(m[off:], p), nil
}

func TestGUIDRoundTrip(t *testing.T) {
	const s = "C12A7328-F81F-11D2-BA4B-00A0C93EC93B"
	if got := MustParseGUID(s).String(); got != s {
		t.Fatalf("String() = %s, want %s", got, s)
	}
	var b [16]byte
	TypeEFISystem.putMixedEndian(b[:])
	want := []byte{0x28, 0x73, 0x2A, 0xC1, 0x1F, 0xF8, 0xD2, 0x11, 0xBA, 0x4B, 0x00, 0xA0, 0xC9, 0x3E, 0xC9, 0x3B}
	if !bytes.Equal(b[:], want) {
		t.Fatalf("mixed-endian = % x, want % x", b, want)
	}
}

func TestDerivedGUIDIsStable(t *testing.T) {
	a, b := DerivedGUID("x"), DerivedGUID("x")
	if a != b || a == DerivedGUID("y") {
		t.Fatal("DerivedGUID not deterministic or not seed-dependent")
	}
	if a[6]>>4 != 4 || a[8]>>6 != 2 {
		t.Fatalf("DerivedGUID %s lacks version/variant bits", a)
	}
}

func TestWriteTo(t *testing.T) {
	const total = 8192
	disk := make(memDisk, total*SectorSize)
	tbl := &Table{
		TotalSectors: total,
		DiskGUID:     DerivedGUID("disk"),
		MBRBootCode:  bytes.Repeat([]byte{0xCC}, 440),
		ActivePMBR:   true,
		Partitions: []Partition{
			{Type: TypeEFISystem, GUID: DerivedGUID("esp"), FirstLBA: 2048, LastLBA: 4095, Name: "EFI"},
			{Type: TypeNetBSDFFS, GUID: DerivedGUID("root"), FirstLBA: 4096, LastLBA: 8100, Attributes: AttrBootMe | AttrLegacyBIOSBootable, Name: "root"},
		},
	}
	if err := tbl.WriteTo(disk); err != nil {
		t.Fatal(err)
	}

	mbr := disk[:SectorSize]
	if mbr[0] != 0xCC || mbr[439] != 0xCC || mbr[440] != 0 {
		t.Error("MBR boot code not copied correctly")
	}
	if mbr[446] != 0x80 || mbr[450] != 0xEE || mbr[510] != 0x55 || mbr[511] != 0xAA {
		t.Error("bad protective MBR entry")
	}

	check := func(lba uint64, wantCurrent, wantEntries uint64) {
		h := disk[lba*SectorSize : lba*SectorSize+SectorSize]
		if string(h[:8]) != "EFI PART" {
			t.Fatalf("LBA %d: missing signature", lba)
		}
		hdr := append([]byte(nil), h[:headerSize]...)
		wantCRC := binary.LittleEndian.Uint32(hdr[16:])
		binary.LittleEndian.PutUint32(hdr[16:], 0)
		if crc32.ChecksumIEEE(hdr) != wantCRC {
			t.Errorf("LBA %d: header CRC mismatch", lba)
		}
		if got := binary.LittleEndian.Uint64(h[24:]); got != wantCurrent {
			t.Errorf("LBA %d: current LBA %d, want %d", lba, got, wantCurrent)
		}
		el := binary.LittleEndian.Uint64(h[72:])
		if el != wantEntries {
			t.Errorf("LBA %d: entries LBA %d, want %d", lba, el, wantEntries)
		}
		ents := disk[el*SectorSize : el*SectorSize+numEntries*entrySize]
		if crc32.ChecksumIEEE(ents) != binary.LittleEndian.Uint32(h[88:]) {
			t.Errorf("LBA %d: entries CRC mismatch", lba)
		}
		e1 := ents[entrySize : 2*entrySize]
		if binary.LittleEndian.Uint64(e1[32:]) != 4096 || binary.LittleEndian.Uint64(e1[48:]) != AttrBootMe|AttrLegacyBIOSBootable {
			t.Errorf("LBA %d: second entry wrong", lba)
		}
	}
	check(1, 1, 2)
	check(total-1, total-1, total-1-entrySectors)
}

func TestOverlapRejected(t *testing.T) {
	tbl := &Table{TotalSectors: 8192, Partitions: []Partition{
		{FirstLBA: 100, LastLBA: 200},
		{FirstLBA: 200, LastLBA: 300},
	}}
	if err := tbl.WriteTo(make(memDisk, 8192*SectorSize)); err == nil {
		t.Fatal("expected overlap error")
	}
}
