// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"
	"log"

	"xenapi"
)

// The two values XAPI reads from HVM_boot_params["firmware"], and it matches them
// exactly: an absent key is BIOS, and no other spelling is UEFI. The checks in
// this package have to draw the line in the same place XAPI does.
const (
	firmwareBIOS = "bios"
	firmwareUEFI = "uefi"
)

// bootCheckAPI is the read-only slice of XenAPI the pre-start check reads. It is
// separate from vtpmAPI because this runs in the step that starts the VM, where
// nothing may be changed, and an interface only so CI can test without a pool.
type bootCheckAPI interface {
	// Firmware reports the firmware the VM is currently set to boot with. XAPI
	// keeps it in HVM_boot_params and treats an absent key as BIOS.
	Firmware() (string, error)
	// VTPMCount reports how many vTPMs the VM has. XAPI permits at most one.
	VTPMCount() (int, error)
}

// xapiBootCheck reads one VM over an established XenAPI session.
type xapiBootCheck struct {
	session *xenapi.Session
	vm      xenapi.VMRef
}

func (b xapiBootCheck) Firmware() (string, error) {
	params, err := xenapi.VM.GetHVMBootParams(b.session, b.vm)
	if err != nil {
		return "", err
	}
	return params["firmware"], nil
}

func (b xapiBootCheck) VTPMCount() (int, error) {
	vtpms, err := xenapi.VM.GetVTPMs(b.session, b.vm)
	if err != nil {
		return 0, err
	}
	return len(vtpms), nil
}

// EnsureVMCanStart reports the firmware a VM is about to boot with, and refuses
// a combination XenServer will not start.
//
// firmware is the build's own value and is empty when the build did not set one,
// which means "keep whatever VM.clone copied from the source" -- so the VM
// itself is then the only place the answer can come from. The returned string is
// the firmware that will actually be used, for the caller to report; it is empty
// when the VM could not be read.
//
// The one refusal so far: XenServer implements the vTPM for UEFI guests only,
// and VM.start on a BIOS VM that has one fails with NOT_IMPLEMENTED [Booting
// BIOS VM with VTPMs attached] -- which names neither the option to change nor
// where the vTPM came from. VM.clone copies the source's vTPM, so a build that
// mentions neither vtpm nor firmware can still arrive here with one.
//
// CloneSourceSupportsFirmware catches the same combination before the build
// starts, by reading the source instead of the clone. This stays as the backstop:
// it reads the clone itself, which is the only way to catch a vTPM the earlier
// check could not see because the source carries the vTPM object without
// platform:vtpm.
//
// Call it while the VM is still halted, before VM.start.
func EnsureVMCanStart(c *Connection, vm xenapi.VMRef, firmware string) (string, error) {
	return ensureVMCanStart(xapiBootCheck{session: c.GetSession(), vm: vm}, firmware)
}

func ensureVMCanStart(api bootCheckAPI, firmware string) (string, error) {
	configured := firmware != ""

	if !configured {
		inherited, err := api.Firmware()
		if err != nil {
			// The check below only replaces an obscure XAPI failure with a
			// readable one, so being unable to run it must not fail a build that
			// would otherwise have worked. VM.start still reports the real
			// problem.
			log.Printf("Could not read the VM's firmware, skipping the BIOS/vTPM check: %s\n", err)
			return "", nil
		}
		// An absent key is how XAPI spells BIOS.
		if firmware = inherited; firmware == "" {
			firmware = firmwareBIOS
		}
	}

	if firmware == firmwareUEFI {
		return firmware, nil
	}

	// XAPI knows the two spellings above and nothing else: any other value fails
	// VM.start with INVALID_VALUE [HVM-boot-params['firmware']] (xapi_xenops.ml:255-266,
	// reached from xapi_vm.ml:303) before a vTPM is looked at, and that error already
	// names the value at fault. Reporting such a build as a BIOS VM would send the
	// user after the wrong thing -- most of all for "UEFI", which was meant as the
	// opposite of what it would be blamed for. Prepare settles the build's own
	// firmware (CA-430878), so only an inherited one still reaches here.
	if firmware != firmwareBIOS {
		log.Printf("VM.start will reject firmware %q outright, skipping the BIOS/vTPM check\n", firmware)
		return firmware, nil
	}

	count, err := api.VTPMCount()
	if err != nil {
		log.Printf("Could not read the VM's vtpm state, skipping the BIOS/vTPM check: %s\n", err)
		return firmware, nil
	}
	if count == 0 {
		return firmware, nil
	}

	origin := "Both were inherited from the source the VM was cloned from; the build set neither."
	remedy := fmt.Sprintf("Set firmware = %q, or use a source without a vTPM.", firmwareUEFI)
	if configured {
		origin = fmt.Sprintf("The build set firmware = %q; the vTPM was inherited from the source the VM "+
			"was cloned from.", firmware)
		remedy = fmt.Sprintf("Remove firmware to keep the source's own, set firmware = %q, or use a source "+
			"without a vTPM.", firmwareUEFI)
	}

	// vtpm = false is named as a remedy for the clone builder only. There it
	// removes the inherited vTPM and the platform:vtpm XenServer would make a
	// fresh one from (CA-431619); on the iso builder it does nothing at all,
	// because StepAttachvTPM only skips.
	return firmware, fmt.Errorf("this VM has a vTPM and will boot with firmware %q, which XenServer refuses "+
		"to start: NOT_IMPLEMENTED [Booting BIOS VM with VTPMs attached]. %s %s On the clone builder, vtpm = "+
		"false removes the inherited vTPM instead, which resolves this without changing the firmware.",
		firmware, origin, remedy)
}
