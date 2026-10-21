// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"
	"strings"

	"xenapi"
)

// CloneSource is the source a build clones from: the option that named it, so a
// message can quote the one the user set, and the kind of object that option
// means. In XAPI every kind is a VM record, so the kind is the only thing that
// makes a shared name-label unambiguous (CA-431860).
type CloneSource struct {
	Option     string
	Kind       string
	Name       string
	IsTemplate bool

	// alternative is what to reach for when the name resolved to another kind. It
	// is not always Option's sibling: the iso builder has clone_template and no
	// clone_vm, so a VM named there means a different builder rather than a
	// different option -- which is why ISOTemplateSource exists rather than this
	// being set from another package.
	alternative string
}

const TemplateLookupHint = "Run `xe template-list params=name-label` on the pool, or " +
	"look at the template list in XenCenter's New VM wizard, to see what is available. " +
	"The name must match exactly one template."

func TemplateSource(name string) CloneSource {
	return CloneSource{Option: "clone_template", Kind: "template", Name: name, IsTemplate: true,
		alternative: "clone_vm"}
}

// ISOTemplateSource is the iso builder's clone_template. That builder installs onto a
// fresh disk and has no clone_vm, so a name that resolved to a VM is the other builder's
// to take rather than another option on this one.
func ISOTemplateSource(name string) CloneSource {
	s := TemplateSource(name)
	s.alternative = "the xenserver-vm-clone builder's clone_vm"
	return s
}

func VMSource(name string) CloneSource {
	return CloneSource{Option: "clone_vm", Kind: "VM", Name: name, alternative: "clone_template"}
}

func (s CloneSource) String() string {
	return fmt.Sprintf("%s '%s'", s.Option, s.Name)
}

// matches reports whether a record is of this source's kind. XAPI has no snapshot
// class -- a snapshot is a VM record carrying is_a_snapshot and is_a_template both
// (xapi_vm_clone.ml:341) -- so is_a_template alone cannot tell the three apart, the
// way xe needs vm-list, template-list and snapshot-list to (cli_operations.ml:1006-1018).
// A snapshot counts as a VM: it is what clone_vm points at to build from a VM that
// has to keep running. A control domain does not: XAPI refuses clone, snapshot and
// hard_shutdown alike on one (xapi_vm_lifecycle.ml:585-602).
func (s CloneSource) matches(record xenapi.VMRecord) bool {
	if s.IsTemplate {
		return record.IsATemplate && !record.IsASnapshot
	}
	return (!record.IsATemplate || record.IsASnapshot) && !record.IsControlDomain
}

func kindOf(record xenapi.VMRecord) string {
	switch {
	case record.IsControlDomain:
		return "control domain"
	case record.IsASnapshot:
		return "snapshot"
	case record.IsATemplate:
		return "template"
	default:
		return "VM"
	}
}

// ResolveCloneSource resolves the source's name-label to exactly one object of
// its own kind. Cloning a golden template and keeping the name is the ordinary
// way to arrive at a source VM, so a template and a VM sharing a name-label is
// not exotic, and before the two options were split it was unbuildable.
func ResolveCloneSource(c *Connection, src CloneSource) (xenapi.VMRef, xenapi.VMRecord, error) {
	var none xenapi.VMRecord

	vms, err := xenapi.VM.GetByNameLabel(c.GetSession(), src.Name)
	if err != nil {
		return "", none, fmt.Errorf("could not look up the %s name-label '%s': %w", src.Kind, src.Name, err)
	}

	var matches []xenapi.VMRef
	var records []xenapi.VMRecord
	var wrongKind []xenapi.VMRecord
	for _, vm := range vms {
		record, err := xenapi.VM.GetRecord(c.GetSession(), vm)
		if err != nil {
			return "", none, fmt.Errorf("could not read the record of a VM named '%s': %w", src.Name, err)
		}
		if src.matches(record) {
			matches = append(matches, vm)
			records = append(records, record)
		} else {
			wrongKind = append(wrongKind, record)
		}
	}

	switch {
	case len(matches) == 1:
		return matches[0], records[0], nil

	case len(matches) > 1:
		return "", none, fmt.Errorf("found %d %ss named '%s'. %s needs a name that is unique among %ss",
			len(matches), src.Kind, src.Name, src.Option, src.Kind)

	case len(wrongKind) > 0:
		if wrongKind[0].IsControlDomain {
			return "", none, fmt.Errorf("%s names a control domain, which XAPI will neither clone nor "+
				"snapshot nor shut down (xapi_vm_lifecycle.ml:585-602). There is no source here to build from",
				src)
		}
		return "", none, fmt.Errorf("%s names a %s, not a %s. Use %s instead",
			src, kindOf(wrongKind[0]), src.Kind, src.alternative)

	default:
		return "", none, fmt.Errorf("could not find a %s named '%s'", src.Kind, src.Name)
	}
}

