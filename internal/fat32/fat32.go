// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Package fat32 writes small, deterministic FAT32 file systems such as EFI
// system partitions.
//
// Files are laid out contiguously, only 8.3 names are supported (no long
// file name entries), and every timestamp is fixed at 1980-01-01 so that
// identical inputs produce identical images.
package fat32

import (
	"encoding/binary"
	"fmt"
	"io"
	"path"
	"sort"
	"strings"
)

const (
	sectorSize        = 512
	sectorsPerCluster = 1
	clusterSize       = sectorSize * sectorsPerCluster
	reservedSectors   = 32
	numFATs           = 2
	dirEntrySize      = 32
	minClusters       = 65525 // below this the volume is FAT16 by definition

	attrDirectory = 0x10
	attrArchive   = 0x20
	attrVolumeID  = 0x08

	eoc        = 0x0FFFFFFF
	fatDate    = 1<<5 | 1 // 1980-01-01
	fsInfoSig1 = 0x41615252
	fsInfoSig2 = 0x61417272
	fsInfoSig3 = 0xAA550000
)

// File is a regular file to place in the file system. Path uses '/'
// separators and must consist of 8.3 components, e.g. "EFI/BOOT/BOOTX64.EFI".
type File struct {
	Path string
	Data []byte
}

// Options controls volume-level metadata.
type Options struct {
	// HiddenSectors is the LBA at which the volume starts on its disk.
	HiddenSectors uint32
	VolumeID      uint32
	// Label is up to 11 characters; it defaults to "NO NAME".
	Label string
}

// MinSectors is the smallest volume size, in 512-byte sectors, that Build
// accepts.
func MinSectors() uint32 {
	s, _ := layoutFor(minClusters)
	return s
}

// layoutFor returns the total sector count needed for n clusters and the
// resulting FAT size.
func layoutFor(n uint32) (total, fatSz uint32) {
	fatSz = ((n+2)*4 + sectorSize - 1) / sectorSize
	return reservedSectors + numFATs*fatSz + n*sectorsPerCluster, fatSz
}

type node struct {
	name     [11]byte
	isDir    bool
	data     []byte
	children []*node
	cluster  uint32
	nclust   uint32
}

func shortName(component string) ([11]byte, error) {
	var n [11]byte
	for i := range n {
		n[i] = ' '
	}
	base, ext, _ := strings.Cut(component, ".")
	if base == "" || len(base) > 8 || len(ext) > 3 || strings.Contains(ext, ".") {
		return n, fmt.Errorf("fat32: %q is not an 8.3 name", component)
	}
	valid := func(s string) bool {
		for _, c := range s {
			if !('A' <= c && c <= 'Z' || '0' <= c && c <= '9' || strings.ContainsRune("!#$%&'()-@^_`{}~", c)) {
				return false
			}
		}
		return true
	}
	base, ext = strings.ToUpper(base), strings.ToUpper(ext)
	if !valid(base) || !valid(ext) {
		return n, fmt.Errorf("fat32: %q contains characters not allowed in short names", component)
	}
	copy(n[:8], base)
	copy(n[8:], ext)
	return n, nil
}

func buildTree(files []File) (*node, error) {
	root := &node{isDir: true}
	dirs := map[string]*node{"": root}
	var mkdir func(p string) (*node, error)
	mkdir = func(p string) (*node, error) {
		if d, ok := dirs[p]; ok {
			if !d.isDir {
				return nil, fmt.Errorf("fat32: %s is a file", p)
			}
			return d, nil
		}
		parent, err := mkdir(path.Dir(p))
		if p == "." || err != nil {
			return root, err
		}
		nm, err := shortName(path.Base(p))
		if err != nil {
			return nil, err
		}
		d := &node{name: nm, isDir: true}
		parent.children = append(parent.children, d)
		dirs[p] = d
		return d, nil
	}
	dirs["."] = root
	for _, f := range files {
		p := strings.Trim(path.Clean("/"+f.Path), "/")
		if p == "" {
			return nil, fmt.Errorf("fat32: empty path")
		}
		if _, dup := dirs[p]; dup {
			return nil, fmt.Errorf("fat32: duplicate path %s", p)
		}
		parent, err := mkdir(path.Dir(p))
		if err != nil {
			return nil, err
		}
		nm, err := shortName(path.Base(p))
		if err != nil {
			return nil, err
		}
		n := &node{name: nm, data: f.Data}
		parent.children = append(parent.children, n)
		dirs[p] = n
	}
	var sortAll func(*node)
	sortAll = func(n *node) {
		sort.Slice(n.children, func(i, j int) bool {
			return string(n.children[i].name[:]) < string(n.children[j].name[:])
		})
		for _, c := range n.children {
			sortAll(c)
		}
	}
	sortAll(root)
	return root, nil
}

