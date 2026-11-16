// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package metadata

import (
	"encoding/binary"
	"fmt"
	"strings"
)

// QEMU's firmware configuration device (fw_cfg) is how a QEMU guest gets
// per-boot settings without a metadata service:
//
//	qemu ... -fw_cfg name=opt/beads/user-data,file=user-data.txt
//	qemu ... -fw_cfg name=opt/beads/time-servers,string=10.0.2.2:12345
//
// FWCfgPrefix is the directory these entries live in; QEMU reserves "opt/"
// for users and the rest of the name is ours.
const FWCfgPrefix = "opt/beads/"

const (
	fwcfgFileDir   = 0x19
	fwcfgEntrySize = 64
	fwcfgNameLen   = 56
	// fwcfgMaxFile bounds what is read from one entry.
	fwcfgMaxFile = 1 << 20
)

// FWCfg is a fw_cfg device: select an item, then read its bytes from the
// start.
type FWCfg interface {
	Select(item uint16) error
	Read(n int) ([]byte, error)
}

type fwcfgFile struct {
	size uint32
	sel  uint16
}

func readDir(dev FWCfg) (map[string]fwcfgFile, error) {
	if err := dev.Select(fwcfgFileDir); err != nil {
		return nil, err
	}
	hdr, err := dev.Read(4)
	if err != nil {
		return nil, err
	}
	count := binary.BigEndian.Uint32(hdr)
	if count > 4096 {
		return nil, fmt.Errorf("fw_cfg: implausible directory size %d", count)
	}
	ents, err := dev.Read(int(count) * fwcfgEntrySize)
	if err != nil {
		return nil, err
	}
	dir := map[string]fwcfgFile{}
	for i := 0; i < int(count); i++ {
		e := ents[i*fwcfgEntrySize : (i+1)*fwcfgEntrySize]
		name := string(e[8 : 8+fwcfgNameLen])
		if j := strings.IndexByte(name, 0); j >= 0 {
			name = name[:j]
		}
		dir[name] = fwcfgFile{size: binary.BigEndian.Uint32(e[0:]), sel: binary.BigEndian.Uint16(e[4:])}
	}
	return dir, nil
}

// ReadFWCfg returns the entries under FWCfgPrefix, keyed by the rest of
// their names.
func ReadFWCfg(dev FWCfg) (map[string][]byte, error) {
	dir, err := readDir(dev)
	if err != nil {
		return nil, err
	}
	out := map[string][]byte{}
	for name, f := range dir {
		key, ok := strings.CutPrefix(name, FWCfgPrefix)
		if !ok || key == "" {
			continue
		}
		if f.size > fwcfgMaxFile {
			return nil, fmt.Errorf("fw_cfg: %s is %d bytes", name, f.size)
		}
		if err := dev.Select(f.sel); err != nil {
			return nil, err
		}
		b, err := dev.Read(int(f.size))
		if err != nil {
			return nil, fmt.Errorf("fw_cfg: %s: %w", name, err)
		}
		out[key] = b
	}
	return out, nil
}

// QEMUInfo builds the instance description from fw_cfg entries:
// "user-data", "time-servers" (comma- or whitespace-separated) and
// "otlp-endpoint" (an OTLP/HTTP base URL for logs).
func QEMUInfo(entries map[string][]byte) *Info {
	info := &Info{Platform: QEMU}
	if ud, ok := entries["user-data"]; ok {
		info.UserData = ud
	}
	if ts, ok := entries["time-servers"]; ok {
		info.TimeServers = strings.FieldsFunc(string(ts), func(r rune) bool {
			return r == ',' || r == ' ' || r == '\n' || r == '\t' || r == '\r'
		})
	}
	if ep, ok := entries["otlp-endpoint"]; ok {
		info.OTLPEndpoint = strings.TrimSpace(string(ep))
	}
	return info
}
