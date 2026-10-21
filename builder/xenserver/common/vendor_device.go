// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"

	"xenapi"
)

// vendorDeviceAPI is the slice of XenAPI the vendor-device handling drives.
//
// An interface only so the read-before-write logic can be tested; production code always
// uses xapiVendorDevice.
type vendorDeviceAPI interface {
	// Get reports VM.has_vendor_device.
	Get() (bool, error)
	// Set writes VM.has_vendor_device. XAPI accepts it only while the VM is halted.
	Set(value bool) error
}

// xapiVendorDevice drives one VM's vendor-device flag over an established session.
type xapiVendorDevice struct {
	session *xenapi.Session
	vm      xenapi.VMRef
}

func (v xapiVendorDevice) Get() (bool, error) {
	return xenapi.VM.GetHasVendorDevice(v.session, v.vm)
}

func (v xapiVendorDevice) Set(value bool) error {
	return xenapi.VM.SetHasVendorDevice(v.session, v.vm, value)
}

// ApplyVendorDevice gives a freshly cloned VM the vendor-device setting the build asked
// for, and reports whether it had to change anything. Call it while the VM is halted:
// XAPI rejects the write on a running VM.
func ApplyVendorDevice(c *Connection, vm xenapi.VMRef, want bool) (changed bool, err error) {
	return applyVendorDevice(xapiVendorDevice{session: c.GetSession(), vm: vm}, want)
}

// applyVendorDevice writes the flag only when it differs from what the VM already has.
//
// The read is not an optimisation: XenServer's API documentation warns that changing
// has_vendor_device on a VM with Windows already installed is likely to crash it on the
// next start, so an already-correct source must be left alone.
func applyVendorDevice(api vendorDeviceAPI, want bool) (changed bool, err error) {
	current, err := api.Get()
	if err != nil {
		return false, fmt.Errorf("could not read the VM's has_vendor_device: %w", err)
	}

	if current == want {
		return false, nil
	}

	if err := api.Set(want); err != nil {
		return false, fmt.Errorf("could not set has_vendor_device=%t on the VM: %w", want, err)
	}

	return true, nil
}
