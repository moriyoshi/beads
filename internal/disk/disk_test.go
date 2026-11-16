// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package disk

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestAssemble(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "root.ffs")
	payload := bytes.Repeat([]byte("ffs!"), 3<<18) // 3 MiB, not MiB-aligned after rounding below
	payload = append(payload, 1, 2, 3)
	if err := os.WriteFile(root, payload, 0o644); err != nil {
		t.Fatal(err)
	}
	opts := Options{
		RootFS:  root,
		EFIBoot: []byte("MZ fake efi"),
		GPTMBR:  bytes.Repeat([]byte{0xFA}, 440),
		BootCfg: []byte("timeout=0\n"),
	}
	out := filepath.Join(dir, "disk.raw")
	l, err := Assemble(out, opts)
	if err != nil {
		t.Fatal(err)
	}
	img, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if int64(len(img)) != int64(l.TotalSectors)*512 || len(img)%mib != 0 {
		t.Errorf("image is %d bytes; want %d sectors, MiB aligned", len(img), l.TotalSectors)
	}
	if img[0] != 0xFA || img[510] != 0x55 || string(img[512:520]) != "EFI PART" {
		t.Error("bad MBR or primary GPT header")
	}
	if string(img[len(img)-512:len(img)-504]) != "EFI PART" {
		t.Error("missing backup GPT header")
	}
	esp := img[l.ESPFirstLBA*512:]
	if string(esp[82:90]) != "FAT32   " {
		t.Error("ESP is not FAT32")
	}
	if got := img[l.RootFirstLBA*512 : l.RootFirstLBA*512+uint64(len(payload))]; !bytes.Equal(got, payload) {
		t.Error("root file system not copied verbatim")
	}
	if l.ESPFirstLBA%2048 != 0 || l.RootFirstLBA%2048 != 0 {
		t.Errorf("partitions not 1 MiB aligned: %+v", l)
	}

	// Identical inputs give identical images.
	out2 := filepath.Join(dir, "disk2.raw")
	if _, err := Assemble(out2, opts); err != nil {
		t.Fatal(err)
	}
	img2, _ := os.ReadFile(out2)
	if !bytes.Equal(img, img2) {
		t.Error("Assemble is not deterministic")
	}

	// UEFI-only (arm64): no MBR boot code, a different loader name.
	uefi := opts
	uefi.GPTMBR, uefi.EFIName = nil, "BOOTAA64.EFI"
	out4 := filepath.Join(dir, "disk4.raw")
	if _, err := Assemble(out4, uefi); err != nil {
		t.Fatal(err)
	}
	img4, _ := os.ReadFile(out4)
	if img4[0] != 0 || img4[446] != 0 || img4[450] != 0xEE {
		t.Error("UEFI-only image has MBR boot code or an active protective MBR")
	}
	if !bytes.Contains(img4[l.ESPFirstLBA*512:l.RootFirstLBA*512], []byte("BOOTAA64EFI")) {
		t.Error("ESP lacks BOOTAA64.EFI")
	}

	opts.MinSize = 64 * mib
	l3, err := Assemble(filepath.Join(dir, "disk3.raw"), opts)
	if err != nil {
		t.Fatal(err)
	}
	if l3.TotalSectors != 64*mib/512 {
		t.Errorf("MinSize ignored: %d sectors", l3.TotalSectors)
	}
}
