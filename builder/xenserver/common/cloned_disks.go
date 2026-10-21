// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"log"

	"xenapi"
)

// ClonedDisk is one disk a VM.clone duplicated, with the VBD holding it so a teardown can
// unplug before destroying.
type ClonedDisk struct {
	VBD xenapi.VBDRef
	VDI xenapi.VDIRef
}

// ClonedDisks lists the disks VM.clone duplicates for a VM, which is what a teardown has
// to destroy alongside it. Same rule xapi applies: an empty VBD has no VDI, and a CD keeps
// the original ISO rather than copying it (xapi_vm_clone.ml safe_clone_disks). Everything
// else is duplicated, floppies included, which is why GetDisks -- disks only -- is too
// narrow for a teardown.
//
// A VBD whose record cannot be read is skipped rather than failing the sweep: the callers
// are tearing down, and the other disks still have to go.
func ClonedDisks(c *Connection, vm xenapi.VMRef) ([]ClonedDisk, error) {
	vbds, err := xenapi.VM.GetVBDs(c.session, vm)
	if err != nil {
		return nil, err
	}

	disks := make([]ClonedDisk, 0, len(vbds))
	for _, vbd := range vbds {
		rec, err := xenapi.VBD.GetRecord(c.session, vbd)
		if err != nil {
			log.Printf("Unable to get VBD record: %s", err.Error())
			continue
		}
		if rec.Empty || rec.Type == xenapi.VbdTypeCD {
			continue
		}
		disks = append(disks, ClonedDisk{VBD: vbd, VDI: rec.VDI})
	}

	return disks, nil
}