func ceilDiv(a, b uint32) uint32 { return (a + b - 1) / b }

// Build writes a FAT32 file system of exactly sectors sectors to w at byte
// offset off.
func Build(w io.WriterAt, off int64, sectors uint32, files []File, opt Options) error {
	root, err := buildTree(files)
	if err != nil {
		return err
	}

	if sectors <= reservedSectors+numFATs {
		return fmt.Errorf("fat32: %d sectors is too small", sectors)
	}
	// Find the smallest FAT that can map every cluster left over after it.
	// Each data sector costs 4 FAT bytes per FAT copy, which gives a lower
	// bound to start the search from.
	fatSz := max(1, (sectors-reservedSectors)*4/(sectorSize*sectorsPerCluster+numFATs*4))
	var clusters uint32
	for {
		clusters = (sectors - reservedSectors - numFATs*fatSz) / sectorsPerCluster
		if ceilDiv((clusters+2)*4, sectorSize) <= fatSz {
			break
		}
		fatSz++
	}
	if clusters < minClusters {
		return fmt.Errorf("fat32: %d sectors gives %d clusters; FAT32 needs at least %d (use >= %d sectors)", sectors, clusters, minClusters, MinSectors())
	}

	// Allocate clusters: directories breadth-first, then files.
	next := uint32(2)
	var order []*node
	queue := []*node{root}
	for len(queue) > 0 {
		d := queue[0]
		queue = queue[1:]
		order = append(order, d)
		entries := uint32(len(d.children))
		if d != root {
			entries += 2
		} else if opt.Label != "" {
			entries++
		}
		d.nclust = max(1, ceilDiv(entries*dirEntrySize, clusterSize))
		d.cluster = next
		next += d.nclust
		for _, c := range d.children {
			if c.isDir {
				queue = append(queue, c)
			}
		}
	}
	for _, d := range order {
		for _, c := range d.children {
			if !c.isDir && len(c.data) > 0 {
				c.nclust = ceilDiv(uint32(len(c.data)), clusterSize)
				c.cluster = next
				next += c.nclust
			}
		}
	}
	used := next - 2
	if used > clusters {
		return fmt.Errorf("fat32: contents need %d clusters, volume has %d", used, clusters)
	}

	at := func(sector uint32) int64 { return off + int64(sector)*sectorSize }
	dataStart := uint32(reservedSectors + numFATs*fatSz)
	clusterAt := func(c uint32) int64 { return at(dataStart + (c-2)*sectorsPerCluster) }

	// Zero the metadata region so stale bytes in a reused file cannot leak in.
	if _, err := w.WriteAt(make([]byte, int(dataStart)*sectorSize), off); err != nil {
		return err
	}

	label := opt.Label
	if label == "" {
		label = "NO NAME"
	}
	var labelField [11]byte
	copy(labelField[:], fmt.Sprintf("%-11s", strings.ToUpper(label)))

	bs := make([]byte, sectorSize)
	copy(bs[0:3], []byte{0xEB, 0x58, 0x90})
	copy(bs[3:11], "NBIMG   ")
	binary.LittleEndian.PutUint16(bs[11:], sectorSize)
	bs[13] = sectorsPerCluster
	binary.LittleEndian.PutUint16(bs[14:], reservedSectors)
	bs[16] = numFATs
	bs[21] = 0xF8
	binary.LittleEndian.PutUint16(bs[24:], 63)
	binary.LittleEndian.PutUint16(bs[26:], 255)
	binary.LittleEndian.PutUint32(bs[28:], opt.HiddenSectors)
	binary.LittleEndian.PutUint32(bs[32:], sectors)
	binary.LittleEndian.PutUint32(bs[36:], fatSz)
	binary.LittleEndian.PutUint32(bs[44:], root.cluster)
	binary.LittleEndian.PutUint16(bs[48:], 1) // FSInfo sector
	binary.LittleEndian.PutUint16(bs[50:], 6) // backup boot sector
	bs[64] = 0x80
	bs[66] = 0x29
	binary.LittleEndian.PutUint32(bs[67:], opt.VolumeID)
	copy(bs[71:82], labelField[:])
	copy(bs[82:90], "FAT32   ")
	bs[510], bs[511] = 0x55, 0xAA

	fsinfo := make([]byte, sectorSize)
	binary.LittleEndian.PutUint32(fsinfo[0:], fsInfoSig1)
	binary.LittleEndian.PutUint32(fsinfo[484:], fsInfoSig2)
	binary.LittleEndian.PutUint32(fsinfo[488:], clusters-used)
	binary.LittleEndian.PutUint32(fsinfo[492:], next)
	binary.LittleEndian.PutUint32(fsinfo[508:], fsInfoSig3)

	for _, s := range []struct {
		sector uint32
		data   []byte
	}{{0, bs}, {1, fsinfo}, {6, bs}, {7, fsinfo}} {
		if _, err := w.WriteAt(s.data, at(s.sector)); err != nil {
			return err
		}
	}

	fat := make([]byte, int(fatSz)*sectorSize)
	binary.LittleEndian.PutUint32(fat[0:], 0x0FFFFFF8)
	binary.LittleEndian.PutUint32(fat[4:], eoc)
	chain := func(n *node) {
		for i := uint32(0); i < n.nclust; i++ {
			v := uint32(eoc)
			if i+1 < n.nclust {
				v = n.cluster + i + 1
			}
			binary.LittleEndian.PutUint32(fat[(n.cluster+i)*4:], v)
		}
	}
	for _, d := range order {
		chain(d)
		for _, c := range d.children {
			if !c.isDir {
				chain(c)
			}
		}
	}
	for i := uint32(0); i < numFATs; i++ {
		if _, err := w.WriteAt(fat, at(reservedSectors+i*fatSz)); err != nil {
			return err
		}
	}

	entry := func(name [11]byte, attr byte, cluster, size uint32) []byte {
		e := make([]byte, dirEntrySize)
		copy(e[0:11], name[:])
		e[11] = attr
		binary.LittleEndian.PutUint16(e[16:], fatDate) // creation date
		binary.LittleEndian.PutUint16(e[18:], fatDate) // access date
		binary.LittleEndian.PutUint16(e[20:], uint16(cluster>>16))
		binary.LittleEndian.PutUint16(e[24:], fatDate) // write date
		binary.LittleEndian.PutUint16(e[26:], uint16(cluster))
		binary.LittleEndian.PutUint32(e[28:], size)
		return e
	}
	dot := [11]byte{'.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}
	dotdot := [11]byte{'.', '.', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' ', ' '}

	parents := map[*node]*node{}
	for _, d := range order {
		for _, c := range d.children {
			parents[c] = d
		}
	}
	for _, d := range order {
		buf := make([]byte, 0, int(d.nclust)*clusterSize)
		if d == root {
			if opt.Label != "" {
				buf = append(buf, entry(labelField, attrVolumeID, 0, 0)...)
			}
		} else {
			buf = append(buf, entry(dot, attrDirectory, d.cluster, 0)...)
			pc := uint32(0) // ".." of a top-level directory points at cluster 0
			if p := parents[d]; p != root {
				pc = p.cluster
			}
			buf = append(buf, entry(dotdot, attrDirectory, pc, 0)...)
		}
		for _, c := range d.children {
			if c.isDir {
				buf = append(buf, entry(c.name, attrDirectory, c.cluster, 0)...)
			} else {
				buf = append(buf, entry(c.name, attrArchive, c.cluster, uint32(len(c.data)))...)
			}
		}
		buf = buf[:cap(buf)]
		if _, err := w.WriteAt(buf, clusterAt(d.cluster)); err != nil {
			return err
		}
		for _, c := range d.children {
			if !c.isDir && c.nclust > 0 {
				data := make([]byte, int(c.nclust)*clusterSize)
				copy(data, c.data)
				if _, err := w.WriteAt(data, clusterAt(c.cluster)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}
