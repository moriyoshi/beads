// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package rootfs

import "fmt"

// Character device majors, from sys/arch/amd64/conf/majors.amd64 and
// sys/arch/aarch64/conf/majors.aarch64. Minors follow etc/MAKEDEV.tmpl.
var charMajors = map[string]map[string]uint32{
	"amd64": {
		"cons":     0,
		"ctty":     1,
		"mem":      2,
		"log":      7,
		"filedesc": 22,
		"bpf":      23,
		"rnd":      46,
		"sysmon":   67,
		// qemufwcfg's major comes from the machine-independent sys/conf/majors.
		"qemufwcfg": 342,
	},
	"aarch64": {
		"mem":       0,
		"cons":      2,
		"ctty":      3,
		"log":       5,
		"bpf":       32,
		"filedesc":  34,
		"rnd":       52,
		"sysmon":    101,
		"qemufwcfg": 342,
	},
}

type devNode struct {
	name  string
	major string
	minor uint32
	mode  uint32
}

// The device nodes the init shim and a static Go program need. Disks are not
// listed: the kernel mounts the root file system by itself and nothing else
// opens a disk.
var devNodes = []devNode{
	{"console", "cons", 0, 0o600},
	{"tty", "ctty", 0, 0o666},
	{"null", "mem", 2, 0o666},
	{"zero", "mem", 12, 0o666},
	{"full", "mem", 11, 0o666},
	{"klog", "log", 0, 0o600},
	{"stdin", "filedesc", 0, 0o666},
	{"stdout", "filedesc", 1, 0o666},
	{"stderr", "filedesc", 2, 0o666},
	{"bpf", "bpf", 0, 0o600},
	{"random", "rnd", 0, 0o444},
	{"urandom", "rnd", 1, 0o644},
	{"power", "sysmon", 2, 0o640},
	{"qemufwcfg", "qemufwcfg", 0, 0o400},
}

const etcHosts = `127.0.0.1	localhost
::1		localhost
`

const etcNsswitch = `hosts:		files dns
`

// AddBase adds the directories, device nodes and /etc files every appliance
// has. Writable locations (/tmp, /var) are mount points for tmpfs set up by
// the init shim; /etc/resolv.conf points into /var so DHCP can update it while
// the root file system stays read-only.
func AddBase(t *Tree, arch string) error {
	majors, ok := charMajors[arch]
	if !ok {
		return fmt.Errorf("rootfs: unsupported arch %q", arch)
	}
	entries := []Entry{
		{Path: "/dev", Type: TypeDir},
		{Path: "/etc", Type: TypeDir},
		{Path: "/sbin", Type: TypeDir},
		{Path: "/app", Type: TypeDir},
		{Path: "/kern", Type: TypeDir, Mode: 0o555},
		{Path: "/tmp", Type: TypeDir, Mode: 0o1777},
		{Path: "/var", Type: TypeDir},
		{Path: "/etc/hosts", Type: TypeFile, Content: etcHosts},
		{Path: "/etc/nsswitch.conf", Type: TypeFile, Content: etcNsswitch},
		{Path: "/etc/resolv.conf", Type: TypeLink, Target: "/var/run/resolv.conf"},
	}
	for _, d := range devNodes {
		major, ok := majors[d.major]
		if !ok {
			return fmt.Errorf("rootfs: no %s major for %s", d.major, arch)
		}
		entries = append(entries, Entry{Path: "/dev/" + d.name, Type: TypeChar, Mode: d.mode, Major: major, Minor: d.minor})
	}
	for _, e := range entries {
		if err := t.Add(e); err != nil {
			return err
		}
	}
	return nil
}
