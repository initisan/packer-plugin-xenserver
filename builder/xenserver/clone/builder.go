// Copyright (c) 2026. Citrix Systems, Inc.

package clone

import (
	"context"
	"errors"
	"fmt"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/communicator"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	"github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	hconfig "github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"

	"xenapi"

	xscommon "github.com/xenserver/packer-plugin-xenserver/builder/xenserver/common"
)

type Builder struct {
	config CloneConfig
	runner multistep.Runner
}

func (self *Builder) ConfigSpec() hcldec.ObjectSpec { return self.config.FlatMapstructure().HCL2Spec() }

func (self *Builder) Prepare(raws ...interface{}) (params []string, warns []string, retErr error) {

	var errs *packer.MultiError

	err := hconfig.Decode(&self.config, &hconfig.DecodeOpts{
		Interpolate: true,
		InterpolateFilter: &interpolate.RenderFilter{
			Exclude: []string{
				"boot_command",
			},
		},
	}, raws...)

	if err != nil {
		// MultiErrorAppend returns the accumulated error; discarding it dropped
		// every config decode failure, including unknown keys.
		errs = packer.MultiErrorAppend(errs, err)
	}

	// CommonConfig.Prepare validates against the communicator, so the default has to
	// be in place before it runs rather than in SSHConfig.Prepare below.
	self.config.SSHConfig.ApplyCommunicatorDefault()

	commonWarns, commonErrs := self.config.CommonConfig.Prepare(&self.config.PackerConfig)
	warns = append(warns, commonWarns...)
	errs = packer.MultiErrorAppend(errs, commonErrs...)
	errs = packer.MultiErrorAppend(errs, self.config.SSHConfig.Prepare(self.config.GetInterpContext())...)

	// Unlike an iso build, a clone comes off an installed source and may have no work
	// to run at all, so say at validate time which of the two it will do (CA-430805).
	if self.config.Comm.Type == "none" {
		if self.config.bootsTheGuest() {
			warns = append(warns, fmt.Sprintf("communicator = \"none\": the build waits up to %s "+
				"(install_timeout) for the guest to power itself off, and fails if it does not.",
				self.config.InstallTimeout))
		} else {
			warns = append(warns, "communicator = \"none\" with no cd_files and no boot_command: "+
				"nothing would run inside the guest, so the build never starts it and exports the "+
				"clone as it came off the source. Set cd_files or boot_command to give it something "+
				"to run, or a communicator to provision it.")
		}
	}

	// The two ways of naming the source are exclusive. One overloaded option took
	// either kind, which left a source VM unbuildable whenever a template shared
	// its name-label, and named no kind to check the resolved object against.
	switch {
	case self.config.CloneTemplate != "" && self.config.CloneVM != "":
		errs = packer.MultiErrorAppend(errs, errors.New("only one of clone_template or clone_vm may be "+
			"specified: clone_template names a source template, clone_vm a source VM."))

	case self.config.CloneTemplate == "" && self.config.CloneVM == "":
		errs = packer.MultiErrorAppend(errs, errors.New("one of clone_template or clone_vm must be "+
			"specified: clone_template names a source template, clone_vm a source VM."))
	}

	// Neither is defaulted on a clone: stepCloneInstance writes vcpus whenever it is set,
	// but cores-per-socket only when both are, so cores_per_socket on its own leaves the
	// source's topology untouched.
	if self.config.CorePerSocket != nil && self.config.VCPUs == nil {
		warns = append(warns, "cores_per_socket is set but vcpus is not, so the clone keeps the source's "+
			"CPU topology and cores_per_socket has no effect. Set vcpus to apply it.")
	}

	if err := xscommon.ValidateCPUTopology(self.config.VCPUs, self.config.CorePerSocket); err != nil {
		errs = packer.MultiErrorAppend(errs, err)
	}

	// A clone inherits its firmware, so an unset firmware cannot be assumed to
	// be UEFI. Asking for Secure Boot without saying so was silently ignored.
	if self.config.SecureBootEnabled() && self.config.Firmware != "uefi" {
		errs = packer.MultiErrorAppend(
			errs, errors.New("secure_boot can only be enabled with firmware = \"uefi\", which must be set explicitly on a clone."))
	}

	// Same rule as secure_boot, for a sharper reason: XAPI checks firmware not when the
	// vTPM is created but at start time, with a bare NOT_IMPLEMENTED. Without this check
	// the build clones, attaches and then dies at VM.start naming neither option.
	if self.config.VTPMEnabled() && self.config.Firmware != "uefi" {
		errs = packer.MultiErrorAppend(
			errs, errors.New("vtpm can only be enabled with firmware = \"uefi\", which must be set explicitly on a clone."))
	}

	if len(errs.Errors) > 0 {
		retErr = errors.New(errs.Error())
	}

	return nil, warns, retErr

}

// connectStep is a method so a test can check what StepConnect is handed.
func (self *Builder) connectStep() *communicator.StepConnect {
	return xscommon.ConnectStep(&self.config.SSHConfig)
}

