// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"
	"strings"

	"xenapi"
)

// VTPMOutcome says what reconciling a VM's vTPM actually did, so the caller can
// tell the user something true without reading the VM back.
type VTPMOutcome int

const (
	// VTPMUnchanged means the VM already had what the build asked for.
	VTPMUnchanged VTPMOutcome = iota
	// VTPMCreated means a vTPM was added, because the VM had none.
	VTPMCreated
	// VTPMDestroyed means an inherited vTPM was removed, with its secret.
	VTPMDestroyed
	// VTPMDeclarationCleared means the VM had no vTPM to remove, but carried
	// the platform:vtpm XAPI would have manufactured one from at start.
	VTPMDeclarationCleared
)

// vtpmAPI is the slice of XenAPI the vTPM handling drives.
//
// It is an interface only so the reconcile logic can be tested: the XenAPI SDK
// exposes package-level functions bound to a live session, and CI has no pool to
// call them against. Production code always uses xapiVTPM.
type vtpmAPI interface {
	// List reports the VM's vTPMs. XAPI permits at most one.
	List() ([]xenapi.VTPMRef, error)
	// Create attaches a new vTPM. XAPI only accepts it while the VM is halted.
	Create() (xenapi.VTPMRef, error)
	// Destroy removes a vTPM and permanently deletes its backing secret.
	Destroy(ref xenapi.VTPMRef) error
	// Declared reports platform:vtpm. It is the other half of the answer: a VM
	// with no vTPM object still boots with one when this key is true, because
	// XAPI manufactures a fresh vTPM from it at start.
	Declared() (bool, error)
	// ClearDeclaration removes platform:vtpm, which XAPI reads as false.
	ClearDeclaration() error
}

// xapiVTPM drives one VM's vTPMs over an established XenAPI session.
type xapiVTPM struct {
	session *xenapi.Session
	vm      xenapi.VMRef
}

func (v xapiVTPM) List() ([]xenapi.VTPMRef, error) {
	return xenapi.VM.GetVTPMs(v.session, v.vm)
}

// Create asks for a copyable vTPM (is_unique=false), which is what XAPI itself
// defaults to. A unique vTPM's contents are never copied, so a template built
// with one would hand every VM cloned from it an empty TPM.
func (v xapiVTPM) Create() (xenapi.VTPMRef, error) {
	return xenapi.VTPM.Create(v.session, v.vm, false)
}

func (v xapiVTPM) Destroy(ref xenapi.VTPMRef) error {
	return xenapi.VTPM.Destroy(v.session, ref)
}

func (v xapiVTPM) Declared() (bool, error) {
	platform, err := xenapi.VM.GetPlatform(v.session, v.vm)
	if err != nil {
		return false, err
	}
	return platformDeclaresVTPM(platform), nil
}

// platformDeclaresVTPM answers the key the way XAPI reads it. Vm_platform.is_true
// lowercases the value and takes "true" and "1"; the vtpm key's default is false
// (xapi_xenops.ml tpm_of_vm), so everything else, an absent key included, is false.
func platformDeclaresVTPM(platform map[string]string) bool {
	switch strings.ToLower(platform["vtpm"]) {
	case "true", "1":
		return true
	default:
		return false
	}
}

func (v xapiVTPM) ClearDeclaration() error {
	return xenapi.VM.RemoveFromPlatform(v.session, v.vm, "vtpm")
}

// ReconcileVTPM gives a freshly cloned VM the vTPM state the build asked for,
// and reports what it had to change. VM.clone copies every vTPM the source has
// and creates none when it has none (xapi_vm_clone.ml calls Xapi_vtpm.copy per
// device), so both directions have to be acted on after the clone.
//
// Call it while the VM is still halted. XAPI requires Halted to create a vTPM,
// and Halted or Suspended to destroy one.
func ReconcileVTPM(c *Connection, vm xenapi.VMRef, want bool) (VTPMOutcome, error) {
	return reconcileVTPM(xapiVTPM{session: c.GetSession(), vm: vm}, want)
}

// reconcileVTPM reads first and writes only when the VM does not already match.
//
// The read is not an optimisation. XAPI permits one vTPM per VM, so creating a
// second raises VTPM_MAX_AMOUNT_REACHED; and destroying one deletes its backing
// secret outright, with no way back. A VM that already matches must therefore
// not be touched at all.
func reconcileVTPM(api vtpmAPI, want bool) (VTPMOutcome, error) {
	current, err := api.List()
	if err != nil {
		return VTPMUnchanged, fmt.Errorf("could not read the VM's vtpm state: %w", err)
	}

	if want {
		// An existing vTPM object is attached whatever platform:vtpm says
		// (xapi_xenops.ml tpm_of_vm), so there is nothing else to write.
		if len(current) == 0 {
			if _, err := api.Create(); err != nil {
				return VTPMUnchanged, fmt.Errorf("could not create the vtpm the build asked for: %w", err)
			}
			return VTPMCreated, nil
		}
		return VTPMUnchanged, nil
	}

	// Destroying the object is only half of "no vTPM". platform:vtpm is copied
	// by VM.clone, and XAPI manufactures a fresh vTPM from it at the next
	// VM.start for a VM that has none -- so a destroy on its own leaves the
	// build reporting a removal the VM then undoes (CA-431619).
	declared, err := api.Declared()
	if err != nil {
		return VTPMUnchanged, fmt.Errorf("could not read the VM's platform:vtpm state: %w", err)
	}
	if len(current) == 0 && !declared {
		return VTPMUnchanged, nil
	}

	for _, ref := range current {
		if err := api.Destroy(ref); err != nil {
			return VTPMUnchanged, fmt.Errorf("could not destroy the vtpm inherited from the source: %w", err)
		}
	}

	outcome := VTPMDeclarationCleared
	if len(current) > 0 {
		outcome = VTPMDestroyed
	}
	if declared {
		if err := api.ClearDeclaration(); err != nil {
			if len(current) == 0 {
				// Nothing landed: there was no vTPM object to destroy either.
				outcome = VTPMUnchanged
			}
			return outcome, fmt.Errorf("could not clear the VM's platform:vtpm, from which XenServer would "+
				"create a fresh vtpm at start: %w", err)
		}
	}
	return outcome, nil
}