// EnsureCloneSourceIsHalted refuses a source this build cannot make a halted VM out
// of, because XAPI raises a bare VM_BAD_POWER_STATE that names neither the option at
// fault nor the way round it.
//
// A template is halted by construction. A suspended snapshot -- a checkpoint -- is
// clonable, and VM.clone hard-shuts the new VM down through XAPI's own function
// (xapi_vm.ml:756-760) so it arrives halted. The VM.copy that mayCopy selects makes
// that same call through the API dispatcher instead (:826-830), where the copy's
// is_a_template, inherited from the snapshot (xapi_vm_clone.ml:341), fails it with
// VM_IS_TEMPLATE -- after every disk has been copied, and with nothing to undo it.
func EnsureCloneSourceIsHalted(src CloneSource, record xenapi.VMRecord, mayCopy bool) error {
	if src.IsTemplate || record.PowerState == xenapi.VMPowerStateHalted {
		return nil
	}

	if record.IsASnapshot {
		if !mayCopy {
			return nil
		}
		return fmt.Errorf("%s names a suspended snapshot, and vm_sr may send this build through VM.copy, "+
			"which XAPI cannot finish on one: the copy inherits is_a_template from the snapshot, so the "+
			"hard_shutdown XAPI runs to settle its power state is refused with VM_IS_TEMPLATE -- once every "+
			"disk has been copied in full, and with the half-built VM left behind. Leave vm_sr unset to clone "+
			"in place, or name a snapshot of a halted VM, which copies without this step", src)
	}

	return fmt.Errorf("%s is %s, and this build needs a halted source: VM.clone and VM.copy take a halted "+
		"VM only, and raise VM_BAD_POWER_STATE on anything else. A suspended one is accepted solely on a pool "+
		"whose other_config sets allow_clone_suspended_vm, and there the clone arrives suspended, which the "+
		"build cannot start. Shut it down, or snapshot it and name the snapshot in %s instead -- a snapshot "+
		"clones while the VM keeps running, at the cost of an image only crash-consistent as of the snapshot",
		src, record.PowerState, src.Option)
}

// bootModeFacts is the part of a clone source's record the boot-mode rule reads.
//
// The fields are named after the XenCenter accessors they come from, so the port
// below can be read against the original in XenCenter's
// XenModel/XenAPI-Extensions/VM.cs.
type bootModeFacts struct {
	// isHVM is VM.IsHVM(): HVM_boot_policy != "" (VM.cs:643).
	isHVM bool

	// firmware is HVM_boot_params["firmware"], trimmed and lower-cased the way
	// VM.IsDefaultBootModeUefi() does before comparing it (VM.cs:1893). An absent
	// key is how XAPI spells BIOS.
	firmware string

	// hasBooted is !VM.CanChangeBootMode() (VM.cs:1845), which reads
	// last_boot_CPU_flags. XAPI writes it on a VM's first start
	// (xapi_xenops.ml:2486) and never clears it, but it is not only a record of
	// this object running: VM.clone and VM.copy pass it through
	// (xapi_vm_clone.ml:389) and import restores it (import.ml:704), so a
	// template made from a VM that ran carries it too. That is the point rather
	// than a flaw -- the boot mode is settled by whatever laid the disk out, not
	// by which object did the laying.
	hasBooted bool

	// declaresVtpm is BootModesControl.IsVtpmTemplate (BootModesControl.cs:118-120)
	// less its is_a_template conjunct: a source whose platform declares a vTPM.
	// Read the way XAPI reads the key, so platform:vtpm = "1" counts too.
	//
	// The conjunct is dropped deliberately. XenCenter can afford it because the
	// one page that hands BootModesControl something that is not a template
	// (BootOptionsEditPage.cs:237) sets AllowFirmwareChange = false first
	// (BootOptionsEditPage.cs:57), which hides the BIOS option outright -- so
	// XenCenter never offers a VM a BIOS switch to disable. clone_vm takes a VM
	// as readily as clone_template takes a template, so keeping the conjunct here
	// would not be faithful, it would just be a hole: XAPI materialises a vTPM
	// from platform:vtpm at start whether or not the source was a template
	// (xapi_xenops.ml:416-431).
	declaresVtpm bool
}

// CloneSourceSupportsFirmware refuses a build whose firmware overrides a clone
// source that cannot boot that way.
//
// The rule is XenCenter's, ported rather than invented. The New VM wizard shows
// its boot-mode radio buttons for an HVM source only (Page_InstallationMedia.cs:155)
// and then greys out the one the source cannot take: BIOS when the source already
// boots UEFI and has either been started before or carries a vTPM
// (BootModesControl.cs:210-216), and UEFI when the source does not boot UEFI and
// has been started before (BootModesControl.cs:231-234). Packer has no radio
// button to grey out, so each becomes a build error.
//
// It exists because a build would otherwise find out the slow way. firmware is an
// override, so it wins over the source's own and VM.start accepts the result;
// what follows is a guest that never comes up, and a build that waits for it
// until its timeout expires -- 200 minutes by default.
//
// Not ported: VM.SupportsUefiBoot(), the supports-uefi recommendation
// (VM.cs:1904). GetRecommendationByField returns "" for a source that does not
// carry the restriction (VM.cs:1914-1920), so it would refuse every source that
// simply does not mention the field, the ordinary UEFI install included.
//
// Call it before the build does any work, off the record ResolveCloneSource
// already read. Both builders reach the VM this applies to well into their step
// list: with iso_url the install media is downloaded and uploaded first, and on
// the clone path a -force build has by then removed the previous output directory.
//
// removesVtpm says the build will strip the source's vTPM before the VM starts,
// which is what vtpm = false does on the clone builder (CA-431619) and what the
// iso builder has no way to do. It is the one arm below a build can answer for
// itself: XenCenter has no such checkbox, so its control disables BIOS outright.
func CloneSourceSupportsFirmware(src CloneSource, record xenapi.VMRecord, firmware string, removesVtpm bool) error {
	return cloneSourceRejectsFirmware(src, firmware, bootModeFactsFromRecord(record), removesVtpm)
}

