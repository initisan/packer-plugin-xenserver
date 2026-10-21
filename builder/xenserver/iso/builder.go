// Copyright (c) 2026. Citrix Systems, Inc.

package iso

import (
	"context"
	"errors"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2/hcldec"
	"github.com/hashicorp/packer-plugin-sdk/communicator"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	commonsteps "github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"
	"github.com/hashicorp/packer-plugin-sdk/packer"
	hconfig "github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"

	"xenapi"

	xscommon "github.com/xenserver/packer-plugin-xenserver/builder/xenserver/common"
)

type Builder struct {
	config ISOConfig
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

	// An iso build always boots install media, so there is always an end to wait for.
	// Without a communicator to notice it, the guest powering itself off is the only
	// signal left.
	if self.config.Comm.Type == "none" {
		warns = append(warns, fmt.Sprintf("communicator = \"none\": the build waits up to %s "+
			"(install_timeout) for the guest to power itself off, and fails if it does not. Make the "+
			"unattended installation shut the guest down when it finishes.", self.config.InstallTimeout))
	}

	if self.config.GuestToolsISO != "" {
		warns = append(warns, "guest_tools_iso is deprecated: XenServer 8 and later ship no guest-tools ISO. "+
			"Set it only against XCP-ng or legacy pools that still publish one.")
	}

	// Required rather than defaulted: the option decides the VM's virtual hardware, and on
	// genericTemplate the sizing below with it (CA-431859).
	if strings.TrimSpace(self.config.CloneTemplate) == "" {
		errs = packer.MultiErrorAppend(errs, errors.New("clone_template must be specified: the "+
			"name-label of the template to clone as the VM's starting point. "+
			xscommon.TemplateLookupHint))
	}

	// clone_template is the build saying what the VM should look like, and an unset sizing
	// option leaves the template's alone, the same contract as firmware and secure_boot.
	// These used to be defaulted and written regardless, so naming a template sized for
	// the guest OS did nothing (CA-431806).
	//
	// genericTemplate describes no guest OS and so carries nothing to inherit. There is no
	// size this builder could pick for it that is not wrong for some guest, so the build
	// has to say (CP-314723).
	for _, option := range missingGenericSizing(self.config) {
		errs = packer.MultiErrorAppend(errs, fmt.Errorf("%s must be specified with clone_template "+
			"'%s': that template describes no guest OS, so it supplies no %s for the VM to inherit.",
			option, genericTemplate, option))
	}

	// Not an error: Prepare cannot tell which guest the ISO installs, and a Linux one
	// installs happily in 1 GB. Above zero only, because a zero is already an error from
	// CommonConfig.Prepare (CA-431784).
	if self.config.VMMemoryGB != nil && *self.config.VMMemoryGB > 0 &&
		*self.config.VMMemoryGB < minWindowsMemoryGB {
		warns = append(warns, fmt.Sprintf("vm_memory_gb = %d is below the %d GB Microsoft documents as the "+
			"minimum for installing any supported Windows guest; 1 GB is the exact configuration it documents "+
			"as failing to install Windows Server 2025. Windows Setup will not report this: it stalls, and the "+
			"build fails after install_timeout. Ignore this if the ISO installs something else.",
			*self.config.VMMemoryGB, minWindowsMemoryGB))
	}

	// Not without a clone_template to name: the build has already failed above, and the
	// warning would quote an empty one.
	if self.config.CorePerSocket != nil && self.config.VCPUs == nil &&
		strings.TrimSpace(self.config.CloneTemplate) != "" {
		warns = append(warns, fmt.Sprintf("cores_per_socket is ignored without vcpus: the VM keeps the CPU "+
			"topology of clone_template '%s'. Set vcpus as well to choose the topology.", self.config.CloneTemplate))
	}

	if err := xscommon.ValidateCPUTopology(self.config.VCPUs, self.config.CorePerSocket); err != nil {
		errs = packer.MultiErrorAppend(errs, err)
	}

	// firmware is deliberately not defaulted: the VM is cloned from clone_template, so
	// unset means "keep the template's". Defaulting it to bios wrote bios over a Windows 11
	// template's uefi, and its installer refuses to run on the result.

	// The two ways of naming the install media are exclusive: iso_name takes an ISO
	// already on the ISO SR, iso_url downloads one and uploads it. Naming both used
	// to be accepted, and the URL silently ignored.
	hasISOUrl := self.config.RawSingleISOUrl != "" || len(self.config.ISOUrls) > 0
	switch {
	case self.config.ISOName != "" && hasISOUrl:
		errs = packer.MultiErrorAppend(errs, errors.New("only one of iso_name or iso_url/iso_urls may be "+
			"specified: iso_name takes an ISO already present on iso_sr, iso_url downloads one and uploads "+
			"it to the pool."))

	case self.config.ISOName == "" && !hasISOUrl:
		errs = packer.MultiErrorAppend(errs, errors.New("one of iso_name or iso_url/iso_urls must be "+
			"specified: iso_name names an ISO already present on iso_sr, iso_url an ISO to download."))

	case hasISOUrl:
		// The SDK owns the URL list and the checksum from here: it folds
		// iso_url into iso_urls, defaults iso_target_extension, and rewrites
		// iso_checksum into its canonical "type:hash" form.
		isoWarns, isoErrs := self.config.ISOConfig.Prepare(self.config.GetInterpContext())
		warns = append(warns, isoWarns...)
		errs = packer.MultiErrorAppend(errs, isoErrs...)

	default:
		// iso_name mode downloads nothing, so the download options are read by no one.
		if unused := setDownloadOptions(&self.config.ISOConfig); len(unused) > 0 {
			errs = packer.MultiErrorAppend(errs, fmt.Errorf("iso_name attaches an ISO already present on "+
				"iso_sr, so nothing is downloaded: the download options set here (%s) apply to "+
				"iso_url/iso_urls only.", strings.Join(unused, ", ")))
		}
	}

	// iso_sr is the SR iso_name is looked up in, so an iso_name build without one
	// cannot work; it used to fail mid-build instead. A downloaded ISO does not need
	// it: that upload creates a VDI, which goes to vm_sr.
	if self.config.ISOName != "" && self.config.ISOSR == "" {
		errs = packer.MultiErrorAppend(errs, errors.New("iso_sr must be specified with iso_name: it names "+
			"the SR the install ISO is looked up in."))
	}

	// Both need firmware named here rather than left to the template: Prepare has no XenAPI
	// session, so the template's value is not knowable at validation time.
	if self.config.VTPMEnabled && self.config.Firmware != "uefi" {
		errs = packer.MultiErrorAppend(
			errs, errors.New("vtpm can only be enabled with firmware = \"uefi\", which must be set explicitly."))
	}

	if self.config.SecureBootEnabled() && self.config.Firmware != "uefi" {
		errs = packer.MultiErrorAppend(
			errs, errors.New("secure_boot can only be enabled with firmware = \"uefi\", which must be set explicitly."))
	}

	if len(errs.Errors) > 0 {
		retErr = errors.New(errs.Error())
	}

	return nil, warns, retErr

}

