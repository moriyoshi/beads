// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package gpt writes GUID partition tables with a protective MBR.
//
// Only the subset needed for appliance disk images is implemented: a fixed
// sector size of 512 bytes, 128 partition entries, and deterministic GUIDs.
package gpt

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"unicode/utf16"
)

const (
	SectorSize = 512

	numEntries   = 128
	entrySize    = 128
	entrySectors = numEntries * entrySize / SectorSize // 32
	headerSize   = 92

	// Attribute bits understood by the NetBSD boot loaders
	// (sys/sys/disklabel_gpt.h).
	AttrLegacyBIOSBootable = 1 << 2
	AttrBootMe             = 1 << 59
)

// GUID is stored in the canonical (big-endian, textual) byte order and
// converted to the on-disk mixed-endian form when written.
type GUID [16]byte

var (
	TypeEFISystem = MustParseGUID("C12A7328-F81F-11D2-BA4B-00A0C93EC93B")
	TypeNetBSDFFS = MustParseGUID("49F48D5A-B10E-11DC-B99B-0019D1879648")
)

// MustParseGUID parses a textual GUID such as
// "C12A7328-F81F-11D2-BA4B-00A0C93EC93B" and panics on error.
func MustParseGUID(s string) GUID {
	g, err := ParseGUID(s)
	if err != nil {
		panic(err)
	}
	return g
}

// ParseGUID parses a textual GUID.
func ParseGUID(s string) (GUID, error) {
	var g GUID
	if len(s) != 36 || s[8] != '-' || s[13] != '-' || s[18] != '-' || s[23] != '-' {
		return g, fmt.Errorf("gpt: malformed GUID %q", s)
	}
	j := 0
	for i := 0; i < len(s); {
		if s[i] == '-' {
			i++
			continue
		}
		hi, ok1 := unhex(s[i])
		lo, ok2 := unhex(s[i+1])
		if !ok1 || !ok2 {
			return g, fmt.Errorf("gpt: malformed GUID %q", s)
		}
		g[j] = hi<<4 | lo
		j++
		i += 2
	}
	return g, nil
}

func unhex(c byte) (byte, bool) {
	switch {
	case '0' <= c && c <= '9':
		return c - '0', true
	case 'a' <= c && c <= 'f':
		return c - 'a' + 10, true
	case 'A' <= c && c <= 'F':
		return c - 'A' + 10, true
	}
	return 0, false
}

func (g GUID) String() string {
	return fmt.Sprintf("%X-%X-%X-%X-%X", g[0:4], g[4:6], g[6:8], g[8:10], g[10:16])
}

// DerivedGUID returns a version-4-shaped GUID derived from seed, so that
// images built from identical inputs are bit-for-bit identical.
func DerivedGUID(seed string) GUID {
	sum := sha256.Sum256([]byte(seed))
	var g GUID
	copy(g[:], sum[:16])
	g[6] = g[6]&0x0f | 0x40
	g[8] = g[8]&0x3f | 0x80
	return g
}

// putMixedEndian writes g in the on-disk layout: the first three fields
// little-endian, the remaining eight bytes as-is.
func (g GUID) putMixedEndian(b []byte) {
	b[0], b[1], b[2], b[3] = g[3], g[2], g[1], g[0]
	b[4], b[5] = g[5], g[4]
	b[6], b[7] = g[7], g[6]
	copy(b[8:16], g[8:16])
}

// Partition describes one GPT entry. LBAs are inclusive.
type Partition struct {
	Type       GUID
	GUID       GUID
	FirstLBA   uint64
	LastLBA    uint64
	Attributes uint64
	Name       string
}

// Table is a complete partition table for a disk of TotalSectors sectors.
type Table struct {
	TotalSectors uint64
	DiskGUID     GUID
	Partitions   []Partition
	// MBRBootCode, if non-nil, is copied into the first 440 bytes of the
	// protective MBR (e.g. NetBSD's /usr/mdec/gptmbr.bin).
	MBRBootCode []byte
	// ActivePMBR marks the protective MBR entry active, which some BIOSes
	// require before they will run the MBR boot code.
	ActivePMBR bool
}

// FirstUsableLBA is the first sector a partition may occupy.
func FirstUsableLBA() uint64 { return 2 + entrySectors }

// LastUsableLBA is the last sector a partition may occupy on a disk of the
// given size.
func LastUsableLBA(totalSectors uint64) uint64 { return totalSectors - 2 - entrySectors }

