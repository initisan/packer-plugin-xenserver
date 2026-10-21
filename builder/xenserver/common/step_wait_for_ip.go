// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/communicator"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"

	"xenapi"
)

type StepWaitForIP struct {
	VmCleanup
	Chan    <-chan string
	Timeout time.Duration
}

func (self *StepWaitForIP) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packer.Ui)
	c := state.Get("client").(*Connection)

	ui.Say("Step: Wait for VM's IP to become known to us.")

	uuid := state.Get("instance_uuid").(string)
	instance, err := xenapi.VM.GetByUUID(c.session, uuid)
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to get VM from UUID '%s': %s", uuid, err.Error()))
		return multistep.ActionHalt
	}

	var ip string
	err = InterruptibleWait{
		Timeout:           self.Timeout,
		PredicateInterval: 5 * time.Second,
		// Both sources are always consulted: this was `ip_getter`, whose default
		// ("auto") already tried both. Snooping first, as it needs no PV tools.
		Predicate: func() (result bool, err error) {

			// Snoop IP from HTTP fetch
			select {
			case ip = <-self.Chan:
				ui.Message(fmt.Sprintf("Got IP '%s' from HTTP request", ip))
				return true, nil
			default:
			}

			// Look for PV IP
			m, err := xenapi.VM.GetGuestMetrics(c.session, instance)
			if err != nil {
				return false, err
			}
			if m != "" && m != nullRef {
				metrics, err := xenapi.VMGuestMetrics.GetRecord(c.session, m)
				if err != nil {
					return false, err
				}
				networks := metrics.Networks
				var ok bool
				if ip, ok = networks["0/ip"]; ok {
					if ip != "" {
						if strings.HasPrefix(ip, "169.254.") {
							ui.Message(fmt.Sprintf("Got link-local IP '%s' from XenServer tools, which is not usable for SSH. Waiting for a valid IP address", ip))
						} else {
							ui.Message(fmt.Sprintf("Got IP '%s' from XenServer tools", ip))
							return true, nil
						}
					}
				}
			}

			return false, nil
		},
	}.Wait(state)
	// Only one of the three ways the wait can end is worth the troubleshooting advice:
	// neither a cancelled build nor a failed XAPI call failed to find an address.
	switch err.(type) {
	case nil:
	case InterruptedError:
		ui.Say("Interrupted while waiting for the VM's IP address")
		return multistep.ActionHalt
	case TimeoutError:
		ui.Error(fmt.Sprintf("No IP address for the VM within %s (install_timeout)", self.Timeout))
		ui.Error("An address reaches the build from one of two places. The guest tools report it " +
			"through XAPI guest metrics, which needs the tools installed in the guest — leave " +
			"has_vendor_device on so Windows can fetch them. Otherwise the guest fetches a file " +
			"from Packer's HTTP server, which only runs when http_directory is set, and must be " +
			"reachable from the guest. A build that does neither should set communicator = \"none\", " +
			"which waits for the guest to power itself off instead.")
		return multistep.ActionHalt
	default:
		ui.Error(fmt.Sprintf("Could not read the VM's IP address: %s", err.Error()))
		return multistep.ActionHalt
	}

	ui.Say(fmt.Sprintf("Got IP address '%s'", ip))
	state.Put("instance_ssh_address", ip)

	return multistep.ActionContinue
}

func InstanceSSHIP(state multistep.StateBag) (string, error) {
	ip := state.Get("instance_ssh_address").(string)
	return ip, nil
}

// CommHost gives StepConnect the address to dial: the one the build named with
// ssh_host or winrm_host, or the IP discovered from the guest.
//
// The SDK assigns whatever this callback returns back over the configured host
// on every attempt of the connect loop (step_connect_ssh.go:147,
// step_connect_winrm.go:116), so a host the builder does not hand back here is
// accepted at validate time and then never used (CA-431607).
func CommHost(comm *communicator.Config) func(multistep.StateBag) (string, error) {
	return func(state multistep.StateBag) (string, error) {
		if host := comm.Host(); host != "" {
			return host, nil
		}
		return InstanceSSHIP(state)
	}
}

// ConnectStep builds the communicator step both builders register.
//
// No SSHPort callback, for the reason CommHost exists: the SDK writes that answer back
// over ssh_port too, and only calls it when it is set. Unset, the port is the one the
// build asked for, or the SDK's own default of 22.
func ConnectStep(ssh *SSHConfig) *communicator.StepConnect {
	return &communicator.StepConnect{
		Config:    &ssh.Comm,
		Host:      CommHost(&ssh.Comm),
		SSHConfig: ssh.Comm.SSHConfigFunc(),
	}
}
