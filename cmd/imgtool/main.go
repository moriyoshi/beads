// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Command imgtool is the build-time helper behind the Bazel image rules.
//
//	imgtool rootfs --manifest m.json --makefs nbmakefs --installboot nbinstallboot --bootxx bootxx_ffsv2 --out rootfs.ffs
//	imgtool disk   --rootfs rootfs.ffs --efiboot bootx64.efi --gptmbr gptmbr.bin --out disk.raw
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/exec"
	"strconv"

	"github.com/moriyoshi/beads/internal/disk"
	"github.com/moriyoshi/beads/internal/rootfs"
)

// manifest is the rootfs description written by the netbsd_rootfs rule.
type manifest struct {
	Arch    string         `json:"arch"`
	Entries []rootfs.Entry `json:"entries"`
}

// bootCfg returns the boot.cfg shared by the BIOS (/boot.cfg on the root
// file system) and UEFI (EFI/NetBSD/boot.cfg on the ESP) loaders.
// An empty console leaves the choice to the firmware and kernel (arm64,
// where the console comes from ACPI SPCR or the device tree).
func bootCfg(console string) []byte {
	cfg := "menu=Boot appliance:boot netbsd\ntimeout=0\n"
	if console != "" {
		cfg += "consdev=" + console + "\n"
	}
	return []byte(cfg)
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: imgtool {rootfs|disk} [flags]")
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "rootfs":
		err = cmdRootfs(os.Args[2:])
	case "disk":
		err = cmdDisk(os.Args[2:])
	default:
		err = fmt.Errorf("unknown subcommand %q", os.Args[1])
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "imgtool:", err)
		os.Exit(1)
	}
}

func cmdRootfs(args []string) error {
	fs := flag.NewFlagSet("rootfs", flag.ExitOnError)
	manifestPath := fs.String("manifest", "", "JSON manifest of entries")
	makefs := fs.String("makefs", "", "path to makefs")
	installboot := fs.String("installboot", "", "path to installboot")
	bootxx := fs.String("bootxx", "", "primary bootstrap (bootxx_ffsv2)")
	console := fs.String("console", "com0", "boot loader and kernel console")
	size := fs.Int64("size", 0, "fixed image size in bytes (0 = minimum plus headroom)")
	epoch := fs.Int64("epoch", 0, "timestamp for every file")
	out := fs.String("out", "", "output image")
	specOut := fs.String("spec-out", "", "also write the mtree spec here")
	fs.Parse(args)

	b, err := os.ReadFile(*manifestPath)
	if err != nil {
		return err
	}
	var m manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return fmt.Errorf("%s: %w", *manifestPath, err)
	}
	t := rootfs.NewTree()
	if err := rootfs.AddBase(t, m.Arch); err != nil {
		return err
	}
	for _, e := range m.Entries {
		if err := t.Add(e); err != nil {
			return err
		}
	}
	if err := t.Add(rootfs.Entry{Path: "/boot.cfg", Type: rootfs.TypeFile, Content: string(bootCfg(*console))}); err != nil {
		return err
	}
	if err := t.Build(*out, *specOut, rootfs.MakefsOptions{Makefs: *makefs, SizeBytes: *size, Epoch: *epoch}); err != nil {
		return err
	}
	if *bootxx == "" {
		return nil
	}
	// The x86 primary bootstrap carries the console settings that /boot
	// uses until it has read boot.cfg.
	ib := exec.Command(*installboot, "-m", m.Arch, "-o", "console="+*console+",speed=115200,timeout=0", *out, *bootxx)
	ib.Stdout, ib.Stderr = os.Stderr, os.Stderr
	if err := ib.Run(); err != nil {
		return fmt.Errorf("installboot: %w", err)
	}
	return nil
}

func cmdDisk(args []string) error {
	fs := flag.NewFlagSet("disk", flag.ExitOnError)
	root := fs.String("rootfs", "", "FFS root image with bootxx installed")
	efiboot := fs.String("efiboot", "", "bootx64.efi")
	gptmbr := fs.String("gptmbr", "", "gptmbr.bin (omit for a UEFI-only image)")
	efiName := fs.String("efi-name", "BOOTX64.EFI", "loader file name under EFI/BOOT on the ESP")
	console := fs.String("console", "com0", "boot loader and kernel console")
	espMiB := fs.Int("esp-size-mib", disk.DefaultESPSizeMiB, "EFI system partition size")
	minSize := fs.String("min-size", "0", "minimum disk size in bytes")
	out := fs.String("out", "", "output raw disk image")
	fs.Parse(args)

	efi, err := os.ReadFile(*efiboot)
	if err != nil {
		return err
	}
	var mbr []byte
	if *gptmbr != "" {
		if mbr, err = os.ReadFile(*gptmbr); err != nil {
			return err
		}
	}
	min, err := strconv.ParseInt(*minSize, 10, 64)
	if err != nil {
		return fmt.Errorf("--min-size: %w", err)
	}
	_, err = disk.Assemble(*out, disk.Options{
		RootFS: *root, EFIBoot: efi, EFIName: *efiName, GPTMBR: mbr, BootCfg: bootCfg(*console),
		ESPSizeMiB: *espMiB, MinSize: min,
	})
	return err
}
