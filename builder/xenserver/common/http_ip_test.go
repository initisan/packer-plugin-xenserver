// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// stubResolver records lookups instead of performing them, so the precedence rules can be
// asserted without touching the host's real networking.
func stubResolver(routed, named string, routedErr, namedErr error, calls *[]string) httpIPResolver {
	return httpIPResolver{
		routeTo: func(host string) (string, error) {
			*calls = append(*calls, "route:"+host)
			return routed, routedErr
		},
		addrOfInterface: func(name string) (string, error) {
			*calls = append(*calls, "iface:"+name)
			return named, namedErr
		},
	}
}

// Pinning http_bind_address overrides our guess on purpose, usually because the guest
// network is not the one facing the pool. Probing anyway would override them back.
func TestHTTPIPResolver_ExplicitBindAddressWinsWithoutProbing(t *testing.T) {
	var calls []string
	r := stubResolver("10.0.0.9", "10.0.1.9", nil, nil, &calls)

	ip, err := r.resolve("192.168.5.4", "eth0", "10.0.0.1")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "192.168.5.4" {
		t.Errorf("ip = %q, want the configured http_bind_address", ip)
	}
	if len(calls) != 0 {
		t.Errorf("an explicit address must short-circuit discovery, but it ran %v", calls)
	}
}

// "0.0.0.0" and "::" are legitimate *bind* addresses meaning "every interface", but they
// are useless as a callback address for the guest, so they must not be handed out as one.
func TestHTTPIPResolver_WildcardBindAddressFallsThroughToDiscovery(t *testing.T) {
	for _, bind := range []string{"", "0.0.0.0", "::"} {
		var calls []string
		r := stubResolver("10.0.0.9", "", nil, nil, &calls)

		ip, err := r.resolve(bind, "", "10.0.0.1")

		if err != nil {
			t.Errorf("bind %q: unexpected error: %v", bind, err)
			continue
		}
		if ip != "10.0.0.9" {
			t.Errorf("bind %q: ip = %q, want the discovered address", bind, ip)
		}
	}
}

func TestHTTPIPResolver_NamedInterfaceIsPreferredOverRouteLookup(t *testing.T) {
	var calls []string
	r := stubResolver("10.0.0.9", "10.0.1.9", nil, nil, &calls)

	ip, err := r.resolve("", "eth1", "10.0.0.1")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "10.0.1.9" {
		t.Errorf("ip = %q, want the address of the named interface", ip)
	}
	if len(calls) != 1 || calls[0] != "iface:eth1" {
		t.Errorf("calls = %v, want only the interface lookup", calls)
	}
}

// With no override, the resolver must ask which of our addresses faces the pool. The old
// code answered that with `echo $SSH_CLIENT` over an SSH login to dom0, which XS9 disables.
func TestHTTPIPResolver_DefaultsToTheAddressFacingThePool(t *testing.T) {
	var calls []string
	r := stubResolver("10.0.0.9", "", nil, nil, &calls)

	ip, err := r.resolve("", "", "xs-pool.example.com")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ip != "10.0.0.9" {
		t.Errorf("ip = %q, want the route-derived address", ip)
	}
	if len(calls) != 1 || calls[0] != "route:xs-pool.example.com" {
		t.Errorf("calls = %v, want only the route lookup against the pool address", calls)
	}
}

// A failure here halts the build, so the message has to name the knob that fixes it
// rather than just reporting the syscall error.
func TestHTTPIPResolver_RouteFailureSuggestsTheOverride(t *testing.T) {
	var calls []string
	r := stubResolver("", "", errors.New("network is unreachable"), nil, &calls)

	_, err := r.resolve("", "", "10.0.0.1")

	if err == nil {
		t.Fatal("expected an error when the route lookup fails")
	}
	if !strings.Contains(err.Error(), "http_bind_address") {
		t.Errorf("error %q should point at http_bind_address", err)
	}
	if !strings.Contains(err.Error(), "network is unreachable") {
		t.Errorf("error %q should keep the underlying cause", err)
	}
}

func TestHTTPIPResolver_InterfaceFailureNamesTheInterface(t *testing.T) {
	var calls []string
	cause := errors.New("no such device")
	r := stubResolver("", "", nil, cause, &calls)

	_, err := r.resolve("", "eth9", "10.0.0.1")

	if err == nil {
		t.Fatal("expected an error when the named interface cannot be used")
	}
	if !strings.Contains(err.Error(), "eth9") {
		t.Errorf("error %q should name the interface that failed", err)
	}
	if !errors.Is(err, cause) {
		t.Errorf("error %q should wrap the underlying cause", err)
	}
}

// xenserver_host is whatever the user typed -- scheme, trailing slash, explicit port. A
// route lookup needs the bare host, or a working pool address becomes unresolvable.
func TestBareHost(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"10.0.0.1", "10.0.0.1"},
		{"https://10.0.0.1", "10.0.0.1"},
		{"HTTPS://10.0.0.1", "10.0.0.1"},
		{"http://xs.example.com/", "xs.example.com"},
		{"https://10.0.0.1:443", "10.0.0.1"},
		{"10.0.0.1:443", "10.0.0.1"},
		{"[fd00::1]:443", "fd00::1"},
		{"fd00::1", "fd00::1"},
		{"", ""},
	} {
		if got := bareHost(tc.in); got != tc.want {
			t.Errorf("bareHost(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// URL schemes are case-insensitive, so CommonConfig.Prepare accepts "HTTPS://pool" and
// everything stripping the scheme back off must too. NewXenAPIClient is the caller that
// matters: it keeps the port, which is why it cannot just use bareHost.
func TestStripScheme(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"10.0.0.1", "10.0.0.1"},
		{"https://10.0.0.1", "10.0.0.1"},
		{"HTTPS://10.0.0.1", "10.0.0.1"},
		{"Https://xs.example.com:443", "xs.example.com:443"},
		{"10.0.0.1:443", "10.0.0.1:443"},
		{"", ""},
	} {
		if got := stripScheme(tc.in); got != tc.want {
			t.Errorf("stripScheme(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// A routing-table question, not a connection: nothing need listen at the far end.
func TestLocalAddrForHost_ResolvesWithoutAPeer(t *testing.T) {
	ip, err := localAddrForHost("127.0.0.1")

	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if parsed := net.ParseIP(ip); parsed == nil {
		t.Fatalf("localAddrForHost returned %q, which is not an IP", ip)
	}
	if ip != "127.0.0.1" {
		t.Errorf("ip = %q, want the loopback source address for a loopback destination", ip)
	}
}

func TestLocalAddrForHost_RejectsAnEmptyHost(t *testing.T) {
	if _, err := localAddrForHost(""); err == nil {
		t.Error("an empty pool address must be reported, not silently probed")
	}
}
