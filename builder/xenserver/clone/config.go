// Copyright (c) 2026. Citrix Systems, Inc.

//go:generate packer-sdc struct-markdown
//go:generate packer-sdc mapstructure-to-hcl2 -type CloneConfig
package clone

import (
	"github.com/hashicorp/packer-plugin-sdk/common"

	xscommon "github.com/xenserver/packer-plugin-xenserver/builder/xenserver/common"
)

// CloneConfig is the xenserver-vm-clone builder's configuration: the options
// shared with the iso builder, plus `clone_template`, `clone_vm` and `vtpm`.
// `clone_template` and `vtpm` have namesakes on the iso builder but do not mean
// the same thing here: `clone_template` is one of a pair with `clone_vm` rather
// than required on its own, and `vtpm` is a `*bool`, so unset, `true` and
// `false` are three distinct requests.
//
// The install-media options (`iso_url`, `iso_name`, the checksum
// fields, `guest_tools_iso`) are absent on purpose — this builder implements
// none of them, so naming one has to be a validation error rather than a silent
// no-op. `iso_sr` is the exception: it is one of the shared options, so it
// decodes here, but only the iso builder's `iso_name` lookup reads it.
type CloneConfig struct {
	common.PackerConfig   `mapstructure:",squash"`
	xscommon.CommonConfig `mapstructure:",squash"`

	// The name-label of the source template to clone. Exactly one of
	// `clone_template` or `clone_vm` must be set, and the name must resolve to
	// exactly one template on the pool. A snapshot does not count as one: XAPI
	// stores it as a template, but `clone_vm` is the option that takes it.
	CloneTemplate string `mapstructure:"clone_template"`

	// The name-label of the source VM to clone. Exactly one of `clone_template`
	// or `clone_vm` must be set, and the name must resolve to exactly one VM on
	// the pool. A snapshot counts as one.
	//
	// The VM must be halted: `VM.clone` and `VM.copy` take a halted VM only and
	// raise `VM_BAD_POWER_STATE` on anything else, suspended included -- a
	// suspended source is accepted solely on a pool whose `other_config` sets
	// `allow_clone_suspended_vm`, and there the clone arrives suspended, which
	// the build cannot start. To build from a VM that has to keep running,
	// snapshot it and name the snapshot here instead -- a snapshot clones while
	// the VM keeps running, at the cost of an image only crash-consistent as of
	// the snapshot.
	//
	// A snapshot named here may be a checkpoint, which is suspended rather than
	// halted. That is allowed, because `VM.clone` halts the clone itself -- but
	// not together with a `vm_sr` that makes the build copy rather than clone,
	// which XAPI cannot settle the power state of (`VM_IS_TEMPLATE`).
	CloneVM string `mapstructure:"clone_vm"`

	// Attach a virtual Trusted Platform Module (vTPM) to the VM. `true` requires
	// `firmware = "uefi"`, which must be set explicitly on a clone.
	//
	// That check reads the config, not the clone. A build that never starts the
	// guest -- `communicator = "none"` with no `cd_files` and no `boot_command` --
	// never writes `firmware` to the VM at all, so `true` against a BIOS source
	// leaves the image carrying a vTPM on the source's own boot mode, which
	// XenServer refuses to start: NOT_IMPLEMENTED [Booting BIOS VM with VTPMs
	// attached]. Give such a build a reason to start, or clone a source that
	// already boots UEFI.
	//
	// Omit it to keep whatever the source has, which is why this is a pointer
	// and the iso builder's `vtpm` is a plain bool: `VM.clone` copies the
	// source's vTPM and creates none otherwise, so an unset option and an
	// explicit `false` have to be told apart here. `true` adds a vTPM when the
	// source had none; `false` destroys one the clone inherited, which also
	// destroys the state sealed to it and cannot be undone.
	//
	// `false` also removes `platform:vtpm`, the key the clone copies from the
	// source and from which XenServer would create a fresh vTPM at start. So a
	// vTPM is no longer what stops `firmware = "bios"`, whether the source carries
	// it as an object, as that key, or as both. The other arm of the pre-flight
	// check still stands: a source that has already run under UEFI has its boot
	// mode settled, and `false` does nothing for that.
	VTPM *bool `mapstructure:"vtpm"`
}

// cloneSource is the source this build clones from. Prepare has already rejected
// both set and neither, so an unset clone_vm means clone_template.
func (c CloneConfig) cloneSource() xscommon.CloneSource {
	if c.CloneVM != "" {
		return xscommon.VMSource(c.CloneVM)
	}
	return xscommon.TemplateSource(c.CloneTemplate)
}

// VTPMEnabled reports whether the build asked for a vTPM. Use it rather than
// dereferencing: a nil VTPM means "keep the source's", which is neither true nor
// false. Test VTPM against nil first when that distinction matters.
func (c CloneConfig) VTPMEnabled() bool {
	return c.VTPM != nil && *c.VTPM
}

// removesVTPM reports whether this build takes the source's vTPM off the clone.
// Only an explicit false does; unset keeps whatever the source has. The pre-flight
// boot-mode check needs it to know whether a UEFI source's vTPM will still be
// attached when the VM starts, which is what firmware = "bios" turns on.
func (c CloneConfig) removesVTPM() bool {
	return c.VTPM != nil && !*c.VTPM
}

// bootsTheGuest reports whether this build has to start the cloned VM. With no
// communicator, no cd_files and no boot_command nothing runs inside the guest and
// nothing powers it off again, so the build would wait out install_timeout at the
// source's own desktop (CA-430805); it exports the clone unstarted instead.
func (c CloneConfig) bootsTheGuest() bool {
	return c.Comm.Type != "none" ||
		len(c.CDFiles) > 0 ||
		len(c.BootCommand) > 0
}
