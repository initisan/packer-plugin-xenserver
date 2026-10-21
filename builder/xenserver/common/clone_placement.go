// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"

	"xenapi"
)

// diskSRs is the slice of XenAPI the placement decision reads.
//
// It is an interface only so the decision can be tested: the XenAPI SDK exposes
// package-level functions bound to a live session, and CI has no pool to call
// them against. Production code always uses xapiDiskSRs.
type diskSRs interface {
	// SRs reports the SR each of the VM's disks is on, one entry per disk.
	SRs() ([]xenapi.SRRef, error)
}

// xapiDiskSRs reads one VM's disks over an established XenAPI session.
type xapiDiskSRs struct {
	c  *Connection
	vm xenapi.VMRef
}

func (d xapiDiskSRs) SRs() ([]xenapi.SRRef, error) {
	vbds, err := xenapi.VM.GetVBDs(d.c.session, d.vm)
	if err != nil {
		return nil, err
	}

	srs := make([]xenapi.SRRef, 0, len(vbds))
	for _, vbd := range vbds {
		rec, err := xenapi.VBD.GetRecord(d.c.session, vbd)
		if err != nil {
			return nil, err
		}
		// ClonedDisks' rule, read strictly: placement decides clone versus copy, so
		// a VBD that cannot be read is an error here rather than a disk to skip.
		if rec.Empty || rec.Type == xenapi.VbdTypeCD {
			continue
		}
		sr, err := xenapi.VDI.GetSR(d.c.session, rec.VDI)
		if err != nil {
			return nil, err
		}
		srs = append(srs, sr)
	}
	return srs, nil
}

// TemplateNeedsCopy reports whether putting the build's disks on target means
// copying the source's, rather than cloning them where they already are.
//
// VM.clone takes no SR: it clones each disk onto the SR that disk is already on,
// copy-on-write and near-instant. Only VM.copy takes a target, and it is a full
// copy of every disk -- xapi runs the same code for both and branches per disk
// on VDI.copy versus VDI.clone (xapi_vm_clone.ml clone_single_vdi). That cost
// buys nothing when the source is already on target, so the caller is told to
// clone in that case (CA-430509).
func TemplateNeedsCopy(c *Connection, template xenapi.VMRef, target xenapi.SRRef) (bool, error) {
	return templateNeedsCopy(xapiDiskSRs{c: c, vm: template}, target)
}

func templateNeedsCopy(api diskSRs, target xenapi.SRRef) (bool, error) {
	srs, err := api.SRs()
	if err != nil {
		return false, fmt.Errorf("could not read which SRs the source's disks are on: %w", err)
	}

	for _, sr := range srs {
		if sr != target {
			return true, nil
		}
	}

	// Every disk is already on target, or there are no disks to place. Neither
	// is worth a copy.
	return false, nil
}
