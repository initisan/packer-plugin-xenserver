// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/packer"

	"xenapi"
)

// shutdownTimeout bounds the wait for a shutdown_command to take the VM down before
// falling back to pulling the plug.
const shutdownTimeout = 300 * time.Second

type StepShutdown struct{}

func (StepShutdown) Run(ctx context.Context, state multistep.StateBag) multistep.StepAction {
	config := state.Get("commonconfig").(CommonConfig)
	ui := state.Get("ui").(packer.Ui)
	c := state.Get("client").(*Connection)
	instance_uuid := state.Get("instance_uuid").(string)

	instance, err := xenapi.VM.GetByUUID(c.session, instance_uuid)
	if err != nil {
		ui.Error(fmt.Sprintf("Could not get VM with UUID '%s': %s", instance_uuid, err.Error()))
		return multistep.ActionHalt
	}

	ui.Say("Step: Shutting down VM")

	// The VM can already be Halted: a build with no communicator has waited for the guest
	// to power itself off, and a clone build that boots nothing never started it. Asking
	// XAPI to shut it down again would fail, and every failure here ends in a hard shutdown.
	powerState, err := xenapi.VM.GetPowerState(c.session, instance)
	if err != nil {
		ui.Error(fmt.Sprintf("Could not read the power state of VM '%s': %s", instance_uuid, err.Error()))
		return multistep.ActionHalt
	}
	if powerState == xenapi.VMPowerStateHalted {
		ui.Message("The VM has already shut down")
		return multistep.ActionContinue
	}

	if !shutdownGracefully(ctx, state, config, ui, c, instance) {
		// Cancelled, not a guest failure: leave the VM to the cleanup path, which honours keep_vm.
		if _, cancelled := state.GetOk(multistep.StateCancelled); cancelled {
			return multistep.ActionHalt
		}

		// The graceful path can also fail with the VM already down: a hung communicator, a
		// transient GetPowerState error, or a halt inside the last poll interval. XAPI
		// rejects hard_shutdown on a Halted VM, failing an otherwise good build.
		if powerState, err := xenapi.VM.GetPowerState(c.session, instance); err == nil &&
			powerState == xenapi.VMPowerStateHalted {
			ui.Message("The VM is already Halted, so there is nothing to force")
		} else {
			ui.Say("WARNING: Forcing hard shutdown of the VM...")
			if err := xenapi.VM.HardShutdown(c.session, instance); err != nil {
				ui.Error(fmt.Sprintf("Could not hard shut down VM -- giving up: %s", err.Error()))
				return multistep.ActionHalt
			}
		}
	}

	ui.Message("Successfully shut down VM")
	return multistep.ActionContinue
}

// shutdownGracefully reports whether the VM was brought down without forcing it.
func shutdownGracefully(ctx context.Context, state multistep.StateBag, config CommonConfig,
	ui packer.Ui, c *Connection, instance xenapi.VMRef) bool {

	if config.ShutdownCommand == "" {
		ui.Message("Attempting to cleanly shutdown the VM...")
		if err := xenapi.VM.CleanShutdown(c.session, instance); err != nil {
			ui.Error(fmt.Sprintf("Could not shut down VM: %s", err.Error()))
			return false
		}
		return true
	}

	remaining := runShutdownCommand(ctx, state, ui, config.ShutdownCommand, shutdownTimeout)
	if remaining <= 0 {
		ui.Error(fmt.Sprintf(
			"The shutdown command used the whole %s allowed for a graceful shutdown without the VM halting",
			shutdownTimeout))
		return false
	}

	ui.Message("Waiting for VM to enter Halted state...")
	err := InterruptibleWait{
		Predicate: func() (bool, error) {
			powerState, err := xenapi.VM.GetPowerState(c.session, instance)
			return powerState == xenapi.VMPowerStateHalted, err
		},
		PredicateInterval: 5 * time.Second,
		Timeout:           remaining,
	}.Wait(state)

	if err != nil {
		ui.Error(fmt.Sprintf("Error waiting for VM to halt: %s", err.Error()))
		return false
	}
	return true
}

// runShutdownCommand runs the guest shutdown command against the graceful-shutdown
// budget, and reports how much of that budget is left for the VM to actually halt.
//
// The deadline is the point: without one, a shutdown_command that never returns parks the
// build inside RunWithUi for as long as the communicator stays up and the hard-shutdown
// fallback is never reached. One deadline spans both halves, so a slow command eats into
// the wait rather than adding to it.
func runShutdownCommand(ctx context.Context, state multistep.StateBag, ui packer.Ui,
	command string, budget time.Duration) (remaining time.Duration) {

	deadline := time.Now().Add(budget)
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	ui.Message("Executing shutdown command...")
	if err := runInGuest(ctx, state, ui, command); err != nil {
		// Not fatal on its own: a command that halts the machine it runs on often
		// drops the connection before it can report anything. The power state is the
		// only trustworthy verdict, so let the wait decide.
		ui.Message(fmt.Sprintf("Shutdown command did not report success (%s); "+
			"waiting to see whether the VM halts anyway", err))
	}

	return time.Until(deadline)
}

// runInGuest executes command inside the guest over the build's own communicator, so it
// works for WinRM as well as SSH. It replaces an SSH tunnel through dom0, which XenServer
// 9 rules out by disabling host SSH and which never carried WinRM anyway.
func runInGuest(ctx context.Context, state multistep.StateBag, ui packer.Ui, command string) error {
	comm, ok := state.Get("communicator").(packer.Communicator)
	if !ok || comm == nil {
		return errors.New("no guest communicator has been established to run it on")
	}

	cmd := &packer.RemoteCmd{Command: command}
	if err := cmd.RunWithUi(ctx, comm, ui); err != nil {
		return err
	}

	// CmdDisconnect means the guest hung up, which is the normal outcome of a
	// successful shutdown command rather than a failure.
	if status := cmd.ExitStatus(); status != 0 && status != packer.CmdDisconnect {
		return fmt.Errorf("exit status %d", status)
	}
	return nil
}

func (StepShutdown) Cleanup(state multistep.StateBag) {}
