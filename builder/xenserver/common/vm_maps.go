// Copyright (c) 2026. Citrix Systems, Inc.

package common

import "xenapi"

// Every write to one of a VM's string maps is a remove-then-add: XAPI's add_to_ setters
// raise MAP_DUPLICATE_KEY on a key that is already there, and a clone has whatever its
// source had. remove_from_ is a no-op when the key is absent.

// setPlatformKey replaces one platform key, leaving the rest of the map alone.
func setPlatformKey(session *xenapi.Session, vm xenapi.VMRef, key, value string) error {
	if err := xenapi.VM.RemoveFromPlatform(session, vm, key); err != nil {
		return err
	}
	return xenapi.VM.AddToPlatform(session, vm, key, value)
}

// setHVMBootParam replaces one HVM_boot_params key, leaving the rest of the map alone.
func setHVMBootParam(session *xenapi.Session, vm xenapi.VMRef, key, value string) error {
	if err := xenapi.VM.RemoveFromHVMBootParams(session, vm, key); err != nil {
		return err
	}
	return xenapi.VM.AddToHVMBootParams(session, vm, key, value)
}
