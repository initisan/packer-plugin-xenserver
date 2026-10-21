// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"

	"xenapi"
)

// The two values written to platform:secureboot.
//
// "auto" rather than "true" is XAPI's own spelling for "on where the pool has
// certificates to enforce it with": VM.start resolves it against the active UEFI
// certificates and writes back true or false (xapi_vm.ml:283-292).
const (
	secureBootAuto = "auto"
	secureBootOff  = "false"
)

// SecureBootOutcome says what setting Secure Boot actually did, so the caller
// can tell the user something true without reading the VM back.
type SecureBootOutcome int

const (
	// SecureBootKept means the build said nothing about secure_boot, so the
	// value the clone inherited from its source stands.
	SecureBootKept SecureBootOutcome = iota
	// SecureBootDisabled means Secure Boot was written off.
	SecureBootDisabled
	// SecureBootAuto means Secure Boot was written on, subject to the pool
	// having UEFI certificates to enforce it with.
	SecureBootAuto
)

// Describe reports the outcome for the build log. source names the
// clone_template, which is where a kept setting came from.
func (o SecureBootOutcome) Describe(source string) string {
	switch o {
	case SecureBootDisabled:
		return "Set Secure boot to Disabled"
	case SecureBootAuto:
		return "Set Secure boot to Auto"
	}
	return fmt.Sprintf("Keeping the Secure Boot setting inherited from '%s'", source)
}

// secureBootAPI is the slice of XenAPI the Secure Boot handling drives.
//
// It is an interface only so the decision can be tested: the XenAPI SDK exposes
// package-level functions bound to a live session, and CI has no pool to call
// them against. Production code always uses xapiSecureBoot.
type secureBootAPI interface {
	// Set writes platform:secureboot, replacing any value already there.
	Set(value string) error
}

// xapiSecureBoot writes one VM's platform:secureboot over an established XenAPI
// session.
type xapiSecureBoot struct {
	session *xenapi.Session
	vm      xenapi.VMRef
}

func (s xapiSecureBoot) Set(value string) error {
	return setPlatformKey(s.session, s.vm, "secureboot", value)
}

// ReconcileSecureBoot gives a freshly cloned VM the Secure Boot state the build
// asked for, and reports what it had to change.
//
// secureBoot is nil when the build did not set secure_boot, which means "keep
// whatever VM.clone copied from the source" -- and keeping it means writing
// nothing at all. There is no value that means "unset": XAPI leaves a
// platform:secureboot it already has alone and treats an absent one as off
// (xapi_vm.ml:283-292, xapi_vm.ml:1714), so writing "false" to be safe is
// precisely what turns a Secure Boot template's Secure Boot off.
//
// Both builders clone, so both need this. Keeping it in one place is deliberate:
// the two had a copy each, only one of them learned to leave an inherited value
// alone, and a build off a Secure Boot template quietly lost it on the other.
//
// Call it while the VM is still halted, before VM.start resolves "auto".
func ReconcileSecureBoot(c *Connection, vm xenapi.VMRef, secureBoot *bool) (SecureBootOutcome, error) {
	return reconcileSecureBoot(xapiSecureBoot{session: c.GetSession(), vm: vm}, secureBoot)
}

func reconcileSecureBoot(api secureBootAPI, secureBoot *bool) (SecureBootOutcome, error) {
	if secureBoot == nil {
		return SecureBootKept, nil
	}

	// Prepare rejects secure_boot = true unless firmware is uefi on both
	// builders, so the firmware is not re-tested here.
	value, outcome := secureBootOff, SecureBootDisabled
	if *secureBoot {
		value, outcome = secureBootAuto, SecureBootAuto
	}

	if err := api.Set(value); err != nil {
		return SecureBootKept, fmt.Errorf("could not set the VM's secure boot state to %q: %w", value, err)
	}
	return outcome, nil
}
