// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package metadata

import (
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

// fakeFWCfg serves a file directory and its items the way QEMU does.
type fakeFWCfg struct {
	items map[uint16][]byte
	cur   []byte
}

func newFakeFWCfg(files map[string]string) *fakeFWCfg {
	f := &fakeFWCfg{items: map[uint16][]byte{}}
	dir := binary.BigEndian.AppendUint32(nil, uint32(len(files)))
	sel := uint16(0x20)
	for _, name := range []string{"etc/boot-fail-wait", "opt/beads/otlp-endpoint", "opt/beads/time-servers", "opt/beads/user-data", "opt/other/x"} {
		data, ok := files[name]
		if !ok {
			continue
		}
		e := make([]byte, fwcfgEntrySize)
		binary.BigEndian.PutUint32(e[0:], uint32(len(data)))
		binary.BigEndian.PutUint16(e[4:], sel)
		copy(e[8:], name)
		dir = append(dir, e...)
		f.items[sel] = []byte(data)
		sel++
	}
	f.items[fwcfgFileDir] = dir
	return f
}

func (f *fakeFWCfg) Select(item uint16) error {
	b, ok := f.items[item]
	if !ok {
		return errors.New("no such item")
	}
	f.cur = b
	return nil
}

func (f *fakeFWCfg) Read(n int) ([]byte, error) {
	if n > len(f.cur) {
		return nil, errors.New("short read")
	}
	b := f.cur[:n]
	f.cur = f.cur[n:]
	return b, nil
}

func TestReadFWCfg(t *testing.T) {
	dev := newFakeFWCfg(map[string]string{
		"etc/boot-fail-wait":      "\x05\x00\x00\x00",
		"opt/beads/time-servers":  "10.0.2.2:12345, 10.0.2.3\n",
		"opt/beads/user-data":     "hello",
		"opt/beads/otlp-endpoint": "http://10.0.2.2:4318\n",
		"opt/other/x":             "ignored",
	})
	entries, err := ReadFWCfg(dev)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 || string(entries["user-data"]) != "hello" {
		t.Fatalf("entries %q", entries)
	}
	info := QEMUInfo(entries)
	if want := []string{"10.0.2.2:12345", "10.0.2.3"}; !reflect.DeepEqual(info.TimeServers, want) {
		t.Errorf("time servers %q, want %q", info.TimeServers, want)
	}
	if info.Platform != QEMU || string(info.UserData) != "hello" || info.OTLPEndpoint != "http://10.0.2.2:4318" {
		t.Errorf("info %+v", info)
	}
}

func TestReadFWCfgEmpty(t *testing.T) {
	entries, err := ReadFWCfg(newFakeFWCfg(nil))
	if err != nil || len(entries) != 0 {
		t.Errorf("entries %v, err %v", entries, err)
	}
	if info := QEMUInfo(entries); info.UserData != nil || info.TimeServers != nil {
		t.Errorf("info %+v", info)
	}
}
