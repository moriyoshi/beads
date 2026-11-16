// Copyright 2026 Moriyoshi Koizumi
// SPDX-License-Identifier: Apache-2.0

// Command app is the consumer workspace's appliance program.
package main

import (
	"flag"
	"fmt"
	"log"
	"net/http"
)

func main() {
	listen := flag.String("listen", ":8080", "address to serve on")
	flag.Parse()
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintln(w, "consumer ok")
	})
	log.Fatal(http.ListenAndServe(*listen, nil))
}
