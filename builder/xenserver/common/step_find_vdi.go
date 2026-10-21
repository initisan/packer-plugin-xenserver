// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"context"
	"fmt"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"

	"xenapi"
)

type StepFindVdi struct {
	VdiName    string
	VdiUuidKey string
	// SRName restricts the lookup to that SR's VDIs. Empty means the whole pool, which
	// is what the deprecated guest_tools_iso needs: its ISO is not on iso_sr.
	SRName string
}

func (self *StepFindVdi) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	ui := state.Get("ui").(packer.Ui)
	c := state.Get("client").(*Connection)

	// Both callers wire an optional config value, so an unset name means there is no VDI to look up.
	if self.VdiName == "" {
		return multistep.ActionContinue
	}

	// "not found" means something different when the search was confined to one SR.
	scope := "in the pool"
	if self.SRName != "" {
		scope = fmt.Sprintf("in SR '%s'", self.SRName)
	}

	vdis, err := xenapi.VDI.GetByNameLabel(c.session, self.VdiName)
	if err != nil {
		// Discarding this reported the empty result as "couldn't find a VDI",
		// sending the user after a missing ISO when the session had expired.
		ui.Error(fmt.Sprintf("Unable to look up VDIs named '%s': %s", self.VdiName, err.Error()))
		return multistep.ActionHalt
	}

	if self.SRName != "" {
		vdis, err = vdisInSR(c, vdis, self.SRName)
		if err != nil {
			ui.Error(err.Error())
			return multistep.ActionHalt
		}
	}

	switch {
	case len(vdis) == 0:
		ui.Error(fmt.Sprintf("Couldn't find a VDI named '%s' %s", self.VdiName, scope))
		return multistep.ActionHalt
	case len(vdis) > 1:
		ui.Error(fmt.Sprintf("Found more than one VDI with name '%s' %s. Name must be unique", self.VdiName, scope))
		return multistep.ActionHalt
	}

	vdi := vdis[0]

	vdiUuid, err := xenapi.VDI.GetUUID(c.session, vdi)
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to get UUID of VDI '%s': %s", self.VdiName, err.Error()))
		return multistep.ActionHalt
	}
	state.Put(self.VdiUuidKey, vdiUuid)

	return multistep.ActionContinue
}

// vdisInSR narrows VDI references to those held by the named SR. VDI.get_by_name_label
// searches the whole pool, so without this an identically named ISO on any other SR
// could be picked up and iso_sr would have no say in which.
func vdisInSR(c *Connection, vdis []xenapi.VDIRef, srName string) ([]xenapi.VDIRef, error) {
	sr, err := findSRByNameLabel(c, srName)
	if err != nil {
		return nil, err
	}

	var matched []xenapi.VDIRef
	for _, vdi := range vdis {
		vdiSR, err := xenapi.VDI.GetSR(c.session, vdi)
		if err != nil {
			return nil, fmt.Errorf("unable to get the SR of VDI '%s': %s", vdi, err.Error())
		}
		if vdiSR == sr {
			matched = append(matched, vdi)
		}
	}

	return matched, nil
}

func (self *StepFindVdi) Cleanup(state multistep.StateBag) {}
