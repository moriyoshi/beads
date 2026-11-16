// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

package sntp

import (
	"encoding/binary"
	"net"
	"time"
)

// Serve answers SNTP requests on conn with the time from now, reporting
// itself as a stratum-1 server, until conn is closed. It exists for tests:
// vmrun runs it on the host so that a guest booted with a wrong clock has
// something to synchronize with.
func Serve(conn net.PacketConn, now func() time.Time) error {
	buf := make([]byte, 512)
	for {
		n, addr, err := conn.ReadFrom(buf)
		if err != nil {
			return err
		}
		recv := now()
		if n < packetLen || buf[0]&7 != 3 {
			continue
		}
		resp := make([]byte, packetLen)
		resp[0] = 0<<6 | 4<<3 | 4 // no leap warning, version 4, server mode
		resp[1] = 1               // stratum 1
		resp[2] = buf[2]          // poll
		resp[3] = 0xec            // precision ~2^-20 s
		copy(resp[12:16], "TEST")
		binary.BigEndian.PutUint64(resp[16:], toNTP(recv))                       // reference
		binary.BigEndian.PutUint64(resp[24:], binary.BigEndian.Uint64(buf[40:])) // originate
		binary.BigEndian.PutUint64(resp[32:], toNTP(recv))                       // receive
		binary.BigEndian.PutUint64(resp[40:], toNTP(now()))                      // transmit
		conn.WriteTo(resp, addr)
	}
}
