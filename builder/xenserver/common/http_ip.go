// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"errors"
	"fmt"
	"net"
	"strings"
)

// isWildcardBindAddress reports whether an http_bind_address means "listen on every
// interface". Fine to bind to, but it names no address the guest can call back on.
// Matched as an address rather than a string, so "::0" and "::ffff:0.0.0.0" count too.
func isWildcardBindAddress(bindAddress string) bool {
	ip := net.ParseIP(bindAddress)
	return ip == nil || ip.IsUnspecified()
}

// httpIPResolver works out which of this machine's addresses a guest should use to reach
// Packer's HTTP server: the value behind `{{ .HTTPIP }}`. The two lookups are fields
// rather than direct calls so tests need no particular host network configuration.
type httpIPResolver struct {
	// routeTo reports the local address the kernel would send from when talking to host.
	routeTo func(host string) (string, error)
	// addrOfInterface reports the first usable IPv4 address assigned to a named interface.
	addrOfInterface func(name string) (string, error)
}

func newHTTPIPResolver() httpIPResolver {
	return httpIPResolver{
		routeTo:         localAddrForHost,
		addrOfInterface: addrOfInterface,
	}
}

// resolve applies the precedence an operator would expect: an explicit bind address wins,
// then a named interface, otherwise ask the routing table which of our addresses faces
// the pool. That last case replaces the plugin's original probe, which SSHed into dom0
// for `echo $SSH_CLIENT` -- and XenServer 9 ships with dom0 SSH disabled.
func (r httpIPResolver) resolve(bindAddress, interfaceName, poolAddress string) (string, error) {
	if !isWildcardBindAddress(bindAddress) {
		return bindAddress, nil
	}

	if interfaceName != "" {
		ip, err := r.addrOfInterface(interfaceName)
		if err != nil {
			return "", fmt.Errorf("cannot use http_interface %q: %w", interfaceName, err)
		}
		return ip, nil
	}

	ip, err := r.routeTo(poolAddress)
	if err != nil {
		return "", fmt.Errorf("could not work out which local address reaches the XenServer pool at %q: %w; "+
			"set http_bind_address to the address the guest should fetch from", poolAddress, err)
	}
	return ip, nil
}

// localAddrForHost asks the kernel which source address it would use to reach host.
// Dialling UDP does the routing-table lookup and binds a local address but sends nothing,
// so it answers whether or not anything is listening. The port is arbitrary for the same
// reason.
func localAddrForHost(host string) (string, error) {
	host = bareHost(host)
	if host == "" {
		return "", errors.New("no XenServer pool address to route towards")
	}

	conn, err := net.Dial("udp", net.JoinHostPort(host, "80"))
	if err != nil {
		return "", err
	}
	defer conn.Close()

	addr, ok := conn.LocalAddr().(*net.UDPAddr)
	if !ok || addr.IP == nil || addr.IP.IsUnspecified() {
		return "", fmt.Errorf("the route to %q has no local source address", host)
	}
	return addr.IP.String(), nil
}

// addrOfInterface returns the first non-loopback IPv4 address assigned to name.
func addrOfInterface(name string) (string, error) {
	iface, err := net.InterfaceByName(name)
	if err != nil {
		return "", err
	}

	addrs, err := iface.Addrs()
	if err != nil {
		return "", err
	}

	for _, addr := range addrs {
		ipNet, ok := addr.(*net.IPNet)
		if !ok || ipNet.IP.IsLoopback() {
			continue
		}
		if ipv4 := ipNet.IP.To4(); ipv4 != nil {
			return ipv4.String(), nil
		}
	}

	return "", fmt.Errorf("interface %q has no non-loopback IPv4 address", name)
}

// stripScheme removes a leading URL scheme however it is spelled. Schemes are
// case-insensitive and xenserver_host is free text, so Prepare accepts "HTTPS://pool" and
// a case-sensitive TrimPrefix would leave the scheme in the host downstream.
func stripScheme(host string) string {
	if i := strings.Index(host, "://"); i >= 0 {
		return host[i+len("://"):]
	}
	return host
}

// bareHost strips the scheme, trailing slash and any explicit port from a configured pool
// address, leaving the host a route lookup can use. All three forms turn up in real
// templates.
func bareHost(host string) string {
	host = strings.TrimSuffix(stripScheme(host), "/")

	// SplitHostPort only succeeds when a port really is present, so a bare IPv6
	// literal fails it and is returned untouched.
	if stripped, _, err := net.SplitHostPort(host); err == nil {
		return stripped
	}

	// A bracketed IPv6 literal with no port fails that check, and JoinHostPort
	// would bracket it a second time.
	if len(host) > 1 && host[0] == '[' && host[len(host)-1] == ']' {
		return host[1 : len(host)-1]
	}

	return host
}
