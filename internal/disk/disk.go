// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package disk assembles a bootable hybrid BIOS/UEFI disk image from a
// NetBSD root file system.
//
// Layout (1 MiB aligned):
//
//	LBA 0       protective MBR carrying gptmbr.bin (BIOS stage 0)
//	LBA 1..33   primary GPT
//	1 MiB       EFI system partition (FAT32): EFI/BOOT/BOOT<arch>.EFI and boot.cfg
//	...         NetBSD FFS root, with bootxx_ffsv2 already installed by installboot
//	end         backup GPT
//
// BIOS firmware runs gptmbr.bin, which chain-loads the partition marked
// legacy-BIOS-bootable; UEFI firmware runs the EFI loader, which loads the
// kernel from the partition marked bootme (or the first FFS partition).
// Without gptmbr.bin (arm64) the image is UEFI-only.
package disk

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/moriyoshi/beads/internal/fat32"
	"github.com/moriyoshi/beads/internal/gpt"
)

const (
	mib        = 1 << 20
	alignSects = mib / gpt.SectorSize

	// DefaultESPSizeMiB leaves headroom over the FAT32 minimum (~33 MiB).
	DefaultESPSizeMiB = 40
)

// Options describes the inputs of an image.
type Options struct {
	RootFS  string // FFS image (with the x86 primary bootstrap installed)
	EFIBoot []byte // NetBSD EFI loader: bootx64.efi or bootaa64.efi
	// EFIName is the loader's file name under EFI/BOOT, the removable-media
	// path firmware boots by default: BOOTX64.EFI (default) or BOOTAA64.EFI.
	EFIName string
	GPTMBR  []byte // NetBSD gptmbr.bin; nil for a UEFI-only image
	BootCfg []byte // boot.cfg read by the EFI loader from the ESP

	ESPSizeMiB int   // 0 means DefaultESPSizeMiB
	MinSize    int64 // pad the disk to at least this many bytes
}

// Layout records where things ended up, for callers and tests.
type Layout struct {
	TotalSectors uint64
	ESPFirstLBA  uint64
	ESPLastLBA   uint64
	RootFirstLBA uint64
	RootLastLBA  uint64
}

func roundUp(n, m uint64) uint64 { return (n + m - 1) / m * m }

// Assemble writes the disk image to out.
func Assemble(out string, o Options) (*Layout, error) {
	if len(o.EFIBoot) == 0 {
		return nil, fmt.Errorf("disk: an EFI loader is required")
	}
	efiName := o.EFIName
	if efiName == "" {
		efiName = "BOOTX64.EFI"
	}
	mbrCode := o.GPTMBR
	if len(mbrCode) > 440 {
		mbrCode = mbrCode[:440] // the rest is a placeholder partition table
	}

	root, err := os.Open(o.RootFS)
	if err != nil {
		return nil, err
	}
	defer root.Close()
	st, err := root.Stat()
	if err != nil {
		return nil, err
	}
	h := sha256.New()
	if _, err := io.Copy(h, root); err != nil {
		return nil, err
	}
	h.Write(o.EFIBoot)
	h.Write(o.BootCfg)
	seed := hex.EncodeToString(h.Sum(nil))

	espMiB := o.ESPSizeMiB
	if espMiB == 0 {
		espMiB = DefaultESPSizeMiB
	}
	espSects := uint64(espMiB) * alignSects
	if espSects < uint64(fat32.MinSectors()) {
		return nil, fmt.Errorf("disk: ESP of %d MiB is below the FAT32 minimum", espMiB)
	}
	rootSects := roundUp(uint64(st.Size()), gpt.SectorSize) / gpt.SectorSize

	l := &Layout{ESPFirstLBA: alignSects}
	l.ESPLastLBA = l.ESPFirstLBA + espSects - 1
	l.RootFirstLBA = l.ESPLastLBA + 1
	l.RootLastLBA = l.RootFirstLBA + rootSects - 1
	// One aligned MiB at the end holds the backup GPT.
	l.TotalSectors = roundUp(l.RootLastLBA+1, alignSects) + alignSects
	if min := roundUp(uint64(o.MinSize), mib) / gpt.SectorSize; min > l.TotalSectors {
		l.TotalSectors = min
	}

	f, err := os.Create(out)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := f.Truncate(int64(l.TotalSectors) * gpt.SectorSize); err != nil {
		return nil, err
	}

	volGUID := gpt.DerivedGUID(seed + "/esp-volid")
	volID := binary.LittleEndian.Uint32(volGUID[:4])
	// The x86 loader reads EFI/NetBSD/boot.cfg; the MI one (arm64) reads
	// boot.cfg next to itself.
	espFiles := []fat32.File{
		{Path: "EFI/BOOT/" + efiName, Data: o.EFIBoot},
		{Path: "EFI/BOOT/BOOT.CFG", Data: o.BootCfg},
		{Path: "EFI/NETBSD/BOOT.CFG", Data: o.BootCfg},
	}
	rootAttrs := uint64(gpt.AttrBootMe)
	if len(mbrCode) > 0 {
		rootAttrs |= gpt.AttrLegacyBIOSBootable
	}
	if err := fat32.Build(f, int64(l.ESPFirstLBA)*gpt.SectorSize, uint32(espSects), espFiles,
		fat32.Options{HiddenSectors: uint32(l.ESPFirstLBA), VolumeID: volID, Label: "EFI"}); err != nil {
		return nil, err
	}

	if _, err := root.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	if _, err := io.Copy(io.NewOffsetWriter(f, int64(l.RootFirstLBA)*gpt.SectorSize), root); err != nil {
		return nil, err
	}

	tbl := &gpt.Table{
		TotalSectors: l.TotalSectors,
		DiskGUID:     gpt.DerivedGUID(seed + "/disk"),
		MBRBootCode:  mbrCode,
		ActivePMBR:   len(mbrCode) > 0,
		Partitions: []gpt.Partition{
			{
				Type: gpt.TypeEFISystem, GUID: gpt.DerivedGUID(seed + "/esp"),
				FirstLBA: l.ESPFirstLBA, LastLBA: l.ESPLastLBA, Name: "EFI",
			},
			{
				Type: gpt.TypeNetBSDFFS, GUID: gpt.DerivedGUID(seed + "/root"),
				FirstLBA: l.RootFirstLBA, LastLBA: l.RootLastLBA, Name: "root",
				Attributes: rootAttrs,
			},
		},
	}
	if err := tbl.WriteTo(f); err != nil {
		return nil, err
	}
	return l, f.Close()
}
