// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"
	"log"
	"net"
)

// HTTPPortMin/HTTPPortMax bound the local port the `http_directory` server listens on.
// Once user-facing options (http_port_min/http_port_max); now fixed, so the guest has to
// be able to reach this whole range on the machine running Packer.
const (
	HTTPPortMin uint = 8000
	HTTPPortMax uint = 9000
)

// FindPort listens on a free port in [portMin, portMax] on address, which may be empty
// for every interface. Returns the listener and port, or nil, 0 on failure.
func FindPort(address string, portMin uint, portMax uint) (net.Listener, uint) {
	log.Printf("Looking for an available port between %d and %d on '%s'", portMin, portMax, address)

	for port := portMin; port <= portMax; port++ {
		log.Printf("Trying port: %d", port)
		l, err := net.Listen("tcp", net.JoinHostPort(address, fmt.Sprint(port)))
		if err == nil {
			return l, port
		} else {
			log.Printf("Port %d unavailable: %s", port, err.Error())
		}
	}

	return nil, 0
}
