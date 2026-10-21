// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"
	"log"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"

	"xenapi"
)

// Vars, not constants, so a test can exercise the retry without waiting it out.
var (
	vdiDestroyAttempts = 3
	vdiDestroyBackoff  = 5 * time.Second
)

type VmCleanup struct{}

func (self *VmCleanup) Cleanup(state multistep.StateBag) {
	config := state.Get("commonconfig").(CommonConfig)
	c := state.Get("client").(*Connection)
	ui := state.Get("ui").(packer.Ui)

	if config.ShouldKeepVM(state) {
		return
	}

	uuid := state.Get("instance_uuid").(string)
	instance, err := xenapi.VM.GetByUUID(c.session, uuid)
	if err != nil {
		log.Printf("Unable to get VM from UUID '%s': %s", uuid, err.Error())
		return
	}

	left := 0

	// Collected before the VM is destroyed: VM.destroy leaves the VDIs behind but takes
	// the VBDs that name them with it.
	disks, err := ClonedDisks(c, instance)
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to list the disks of VM '%s', so they are left behind: %s", uuid, err.Error()))
		left++
	}

	ui.Say(fmt.Sprintf("Found %d disk(s) attached to VM", len(disks)))

	// Shutdown the VM if it's running
	vmstate, err := xenapi.VM.GetPowerState(c.session, instance)
	if err != nil {
		// Unreadable is not halted: the zero value skips the stop below and VM.destroy
		// then refuses a running VM. A shutdown of an already-halted VM is only logged.
		log.Printf("Unable to get VM power state '%s': %s", uuid, err.Error())
		vmstate = xenapi.VMPowerStateRunning
	}

	switch vmstate {
	case xenapi.VMPowerStateRunning:
		ui.Say(fmt.Sprintf("Shutting down VM on cleanup: %s", uuid))
		err = xenapi.VM.Shutdown(c.session, instance)
		if err != nil {
			log.Printf("Unable to shutdown VM '%s': %s", uuid, err.Error())
			err = xenapi.VM.HardShutdown(c.session, instance)
			if err != nil {
				log.Printf("Unable to hard shutdown VM '%s': %s", uuid, err.Error())
			}
		}
	case xenapi.VMPowerStatePaused:
		// Paused VMs cannot receive a clean shutdown signal; hard-shutdown directly.
		ui.Say(fmt.Sprintf("Force-stopping paused VM on cleanup: %s", uuid))
		err = xenapi.VM.HardShutdown(c.session, instance)
		if err != nil {
			log.Printf("Unable to hard shutdown paused VM '%s': %s", uuid, err.Error())
		}
	}

	ui.Say(fmt.Sprintf("Destroying VM on cleanup: %s", uuid))
	err = xenapi.VM.Destroy(c.session, instance)
	if err != nil {
		ui.Error(fmt.Sprintf("Unable to destroy VM '%s': %s", uuid, err.Error()))
		left++
	}

	// After VM destruction, so the disks are detached.
	ui.Say(fmt.Sprintf("Destroying %d disk(s)...", len(disks)))
	for _, disk := range disks {
		_ = xenapi.VBD.Unplug(c.session, disk.VBD)
		if err := destroyVDI(c, ui, disk.VDI); err != nil {
			ui.Error(err.Error())
			left++
		}
	}

	// Said only when it is true: an operator told the pool is clean does not go looking,
	// and a VM left running holds its disks and the vm_name of the next build.
	if left > 0 {
		ui.Error(fmt.Sprintf("Cleanup left %d object(s) of VM '%s' on the pool, to be removed by hand", left, uuid))
		return
	}
	ui.Say("VM and disk cleanup completed")
}

