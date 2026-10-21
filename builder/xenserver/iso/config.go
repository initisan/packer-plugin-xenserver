// Copyright (c) 2026. Citrix Systems, Inc.

//go:generate packer-sdc struct-markdown
//go:generate packer-sdc mapstructure-to-hcl2 -type ISOConfig
package iso

import (
	"github.com/hashicorp/packer-plugin-sdk/common"
	commonsteps "github.com/hashicorp/packer-plugin-sdk/multistep/commonsteps"

	xscommon "github.com/xenserver/packer-plugin-xenserver/builder/xenserver/common"
)

// genericTemplate describes no particular guest OS, so there is no sizing on it to
// inherit and the build has to supply its own (CP-314723).
const genericTemplate = "Other install media"

// minWindowsMemoryGB is the documented installation minimum for every supported Windows
// guest, Windows Server included. Below it, Windows Setup does not fail: it stalls, and
// the build waits out install_timeout (CA-431784).
const minWindowsMemoryGB = 2

// ISOConfig is the xenserver-vm-iso builder's configuration: the options shared
// with the clone builder, the SDK's own ISO-download options, plus the ones
// declared here. Of those, only `iso_name` and `guest_tools_iso` are
// install-from-ISO options: `clone_template` and `vtpm` exist on the clone
// builder too, where `vtpm` is a pointer rather than a plain bool and can
// additionally destroy a vTPM the clone inherited.
//
// The download options (`iso_url`, `iso_urls`, `iso_checksum`,
// `iso_target_path`, `iso_target_extension`) come from the SDK rather than
// being hand-rolled here, so the combined `"sha256:<hash>"` checksum form, the
// `file:` and `none` forms and the ordered mirror fallback all behave as they
// do in every other Packer builder. The hand-rolled fields they replace made
// `iso_checksum_type` required and then never applied it.
type ISOConfig struct {
	common.PackerConfig   `mapstructure:",squash"`
	xscommon.CommonConfig `mapstructure:",squash"`
	commonsteps.ISOConfig `mapstructure:",squash"`

	// The name-label of an ISO already present on the ISO SR (`iso_sr`).
	// Mutually exclusive with `iso_url` and `iso_urls`: an install ISO is
	// either already on `iso_sr`, or downloaded from a URL and uploaded to
	// `vm_sr` as a new VDI.
	ISOName string `mapstructure:"iso_name"`

	// Deprecated: XenServer 8 and later ship no guest-tools ISO (Linux tools are
	// a LinuxGuestTools-*.tar.gz tarball, Windows tools an MSI), so there is
	// nothing to attach. Retained only for XCP-ng and legacy pools; slated for
	// removal. Empty means no tools VDI is looked up or attached.
	GuestToolsISO string `mapstructure:"guest_tools_iso"`

	// The name-label of the XenServer template to clone as the VM's starting
	// point. Required: naming the template that matches the guest OS lets
	// XenServer optimize the virtual hardware, and it decides enough about the
	// VM to be worth saying out loud. Name "Other install media" to build on
	// the generic template.
	//
	// It must name a template. A VM or a snapshot sharing the name label is not a
	// clash and is never cloned here: this builder installs onto a fresh disk, and
	// `xenserver-vm-clone` is the builder that takes a VM or a snapshot.
	//
	// The VM takes the template's `vcpus`, `cores_per_socket`, `vm_memory_gb`
	// and `disk_size_gb` unless the build sets them, so a template sized for
	// the guest OS needs no sizing options at all. None of the four has a
	// default of its own: "Other install media" carries no sizing to take, so
	// a build naming it must set `vcpus`, `vm_memory_gb` and `disk_size_gb`
	// itself or fail validation.
	//
	// The first three are inherited by `VM.clone` copying them. `disk_size_gb`
	// is read off the template instead -- its provisioning disks, or failing
	// those the disk it carries on device 0 -- because the disk installed onto
	// here is a fresh one this builder creates.
	CloneTemplate string `mapstructure:"clone_template" required:"true"`

	// Attach a virtual Trusted Platform Module (vTPM) to the VM. Requires
	// `firmware = "uefi"`, which must be set explicitly: the check runs before
	// any XenAPI session exists, so a UEFI `clone_template` does not satisfy
	// it. Defaults to false.
	//
	// `false` means "add none", not "have none": `VM.clone` copies the vTPM of
	// whatever `clone_template` names, and this builder leaves an inherited one
	// alone. To build without a vTPM, name a `clone_template` that has neither
	// a vTPM nor `platform:vtpm = "true"`: that key is copied by the clone,
	// and XenServer creates a fresh vTPM from it at start.
	VTPMEnabled bool `mapstructure:"vtpm"`
}