// missingGenericSizing names the sizing options genericTemplate has none of and the build
// left out. Empty for every other clone_template, whose sizing is not knowable until Run
// opens a session.
//
// cores_per_socket is deliberately not among them: a VM with no topology of its own gets
// one core per socket, which is a working VM rather than a missing size.
func missingGenericSizing(c ISOConfig) []string {
	if c.CloneTemplate != genericTemplate {
		return nil
	}

	var missing []string
	for _, option := range []struct {
		name  string
		value *uint
	}{
		{"vcpus", c.VCPUs},
		{"vm_memory_gb", c.VMMemoryGB},
		{"disk_size_gb", c.DiskSizeGB},
	} {
		if option.value == nil {
			missing = append(missing, option.name)
		}
	}

	return missing
}

// bootCommandStep types the boot command. This builder's guest boots the installer CD it
// was given, so on UEFI there is a "Press any key to boot from CD" window to hit, which a
// single press cannot be timed against (CA-432255).
func bootCommandStep(ctx interpolate.Context) *xscommon.StepTypeBootCommand {
	return &xscommon.StepTypeBootCommand{
		Ctx:               ctx,
		SweepUEFICDPrompt: true,
	}
}

// setDownloadOptions names the SDK download options the config sets, so a build that
// reads none of them can say which ones it is about to ignore.
func setDownloadOptions(c *commonsteps.ISOConfig) []string {
	var set []string
	for option, value := range map[string]string{
		"iso_checksum":         c.ISOChecksum,
		"iso_target_path":      c.TargetPath,
		"iso_target_extension": c.TargetExtension,
	} {
		if value != "" {
			set = append(set, option)
		}
	}
	sort.Strings(set)

	return set
}

// finalSteps shuts the guest down and takes the install media off, then snapshots before
// converting to a template: XAPI refuses VM.snapshot on a template (CA-431807).
func finalSteps() []multistep.Step {
	return []multistep.Step{
		new(xscommon.StepShutdown),
		&xscommon.StepDetachVdi{
			VdiUuidKey: "iso_vdi_uuid",
		},
		&xscommon.StepDetachVdi{
			VdiUuidKey: "isoname_vdi_uuid",
		},
		&xscommon.StepDetachVdi{
			VdiUuidKey: "tools_vdi_uuid",
		},
		&xscommon.StepDetachVdi{
			VdiUuidKey: "cd_vdi_uuid",
		},
		new(xscommon.StepCreateSnapshot),
		new(xscommon.StepSetVmToTemplate),
		new(xscommon.StepExport),
	}
}

// downloadSteps fetches a URL-sourced ISO into the Packer cache, from where StepUploadVdi
// uploads it to the VM SR. Separate from Run so a test can check that the options the SDK
// resolved reach the step.
func (self *Builder) downloadSteps() []multistep.Step {
	return []multistep.Step{
		&commonsteps.StepDownload{
			Checksum:    self.config.ISOChecksum,
			Description: "ISO",
			ResultKey:   "iso_path",
			Url:         self.config.ISOUrls,
			Extension:   self.config.TargetExtension,
			TargetPath:  self.config.TargetPath,
		},
	}
}

