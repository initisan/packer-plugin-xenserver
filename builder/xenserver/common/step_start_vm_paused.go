// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"context"
	"fmt"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"

	"xenapi"
)

type StepStartVmPaused struct {
	VmCleanup
}

func (self *StepStartVmPaused) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {

	c := state.Get("client").(*Connection)
	ui := state.Get("ui").(packer.Ui)
	config := state.Get("commonconfig").(CommonConfig)

	ui.Say("Step: Start VM Paused")

	uuid := state.Get("instance_uuid").(string)
	instance, err := xenapi.VM.GetByUUID(c.session, uuid)
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to get VM from UUID '%s': %s", uuid, err.Error()))
		return multistep.ActionHalt
	}

	err = xenapi.VM.SetHVMBootPolicy(c.session, instance, "BIOS order")

	if err != nil {
		ui.Error(fmt.Sprintf("Unable to set HVM boot policy: %s", err.Error()))
		return multistep.ActionHalt
	}

	// Write the keys one at a time. SetHVMBootParams replaces the whole map, so
	// it cleared every boot param the source carried -- including its
	// firmware, whenever the clone builder left the option unset.
	// "cd" boots the hard drive ('c') first, then the CDROM ('d').
	if err = setHVMBootParam(c.session, instance, "order", "cd"); err != nil {
		ui.Error(fmt.Sprintf("Unable to set HVM boot param 'order': %s", err.Error()))
		return multistep.ActionHalt
	}

	// Ask before writing: a BIOS VM with a vTPM cannot start, and saying so here
	// beats letting VM.start below raise NOT_IMPLEMENTED. This has to run even
	// when firmware is unset, because unset is not "no firmware" -- it is the
	// the source's, which the check reads off the VM.
	firmware, err := EnsureVMCanStart(c, instance, config.Firmware)
	if err != nil {
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	// This is the only place the firmware a clone inherited is resolved, and
	// StepTypeBootCommand needs it: a UEFI guest reaches its CD boot prompt too late
	// for a single press to be aimed at.
	state.Put("firmware", firmware)

	switch {
	case config.Firmware != "":
		if err = setHVMBootParam(c.session, instance, "firmware", config.Firmware); err != nil {
			ui.Error(fmt.Sprintf("Unable to set HVM boot param 'firmware': %s", err.Error()))
			return multistep.ActionHalt
		}

	case firmware != "":
		// Nothing to write -- VM.clone already brought the source's firmware
		// over. Say which one that is: it decides whether the install media
		// attached to this VM can boot at all.
		ui.Say(fmt.Sprintf("Keeping the source's firmware (%s)", firmware))
	}

	err = xenapi.VM.Start(c.session, instance, true, false)
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to start VM with UUID '%s': %s", uuid, err.Error()))
		return multistep.ActionHalt
	}

	return multistep.ActionContinue
}
