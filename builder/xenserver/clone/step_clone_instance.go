// Copyright (c) 2026. Citrix Systems, Inc.

package clone

import (
	"context"
	"fmt"
	"log"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	"xenapi"

	xscommon "github.com/xenserver/packer-plugin-xenserver/builder/xenserver/common"
)

type stepCloneInstance struct {
	instance *xenapi.VMRef
	disks    []xscommon.ClonedDisk
}

func (self *stepCloneInstance) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {

	c := state.Get("client").(*xscommon.Connection)
	config := state.Get("config").(CloneConfig)
	ui := state.Get("ui").(packer.Ui)

	ui.Say("Step: Create Instance")

	// Resolved and vetted in Run, before the output directory was prepared and the
	// CD uploaded: by here the pool has already been written to.
	src := config.cloneSource()
	source := state.Get("source_vm").(xenapi.VMRef)

	// Run Pre-Cleanup to check if VM not already exists
	err := xscommon.PreCleanup(state, config.PackerForce, src, source)
	if err != nil {
		state.Put("error", err)
		return multistep.ActionHalt
	}

	// vm_sr names the SR the build's disks belong on, and VM.clone cannot place
	// them: it clones each disk onto the SR that disk is already on. VM.copy
	// takes a target SR, at the price of copying every disk in full, so it is
	// used only when the source is not already where the build asked for
	// (CA-430509).
	var targetSR xenapi.SRRef
	needsCopy := false
	if config.VMSR != "" {
		targetSR, err = config.GetSR(c)
		if err != nil {
			// Until CA-431812 this was the one builder that never resolved the
			// name, so a vm_sr matching no SR was accepted and the build
			// reported success having ignored it.
			ui.Error(fmt.Sprintf("Could not resolve vm_sr: %s", err.Error()))
			state.Put("error", err)
			return multistep.ActionHalt
		}
		needsCopy, err = xscommon.TemplateNeedsCopy(c, source, targetSR)
		if err != nil {
			ui.Error(err.Error())
			state.Put("error", err)
			return multistep.ActionHalt
		}
	}

	var instance xenapi.VMRef
	if needsCopy {
		// Said before the call, not after: this is the step that makes a build
		// with vm_sr take minutes where the others take seconds, and the user
		// is otherwise watching a line that does not move.
		ui.Say(fmt.Sprintf("Copying '%s' onto SR '%s'. Not all of its disks are there already, and the only "+
			"call that places them copies every one of them in full rather than cloning it copy-on-write, "+
			"so this takes as long as the data takes to write. Leave vm_sr unset to clone in place instead; "+
			"on a build with cd_files that also sends the CD to the pool's default SR.",
			src.Name, config.VMSR))
		instance, err = xenapi.VM.Copy(c.GetSession(), source, config.Name(), targetSR)
	} else {
		instance, err = xenapi.VM.Clone(c.GetSession(), source, config.Name())
	}
	if err != nil {
		// A copy is refused where a clone would not be -- an ISO target SR
		// (xapi_vm.ml copy), or a source VDI with on_boot=reset (xapi_vdi.ml) --
		// and XAPI names neither vm_sr nor the copy in the code it raises.
		hint := ""
		if needsCopy {
			hint = " (this build copies rather than clones because vm_sr names an SR the source is not on)"
		}
		ui.Error(fmt.Sprintf("Error creating the VM from '%s'%s: %s", src.Name, hint, err.Error()))
		state.Put("error", err)
		return multistep.ActionHalt
	}
	self.instance = &instance

	// Either call gives the new VM its own disks; record them so Cleanup can take them
	// with the VM. Teardown bookkeeping only, so a failed read is not worth losing an
	// otherwise good clone over.
	self.disks, err = xscommon.ClonedDisks(c, instance)
	if err != nil {
		ui.Error(fmt.Sprintf("Could not list the cloned VM's disks, so cleanup may leave them behind: %s", err.Error()))
	}

	err = xenapi.VM.SetIsATemplate(c.GetSession(), instance, false)
	if err != nil {
		ui.Error(fmt.Sprintf("Error setting is_a_template=false: %s", err.Error()))
		return multistep.ActionHalt
	}

	// Only an explicit value writes anything: XenServer warns that moving the flag under
	// an installed Windows is likely to stop it booting, so unset keeps the source's.
	if config.HasVendorDevice != nil {
		vendorDeviceChanged, err := xscommon.ApplyVendorDevice(c, instance, *config.HasVendorDevice)
		if err != nil {
			ui.Error(err.Error())
			return multistep.ActionHalt
		}
		if vendorDeviceChanged {
			ui.Say(fmt.Sprintf("Set has_vendor_device=%t on the clone; '%s' had it the other way round. "+
				"If that source already has Windows installed, this is likely to stop it booting: "+
				"remove has_vendor_device to keep the source's setting.",
				*config.HasVendorDevice, src.Name))
		}
	}

	// VM.clone copies the source's vTPM and creates none otherwise, so "give this image a
	// vTPM" and "give it none" both have to be acted on here, while the VM is halted.
	if config.VTPM != nil {
		outcome, err := xscommon.ReconcileVTPM(c, instance, config.VTPMEnabled())
		if err != nil {
			ui.Error(err.Error())
			return multistep.ActionHalt
		}

		switch outcome {
		case xscommon.VTPMCreated:
			ui.Say(fmt.Sprintf("Attached a vTPM: '%s' had none", src.Name))
		case xscommon.VTPMDestroyed:
			ui.Say(fmt.Sprintf("Destroyed the vTPM inherited from '%s'. Anything sealed to it, such as a "+
				"BitLocker key, is now unrecoverable in this build's VM. The source is untouched. "+
				"Remove vtpm = false to keep the inherited vTPM.", src.Name))
		case xscommon.VTPMDeclarationCleared:
			ui.Say(fmt.Sprintf("Cleared platform:vtpm, which '%s' declares: the clone had no vTPM to remove, "+
				"but XenServer would have created one from that key at start.", src.Name))
		}
	}

	// One value drives both the maximum and the at-startup count; leaving it unset
	// keeps whatever the source has.
	if config.VCPUs != nil {
		vcpus := *config.VCPUs
		// XAPI holds VCPUs_at_startup <= VCPUs_max in both setters, so step the inherited
		// startup count out of the way before lowering the maximum.
		err = xenapi.VM.SetVCPUsAtStartup(c.GetSession(), instance, 1)
		if err != nil {
			ui.Error(fmt.Sprintf("Error setting VM VCPUs At Startup=1: %s", err.Error()))
			return multistep.ActionHalt
		}

		err = xenapi.VM.SetVCPUsMax(c.GetSession(), instance, int(vcpus))
		if err != nil {
			ui.Error(fmt.Sprintf("Error setting VM VCPUs Max=%d: %s", vcpus, err.Error()))
			return multistep.ActionHalt
		}

		err = xenapi.VM.SetVCPUsAtStartup(c.GetSession(), instance, int(vcpus))
		if err != nil {
			ui.Error(fmt.Sprintf("Error setting VM VCPUs At Startup=%d: %s", vcpus, err.Error()))
			return multistep.ActionHalt
		}
	}

	if config.VMMemoryGB != nil {
		memory := int(*config.VMMemoryGB) * xscommon.BytesPerGB
		err = xenapi.VM.SetMemoryLimits(c.GetSession(), instance, memory, memory, memory, memory)
		if err != nil {
			ui.Error(fmt.Sprintf("Error setting VM memory=%d: %s", memory, err.Error()))
			return multistep.ActionHalt
		}
	}

	// After the vCPU count above: the topology is only valid against the VCPUs_max the VM
	// ends up with. Prepare has already rejected a pair the build set that does not divide.
	topology, err := xscommon.ReconcileCPUTopology(c, instance, config.VCPUs, config.CorePerSocket)
	if err != nil {
		ui.Error(err.Error())
		return multistep.ActionHalt
	}
	ui.Say(topology.Describe(src.Name))

	// Unset keeps the source's: writing "false" to be safe is what turns Secure Boot off
	// on a source that had it.
	secureBoot, err := xscommon.ReconcileSecureBoot(c, instance, config.SecureBoot)
	if err != nil {
		ui.Error(err.Error())
		return multistep.ActionHalt
	}
	ui.Say(secureBoot.Describe(src.Name))

	// Unset keeps the source's, like every other option here. vm_description is a plain
	// string, so "" is the only spelling of unset it has, and writing it blanked the
	// name_description VM.clone copied over.
	if config.VMDescription != "" {
		err = xenapi.VM.SetNameDescription(c.GetSession(), instance, config.VMDescription)
		if err != nil {
			ui.Error(fmt.Sprintf("Error setting VM description: %s", err.Error()))
			return multistep.ActionHalt
		}
	}

	// vm_other_config plus the build-provenance stamp (F6.1). Unconditional: the VM must
	// be identifiable as plugin-built even with no vm_other_config set.
	err = xscommon.StampVM(c, instance, config.VMOtherConfig)
	if err != nil {
		ui.Error(err.Error())
		return multistep.ActionHalt
	}

	// Size the primary disk if a size is specified. The VM is still halted here,
	// which is what VDI.resize needs.
	if config.DiskSizeGB != nil {
		outcome, err := xscommon.SizePrimaryDisk(c, instance, int(*config.DiskSizeGB)*xscommon.BytesPerGB)
		if err != nil {
			ui.Error(fmt.Sprintf("%s (the source is '%s')", err.Error(), src.Name))
			return multistep.ActionHalt
		}
		if outcome == xscommon.DiskGrown {
			ui.Say(fmt.Sprintf("Grew the primary disk to %d GB", *config.DiskSizeGB))
		}
	}

	// A clone inherits the source's VIFs, so network_names -- which means exactly these
	// NICs -- has to destroy them first or VIF.create hits DEVICE_ALREADY_EXISTS. Unset
	// keeps them, falling back to the management network only when there are none.
	if len(config.NetworkNames) == 0 {
		inheritedVIFs, err := xscommon.CountVIFs(c, instance)
		if err != nil {
			ui.Error(err.Error())
			return multistep.ActionHalt
		}

		if inheritedVIFs > 0 {
			ui.Say(fmt.Sprintf("No network_names given; keeping the %d NIC(s) inherited from the source", inheritedVIFs))
			log.Printf("Source supplied %d VIF(s); not creating a management VIF\n", inheritedVIFs)
		} else {
			log.Println("No network name given, attempting to use management interface")
			if err := xscommon.AttachManagementNetwork(c, instance, ui.Say); err != nil {
				ui.Error(err.Error())
				return multistep.ActionHalt
			}
		}

	} else {
		if err := xscommon.DetachAllVIFs(c, instance, ui.Say); err != nil {
			ui.Error(err.Error())
			return multistep.ActionHalt
		}

		log.Printf("Using provided network names: %v\n", config.NetworkNames)
		if err := xscommon.AttachNamedNetworks(c, instance, config.NetworkNames, ui.Say); err != nil {
			ui.Error(err.Error())
			return multistep.ActionHalt
		}
	}

	instanceId, err := xenapi.VM.GetUUID(c.GetSession(), instance)
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to get VM UUID: %s", err.Error()))
		return multistep.ActionHalt
	}

	state.Put("instance_uuid", instanceId)
	ui.Say(fmt.Sprintf("Created instance '%s'", instanceId))

	return multistep.ActionContinue
}