func (t *Table) validate() error {
	if t.TotalSectors < 2*(2+entrySectors)+1 {
		return errors.New("gpt: disk too small")
	}
	if len(t.Partitions) > numEntries {
		return errors.New("gpt: too many partitions")
	}
	if len(t.MBRBootCode) > 440 {
		return errors.New("gpt: MBR boot code longer than 440 bytes")
	}
	lo, hi := FirstUsableLBA(), LastUsableLBA(t.TotalSectors)
	for i, p := range t.Partitions {
		if p.FirstLBA < lo || p.LastLBA > hi || p.FirstLBA > p.LastLBA {
			return fmt.Errorf("gpt: partition %d [%d,%d] outside usable range [%d,%d]", i, p.FirstLBA, p.LastLBA, lo, hi)
		}
		for j := 0; j < i; j++ {
			q := t.Partitions[j]
			if p.FirstLBA <= q.LastLBA && q.FirstLBA <= p.LastLBA {
				return fmt.Errorf("gpt: partitions %d and %d overlap", j, i)
			}
		}
		if len(utf16.Encode([]rune(p.Name))) > 36 {
			return fmt.Errorf("gpt: partition %d name too long", i)
		}
	}
	return nil
}

func (t *Table) entries() []byte {
	buf := make([]byte, numEntries*entrySize)
	for i, p := range t.Partitions {
		e := buf[i*entrySize : (i+1)*entrySize]
		p.Type.putMixedEndian(e[0:16])
		p.GUID.putMixedEndian(e[16:32])
		binary.LittleEndian.PutUint64(e[32:], p.FirstLBA)
		binary.LittleEndian.PutUint64(e[40:], p.LastLBA)
		binary.LittleEndian.PutUint64(e[48:], p.Attributes)
		for k, u := range utf16.Encode([]rune(p.Name)) {
			binary.LittleEndian.PutUint16(e[56+2*k:], u)
		}
	}
	return buf
}

func (t *Table) header(current, backup, entriesLBA uint64, entriesCRC uint32) []byte {
	h := make([]byte, SectorSize)
	copy(h[0:8], "EFI PART")
	binary.LittleEndian.PutUint32(h[8:], 0x00010000)
	binary.LittleEndian.PutUint32(h[12:], headerSize)
	binary.LittleEndian.PutUint64(h[24:], current)
	binary.LittleEndian.PutUint64(h[32:], backup)
	binary.LittleEndian.PutUint64(h[40:], FirstUsableLBA())
	binary.LittleEndian.PutUint64(h[48:], LastUsableLBA(t.TotalSectors))
	t.DiskGUID.putMixedEndian(h[56:72])
	binary.LittleEndian.PutUint64(h[72:], entriesLBA)
	binary.LittleEndian.PutUint32(h[80:], numEntries)
	binary.LittleEndian.PutUint32(h[84:], entrySize)
	binary.LittleEndian.PutUint32(h[88:], entriesCRC)
	binary.LittleEndian.PutUint32(h[16:], crc32.ChecksumIEEE(h[:headerSize]))
	return h
}

func (t *Table) pmbr() []byte {
	m := make([]byte, SectorSize)
	copy(m, t.MBRBootCode)
	e := m[446:462]
	if t.ActivePMBR {
		e[0] = 0x80
	}
	e[1], e[2], e[3] = 0x00, 0x02, 0x00 // CHS of LBA 1
	e[4] = 0xEE
	e[5], e[6], e[7] = 0xFF, 0xFF, 0xFF
	binary.LittleEndian.PutUint32(e[8:], 1)
	size := t.TotalSectors - 1
	if size > 0xFFFFFFFF {
		size = 0xFFFFFFFF
	}
	binary.LittleEndian.PutUint32(e[12:], uint32(size))
	m[510], m[511] = 0x55, 0xAA
	return m
}

// WriteTo writes the protective MBR, the primary header and entries, and the
// backup entries and header. It does not touch partition contents.
func (t *Table) WriteTo(w io.WriterAt) error {
	if err := t.validate(); err != nil {
		return err
	}
	ents := t.entries()
	crc := crc32.ChecksumIEEE(ents)
	last := t.TotalSectors - 1
	backupEntries := last - entrySectors

	writes := []struct {
		lba  uint64
		data []byte
	}{
		{0, t.pmbr()},
		{1, t.header(1, last, 2, crc)},
		{2, ents},
		{backupEntries, ents},
		{last, t.header(last, 1, backupEntries, crc)},
	}
	for _, wr := range writes {
		if _, err := w.WriteAt(wr.data, int64(wr.lba)*SectorSize); err != nil {
			return err
		}
	}
	return nil
}