// PreCleanup clears the way for a build whose vm_name is already taken: with force it
// destroys each matching VM or template and the non-CD disks it owns, without force it
// returns an error naming the clash. src and sourceVM name the object this build clones
// from, which matches by name-label like any other and must survive.
func PreCleanup(state multistep.StateBag, force bool, src CloneSource, sourceVM xenapi.VMRef) error {
	c := state.Get("client").(*Connection)
	ui := state.Get("ui").(packer.Ui)
	config := state.Get("commonconfig").(CommonConfig)

	ui.Say("Step: PreCleanup - Checking for existing VMs with name '" + config.Name() + "'")

	vms, err := xenapi.VM.GetByNameLabel(c.session, config.Name())
	if err != nil {
		log.Printf("Unable to get VM from Name '%s': %s", config.Name(), err.Error())
		return err
	}

	ui.Say(fmt.Sprintf("PreCleanup: Found %d existing VM(s)", len(vms)))

	for _, vm := range vms {
		if vm == sourceVM {
			return fmt.Errorf("vm_name '%s' names this build's own source, %s: -force would "+
				"destroy it and its disks before the clone was taken. Give the build a "+
				"different vm_name, or rename the source", config.Name(), src)
		}
	}

	if force && len(vms) > 0 {
		// -force removes templates too: a successful build converts its VM into one, so a
		// re-run has to be able to clear the previous output.
		ui.Say(fmt.Sprintf("Found %d VM(s) / Template(s) named %s. Deleting due to -force flag", len(vms), config.Name()))

		for _, vm := range vms {
			// Check if this is a template
			isTemplate, err := xenapi.VM.GetIsATemplate(c.session, vm)
			if err != nil {
				return fmt.Errorf("error checking if %s is a template: %v", config.Name(), err)
			}

			if isTemplate {
				// Templates too: see above.
				ui.Say(fmt.Sprintf("Destroying template on -force: %s", config.Name()))
			}

			disks, err := ClonedDisks(c, vm)
			if err != nil {
				log.Printf("Unable to get VBDs for VM '%s': %s", config.Name(), err.Error())
			}

			vmstate, err := xenapi.VM.GetPowerState(c.session, vm)
			if err != nil {
				return fmt.Errorf("error getting power state of VM %s: %v", config.Name(), err)
			}
			ui.Say(fmt.Sprintf("VM %s is in state: %v", config.Name(), vmstate))

			switch vmstate {
			case xenapi.VMPowerStateRunning:
				ui.Say(fmt.Sprintf("Shutting down running VM: %s", config.Name()))
				err = xenapi.VM.Shutdown(c.session, vm)
				if err != nil {
					return fmt.Errorf("error shutting down %s: %v", config.Name(), err)
				}
				ui.Say(fmt.Sprintf("Successfully shut down VM: %s", config.Name()))
			case xenapi.VMPowerStatePaused:
				err = xenapi.VM.HardShutdown(c.session, vm)
				if err != nil {
					return fmt.Errorf("error hard-shutting down paused VM %s: %v", config.Name(), err)
				}
			}

			ui.Say(fmt.Sprintf("Destroying VM: %s", config.Name()))
			err = xenapi.VM.Destroy(c.session, vm)
			if err != nil {
				return fmt.Errorf("error destroying %s: %v", config.Name(), err)
			}
			ui.Say(fmt.Sprintf("Successfully destroyed VM: %s", config.Name()))

			ui.Say(fmt.Sprintf("Destroying %d disk(s) for VM %s", len(disks), config.Name()))
			for _, disk := range disks {
				// Not fatal: -force has freed the vm_name, which is what the build
				// needs. A disk it could not take is an orphan worth saying so about.
				if err := destroyVDI(c, ui, disk.VDI); err != nil {
					ui.Error(err.Error())
				}
			}
		}
	}
	if !force && len(vms) > 0 {
		return fmt.Errorf("%s already exists, you can use -force flag to destroy it", config.Name())
	}

	return nil
}

// destroyVDI destroys one disk, retrying because VDI_IN_USE outlasts the VM.destroy that
// released it. The error is returned rather than fatal: the caller is tearing down, and
// the remaining disks still have to go.
func destroyVDI(c *Connection, ui packer.Ui, vdi xenapi.VDIRef) error {
	vdiUuid, err := xenapi.VDI.GetUUID(c.session, vdi)
	if err != nil {
		return fmt.Errorf("unable to get VDI UUID: %w", err)
	}

	ui.Say(fmt.Sprintf("Destroying VDI (disk): %s", vdiUuid))

	var lastErr error
	for attempt := 1; attempt <= vdiDestroyAttempts; attempt++ {
		if attempt > 1 {
			// Back-to-back attempts cannot outlast the transient VDI_IN_USE this
			// retry exists for.
			time.Sleep(vdiDestroyBackoff)
		}

		lastErr = xenapi.VDI.Destroy(c.session, vdi)
		if lastErr == nil {
			ui.Say(fmt.Sprintf("Successfully destroyed VDI: %s", vdiUuid))
			return nil
		}
		if attempt < vdiDestroyAttempts {
			log.Printf("Attempt %d to destroy VDI '%s' failed: %s. Retrying...", attempt, vdiUuid, lastErr.Error())
		}
	}

	return fmt.Errorf("unable to destroy VDI '%s' after %d attempts: %w", vdiUuid, vdiDestroyAttempts, lastErr)
}
