// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"errors"
	"fmt"
	"strconv"
	"strings"

	"xenapi"
)

// defaultNetworkLabelPrefix is how XenCenter names a pool-wide network: "Network 3" for
// the network on eth3. XAPI stores a longer label, so this form gets a second lookup
// before it counts as missing.
const defaultNetworkLabelPrefix = "Network "

// namedNetworkAPI is the slice of XenAPI the named-network attach drives. It is an
// interface only so the attach can be tested -- the SDK's functions are bound to a live
// session and CI has no pool. Production code always uses xapiNamedNetwork.
type namedNetworkAPI interface {
	// FindByNameLabel returns every network carrying the given name-label.
	FindByNameLabel(label string) ([]xenapi.NetworkRef, error)
	// AttachVIF joins the VM to the network with a VIF at the given device index.
	AttachVIF(network xenapi.NetworkRef, device string) error
}

// xapiNamedNetwork drives one VM's named-network attach over an established session.
type xapiNamedNetwork struct {
	c  *Connection
	vm xenapi.VMRef
}

func (n xapiNamedNetwork) FindByNameLabel(label string) ([]xenapi.NetworkRef, error) {
	return xenapi.Network.GetByNameLabel(n.c.GetSession(), label)
}

func (n xapiNamedNetwork) AttachVIF(network xenapi.NetworkRef, device string) error {
	_, err := ConnectNetwork(n.c, network, n.vm, device)
	return err
}

// AttachNamedNetworks joins the VM to each network in names, one VIF per entry, in device
// order. say receives the progress messages; pass the build's ui.Say.
//
// Every failure is fatal: both builders used to report a failed VIF and carry on, handing
// back an artifact missing a NIC network_names asked for (CA-430088).
func AttachNamedNetworks(c *Connection, vm xenapi.VMRef, names []string, say func(string)) error {
	return attachNamedNetworks(xapiNamedNetwork{c: c, vm: vm}, names, say)
}

func attachNamedNetworks(api namedNetworkAPI, names []string, say func(string)) error {
	for i, label := range names {
		say(fmt.Sprintf("Finding network '%s'", label))

		networks, err := api.FindByNameLabel(label)
		if err != nil {
			return fmt.Errorf("could not look up the network name-label %q: %w", label, err)
		}

		if len(networks) == 0 && strings.HasPrefix(label, defaultNetworkLabelPrefix) {
			builtIn := poolWideNetworkLabel(label)
			say(fmt.Sprintf("No network found with name-label '%s'. This might be a default built-in network '%s'", label, builtIn))

			networks, err = api.FindByNameLabel(builtIn)
			if err != nil {
				return fmt.Errorf("could not look up the default network name-label %q: %w", builtIn, err)
			}
		}

		switch {
		case len(networks) == 0:
			return fmt.Errorf("could not find a network with the name-label %q", label)
		case len(networks) > 1:
			return fmt.Errorf("found %d networks with the name-label %q; the name must be unique", len(networks), label)
		}

		if err := api.AttachVIF(networks[0], strconv.Itoa(i)); err != nil {
			return fmt.Errorf("could not attach the VM to network %q on VIF device %d: %w", label, i, err)
		}
	}

	return nil
}

func poolWideNetworkLabel(label string) string {
	return "Pool-wide network associated with eth" + strings.TrimPrefix(label, defaultNetworkLabelPrefix)
}

// vmVIFAPI is the slice of XenAPI the VIF reconciliation drives. An interface for the
// same reason as namedNetworkAPI above.
type vmVIFAPI interface {
	// ExistingVIFs returns every VIF currently attached to the VM.
	ExistingVIFs() ([]xenapi.VIFRef, error)
	// DestroyVIF removes one VIF from the VM.
	DestroyVIF(vif xenapi.VIFRef) error
}

// xapiVMVIFs drives one VM's VIF reconciliation over an established XenAPI session.
type xapiVMVIFs struct {
	c  *Connection
	vm xenapi.VMRef
}

func (v xapiVMVIFs) ExistingVIFs() ([]xenapi.VIFRef, error) {
	return xenapi.VM.GetVIFs(v.c.GetSession(), v.vm)
}

func (v xapiVMVIFs) DestroyVIF(vif xenapi.VIFRef) error {
	return xenapi.VIF.Destroy(v.c.GetSession(), vif)
}

// CountVIFs reports how many VIFs the VM currently has. VM.clone copies the source
// template's VIFs, so this is how both builders tell "the template gave us NICs" from
// "this VM has none yet".
func CountVIFs(c *Connection, vm xenapi.VMRef) (int, error) {
	return countVIFs(xapiVMVIFs{c: c, vm: vm})
}

func countVIFs(api vmVIFAPI) (int, error) {
	vifs, err := api.ExistingVIFs()
	if err != nil {
		return 0, fmt.Errorf("could not list the VM's VIFs: %w", err)
	}

	return len(vifs), nil
}

// DetachAllVIFs destroys every VIF the VM currently has. say receives the progress
// messages; pass the build's ui.Say.
//
// Both builders need this before they attach network_names: the VM starts with the
// template's VIFs, so device 0 is taken and VIF.Create for the first requested network
// fails with DEVICE_ALREADY_EXISTS (CA-430322, CA-431992).
func DetachAllVIFs(c *Connection, vm xenapi.VMRef, say func(string)) error {
	return detachAllVIFs(xapiVMVIFs{c: c, vm: vm}, say)
}

func detachAllVIFs(api vmVIFAPI, say func(string)) error {
	vifs, err := api.ExistingVIFs()
	if err != nil {
		return fmt.Errorf("could not list the VM's VIFs: %w", err)
	}

	if len(vifs) == 0 {
		return nil
	}

	say(fmt.Sprintf("Removing %d VIF(s) inherited from the source before attaching the requested networks", len(vifs)))

	for _, vif := range vifs {
		if err := api.DestroyVIF(vif); err != nil {
			return fmt.Errorf("could not destroy the inherited VIF %q: %w", vif, err)
		}
	}

	return nil
}

// AttachManagementNetwork gives the VM a single VIF on device 0, on the network carrying
// the pool's management interface. Both builders fall back to it when no network_names
// are given and the template left the VM no VIF, so the guest can still reach the build's
// HTTP server.
//
// The loop deliberately does not stop at the first management PIF: every host in a pool
// has one, and taking the last preserves the behaviour both builders shipped with.
func AttachManagementNetwork(c *Connection, vm xenapi.VMRef, say func(string)) error {
	pifs, err := xenapi.PIF.GetAll(c.GetSession())
	if err != nil {
		return fmt.Errorf("could not list the pool's PIFs: %w", err)
	}

	var network xenapi.NetworkRef
	for _, pif := range pifs {
		rec, err := xenapi.PIF.GetRecord(c.GetSession(), pif)
		if err != nil {
			return fmt.Errorf("could not read the record of PIF %q: %w", pif, err)
		}
		if rec.Management {
			network = rec.Network
		}
	}

	if network == "" {
		return errors.New("could not find the management network to attach the VM to")
	}

	say("Creating a VIF on the pool's management network")

	if _, err := ConnectNetwork(c, network, vm, "0"); err != nil {
		return fmt.Errorf("could not attach the VM to the management network: %w", err)
	}

	return nil
}