// finalSteps shuts the guest down and takes the build's media off, then snapshots before
// converting to a template: XAPI refuses VM.snapshot on a template (CA-431807).
func finalSteps() []multistep.Step {
	return []multistep.Step{
		new(xscommon.StepShutdown),
		&xscommon.StepDetachVdi{
			VdiUuidKey: "cd_vdi_uuid",
		},
		new(xscommon.StepCreateSnapshot),
		new(xscommon.StepSetVmToTemplate),
		new(xscommon.StepExport),
	}
}

func (self *Builder) Run(ctx context.Context, ui packer.Ui, hook packer.Hook) (packer.Artifact, error) {
	c, err := xscommon.NewXenAPIClient(self.config.HostIp, self.config.Username, self.config.Password, self.config.InsecureEnabled(), self.config.CACertPath)

	if err != nil {
		return nil, err
	}
	defer c.Logout()
	ui.Say("XAPI client session established")

	// Before the steps, not in one of them: by the time stepCloneInstance runs, a build with
	// -force has already deleted the previous output directory and uploaded a CD image to the
	// pool. Everything the source is asked for is asked here, off the one record.
	src := self.config.cloneSource()
	sourceVM, sourceRecord, err := xscommon.ResolveCloneSource(c, src)
	if err != nil {
		return nil, err
	}

	if err := xscommon.EnsureCloneSourceIsHalted(src, sourceRecord, self.config.VMSR != ""); err != nil {
		return nil, err
	}

	if self.config.DiskSizeGB != nil {
		if err := xscommon.EnsureCloneSourceHasPrimaryDisk(c, src, sourceVM); err != nil {
			return nil, err
		}
	}

	// Only asked on the path that starts the VM -- the other one never writes firmware
	// to the clone at all, so there is no boot for it to break.
	if self.config.bootsTheGuest() {
		if err := xscommon.CloneSourceSupportsFirmware(src, sourceRecord,
			self.config.Firmware, self.config.removesVTPM()); err != nil {
			return nil, err
		}
	}

	state := new(multistep.BasicStateBag)
	state.Put("client", c)
	state.Put("source_vm", sourceVM)
	state.Put("config", self.config)
	state.Put("commonconfig", self.config.CommonConfig)
	state.Put("hook", hook)
	state.Put("ui", ui)

	httpReqChan := make(chan string, 1)

	steps := []multistep.Step{
		&xscommon.StepPrepareOutputDir{
			Force: self.config.PackerForce,
			Path:  self.config.OutputDir,
		},
		&commonsteps.StepCreateCD{
			Files: self.config.CDFiles,
			Label: "cddata",
		},
		new(xscommon.StepHTTPIPDiscover),
		&xscommon.StepHTTPServer{
			Chan: httpReqChan,
		},
		&xscommon.StepUploadVdi{
			VdiNameFunc: func() string {
				return "Packer-cd-disk"
			},
			ImagePathFunc: func() string {
				if cdPath, ok := state.GetOk("cd_path"); ok {
					return cdPath.(string)
				}
				return ""
			},
			VdiUuidKey: "cd_vdi_uuid",
		},
		new(stepCloneInstance),
		&xscommon.StepAttachvGPU{
			VGPUName: self.config.VGPUProfile,
		},
		&xscommon.StepAttachVdi{
			VdiUuidKey: "cd_vdi_uuid",
			VdiType:    xenapi.VbdTypeCD,
		},
	}

	// Only these four steps need a running VM. StepConnect stays: with communicator =
	// "none", the only way here, it makes no connection, it just puts the SDK's null
	// communicator in the state bag, which StepProvision expects to find.
	if self.config.bootsTheGuest() {
		steps = append(steps,
			new(xscommon.StepStartVmPaused),
			new(xscommon.StepBootWait),
			&xscommon.StepTypeBootCommand{
				Ctx: *self.config.GetInterpContext(),
			},
			xscommon.InstallWaitStep(self.config.Comm.Type, httpReqChan, self.config.InstallTimeout),
		)
	} else {
		ui.Say("Nothing would run inside this guest, so the build does not start it: the clone " +
			"is exported as it came off the source. Set cd_files or boot_command to give it " +
			"something to run, or a communicator to provision it.")
	}

	steps = append(steps, self.connectStep(), new(commonsteps.StepProvision))
	steps = append(steps, finalSteps()...)

	self.runner = &multistep.BasicRunner{Steps: steps}
	self.runner.Run(ctx, state)

	if rawErr, ok := state.GetOk("error"); ok {
		return nil, rawErr.(error)
	}

	if _, ok := state.GetOk(multistep.StateCancelled); ok {
		return nil, errors.New("Build was cancelled.")
	}
	if _, ok := state.GetOk(multistep.StateHalted); ok {
		return nil, errors.New("Build was halted.")
	}

	artifact, err := xscommon.NewArtifact(self.config.OutputDir)
	if err != nil {
		return nil, fmt.Errorf("could not list the exported files in %s: %w", self.config.OutputDir, err)
	}

	return artifact, nil
}