// bootModeFactsFromRecord reads the four fields the rule is written on, each the
// way the XenCenter accessor named on it reads them.
func bootModeFactsFromRecord(record xenapi.VMRecord) bootModeFacts {
	return bootModeFacts{
		isHVM:        record.HVMBootPolicy != "",
		firmware:     strings.ToLower(strings.TrimSpace(record.HVMBootParams["firmware"])),
		hasBooted:    len(record.LastBootCPUFlags) > 0,
		declaresVtpm: platformDeclaresVTPM(record.Platform),
	}
}

// settledNote explains what "already settled" was read from, because the field is
// not what its name suggests and the user has no other way to find out.
const settledNote = " (What is read is last_boot_CPU_flags. XenServer writes it on a VM's first start and " +
	"never clears it, and VM.clone, VM.copy and import all carry it across -- so a template made from a " +
	"VM that ran counts as settled even though the template itself was never started.)"

// cloneSourceRejectsFirmware is BootModesControl.UpdateControls with the wizard
// taken out: nil where XenCenter leaves the requested option enabled, an error
// where it disables it.
func cloneSourceRejectsFirmware(src CloneSource, firmware string, facts bootModeFacts, removesVtpm bool) error {
	// XAPI matches HVM_boot_params["firmware"] exactly and knows only these two;
	// anything else fails VM.start with INVALID_VALUE, which already names the
	// value at fault. An unset firmware asks for the source's own, which is by
	// definition one it can boot.
	if firmware != firmwareBIOS && firmware != firmwareUEFI {
		return nil
	}

	// Page_InstallationMedia.cs:155 -- the control is shown for an HVM source
	// only, and HVM_boot_params means nothing to a PV guest in any case.
	if !facts.isHVM {
		return nil
	}

	if facts.firmware == firmwareUEFI {
		// BootModesControl.cs:210-216. The source boots UEFI, so UEFI is what it
		// is being asked for and BIOS is the option with arms against it. Either
		// arm on its own disables it; they are separate errors here because the
		// way out of them differs.
		switch {
		case firmware != firmwareBIOS:
			return nil

		case facts.hasBooted:
			return fmt.Errorf("%s boots with UEFI and its boot mode is already settled, so "+
				"firmware = %q cannot work: a source that has run under UEFI carries the boot files a UEFI "+
				"guest loads, and a BIOS guest has no firmware to load them with. The VM would start, find "+
				"nothing to boot, and the build would wait for a guest that never comes up. Remove firmware "+
				"to keep the source's %q, or clone from a source that already boots BIOS.%s",
				src, firmwareBIOS, firmwareUEFI, settledNote)

		// The build has said it will take the vTPM off the clone, both the object
		// and platform:vtpm, before anything starts it. XAPI's refusal is on the
		// VM's own VTPMs at start (xapi_xenops.ml:1300-1305), so by then there is
		// nothing left to refuse.
		case facts.declaresVtpm && !removesVtpm:
			return fmt.Errorf("%s boots with UEFI and declares a vTPM (platform:vtpm = "+
				"true), so firmware = %q cannot work: XenServer implements the vTPM for UEFI guests only, "+
				"and VM.start refuses a BIOS VM that has one (NOT_IMPLEMENTED [Booting BIOS VM with VTPMs "+
				"attached]). Remove firmware to keep the source's %q, or pick a source that boots "+
				"BIOS. On the clone builder, vtpm = false builds without one instead, which destroys "+
				"whatever the inherited vTPM had sealed to it.",
				src, firmwareBIOS, firmwareUEFI)
		}

		return nil
	}

	// BootModesControl.cs:231-234, the else branch of the same control: the source
	// does not boot UEFI, so BIOS is what it is being asked for and UEFI is the
	// option that can be disabled -- by the one arm that survives out here, since
	// a vTPM cannot be attached to a BIOS source in the first place.
	if firmware == firmwareUEFI && facts.hasBooted {
		return fmt.Errorf("%s boots with BIOS and its boot mode is already settled, so "+
			"firmware = %q cannot work: a source that has run under BIOS has no EFI system partition, and "+
			"UEFI firmware has nothing to boot from. The VM would start into the UEFI shell, and the build "+
			"would wait for a guest that never comes up. Remove firmware to keep the source's %q, or clone "+
			"from a source that already boots UEFI.%s",
			src, firmwareUEFI, firmwareBIOS, settledNote)
	}

	return nil
}