// connectStep is a method so a test can check what StepConnect is handed.
func (self *Builder) connectStep() *communicator.StepConnect {
	return xscommon.ConnectStep(&self.config.SSHConfig)
}

func (self *Builder) Run(ctx context.Context, ui packer.Ui, hook packer.Hook) (packer.Artifact, error) {
	c, err := xscommon.NewXenAPIClient(self.config.HostIp, self.config.Username, self.config.Password, self.config.InsecureEnabled(), self.config.CACertPath)

	if err != nil {
		return nil, err
	}
	defer c.Logout()
	ui.Say("XAPI client session established")

	// Before the steps, not in stepCreateInstance: with iso_url the ISO is downloaded
	// and uploaded before the VM exists, so a source this build cannot use would cost
	// gigabytes first. Everything the source is asked for is asked here, off the one
	// record.
	src := xscommon.ISOTemplateSource(self.config.CloneTemplate)
	sourceVM, sourceRecord, err := xscommon.ResolveCloneSource(c, src)
	if err != nil {
		return nil, err
	}

	// removesVtpm is false: vtpm = false here only declines to attach one.
	if err := xscommon.CloneSourceSupportsFirmware(src, sourceRecord, self.config.Firmware, false); err != nil {
		return nil, err
	}

	// Bytes rather than GB all the way to VDI.create: a template whose disk is not a whole
	// number of GB would otherwise be rounded, and rounding down installs onto a disk
	// smaller than the template asked for.
	diskBytes := 0
	if self.config.DiskSizeGB != nil {
		diskBytes = int(*self.config.DiskSizeGB) * xscommon.BytesPerGB
	} else if diskBytes, err = xscommon.TemplateDiskSize(c, src, sourceVM, sourceRecord); err != nil {
		return nil, err
	}

	state := new(multistep.BasicStateBag)
	state.Put("client", c)
	state.Put("source_vm", sourceVM)
	state.Put("disk_size_bytes", diskBytes)
	state.Put("config", self.config)
	state.Put("commonconfig", self.config.CommonConfig)
	state.Put("hook", hook)
	state.Put("ui", ui)

	httpReqChan := make(chan string, 1)

	steps := []multistep.Step{
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
		&xscommon.StepUploadVdi{
			VdiNameFunc: func() string {
				if len(self.config.ISOUrls) > 0 {
					return path.Base(self.config.ISOUrls[0])
				}
				return ""
			},
			ImagePathFunc: func() string {
				if isoPath, ok := state.GetOk("iso_path"); ok {
					return isoPath.(string)
				}
				return ""
			},
			VdiUuidKey: "iso_vdi_uuid",
		},
		&xscommon.StepFindVdi{
			VdiName:    self.config.GuestToolsISO,
			VdiUuidKey: "tools_vdi_uuid",
		},
		&xscommon.StepFindVdi{
			VdiName:    self.config.ISOName,
			SRName:     self.config.ISOSR,
			VdiUuidKey: "isoname_vdi_uuid",
		},
		new(stepCreateInstance),
		&xscommon.StepAttachvTPM{
			EnablevTPM: self.config.VTPMEnabled,
		},
		&xscommon.StepAttachvGPU{
			VGPUName: self.config.VGPUProfile,
		},
		&xscommon.StepAttachVdi{
			VdiUuidKey: "iso_vdi_uuid",
			VdiType:    xenapi.VbdTypeCD,
		},
		&xscommon.StepAttachVdi{
			VdiUuidKey: "isoname_vdi_uuid",
			VdiType:    xenapi.VbdTypeCD,
		},
		&xscommon.StepAttachVdi{
			VdiUuidKey: "cd_vdi_uuid",
			VdiType:    xenapi.VbdTypeCD,
		},
		&xscommon.StepAttachVdi{
			VdiUuidKey: "tools_vdi_uuid",
			VdiType:    xenapi.VbdTypeCD,
		},
		new(xscommon.StepStartVmPaused),
		new(xscommon.StepBootWait),
		bootCommandStep(*self.config.GetInterpContext()),
		xscommon.InstallWaitStep(self.config.Comm.Type, httpReqChan, self.config.InstallTimeout),
		self.connectStep(),
		new(commonsteps.StepProvision),
	}
	steps = append(steps, finalSteps()...)

	// Ahead of downloadSteps: refusing the output directory is worth nothing behind a
	// multi-GB ISO download.
	preSteps := []multistep.Step{
		&xscommon.StepPrepareOutputDir{
			Force: self.config.PackerForce,
			Path:  self.config.OutputDir,
		},
	}
	if self.config.ISOName == "" {
		preSteps = append(preSteps, self.downloadSteps()...)
	}
	steps = append(preSteps, steps...)

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
