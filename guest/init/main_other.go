// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

//go:build !netbsd

package main

import (
	"fmt"
	"os"
)

func main() {
	fmt.Fprintln(os.Stderr, "init: this program is PID 1 of a NetBSD appliance image and only runs on NetBSD")
	os.Exit(1)
}
