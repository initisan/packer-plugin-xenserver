// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"testing"

	"github.com/hashicorp/packer-plugin-sdk/communicator"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
)

func stateWithIP(ip string) multistep.StateBag {
	state := new(multistep.BasicStateBag)
	state.Put("instance_ssh_address", ip)
	return state
}

// CA-431607. Both hosts used to be accepted by packer validate and then dropped:
// the builders handed StepConnect the discovered IP unconditionally, and the SDK
// writes the callback's answer back over the configured value.
func TestCommHost_PrefersTheConfiguredHost(t *testing.T) {
	cases := map[string]communicator.Config{
		"winrm_host": {Type: "winrm", WinRM: communicator.WinRM{WinRMHost: "10.0.0.9"}},
		"ssh_host":   {Type: "ssh", SSH: communicator.SSH{SSHHost: "10.0.0.9"}},
	}

	for name, comm := range cases {
		t.Run(name, func(t *testing.T) {
			host, err := CommHost(&comm)(stateWithIP("192.168.1.5"))
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if host != "10.0.0.9" {
				t.Errorf("host = %q, want the configured %s", host, name)
			}
		})
	}
}

// Unset is the usual case: the guest's address is not known until it reports one.
func TestCommHost_FallsBackToTheDiscoveredIP(t *testing.T) {
	cases := map[string]communicator.Config{
		"ssh":   {Type: "ssh"},
		"winrm": {Type: "winrm"},
		// A host set for the communicator that is not in use is not the address
		// to dial -- Config.Host() reads only the one matching Type.
		"winrm with an ssh_host": {Type: "winrm", SSH: communicator.SSH{SSHHost: "10.0.0.9"}},
	}

	for name, comm := range cases {
		t.Run(name, func(t *testing.T) {
			host, err := CommHost(&comm)(stateWithIP("192.168.1.5"))
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if host != "192.168.1.5" {
				t.Errorf("host = %q, want the discovered IP", host)
			}
		})
	}
}