// Cleanup destroys the cloned VM and the disks it was cloned with. VM.destroy
// leaves VDIs behind, so they are destroyed here rather than left to VmCleanup,
// which a build that fails before StepStartVmPaused never reaches.
func (self *stepCloneInstance) Cleanup(state multistep.StateBag) {
	config := state.Get("config").(CloneConfig)
	if config.ShouldKeepVM(state) {
		return
	}

	ui := state.Get("ui").(packer.Ui)
	c := state.Get("client").(*xscommon.Connection)

	if self.instance != nil {
		ui.Say("Destroying VM")
		_ = xenapi.VM.HardShutdown(c.GetSession(), *self.instance) // redundant, just in case
		if err := xenapi.VM.Destroy(c.GetSession(), *self.instance); err != nil {
			log.Printf("Unable to destroy the cloned VM: %s", err.Error())
		}
	}

	// Sweep the disks whether or not the VM went away: VM.destroy leaves them
	// behind when it succeeds, and when it fails they are exactly what leaks.
	if len(self.disks) > 0 {
		ui.Say(fmt.Sprintf("Destroying %d disk(s)...", len(self.disks)))
		for _, disk := range self.disks {
			if err := xenapi.VDI.Destroy(c.GetSession(), disk.VDI); err != nil {
				log.Printf("Unable to destroy VDI: %s", err.Error())
			}
		}
	}
}
