// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"errors"
	"sort"
	"strings"
	"testing"

	"xenapi"
)

// A XenServer Windows 11 template: HVM, uefi, never started, and carrying a vTPM
// in its platform. This is the source the check exists for.
var win11Template = bootModeFacts{
	isHVM:        true,
	firmware:     "uefi",
	declaresVtpm: true,
}

// A VM installed under UEFI and cloned from, rather than a template. The clone
// builder accepts one, and last_boot_CPU_flags is what marks its boot mode as
// settled.
var bootedUEFIVM = bootModeFacts{
	isHVM:     true,
	firmware:  "uefi",
	hasBooted: true,
}

// A VM installed under BIOS. The mirror image of the one above, and the source
// the UEFI-direction arm exists for.
var bootedBIOSVM = bootModeFacts{
	isHVM:     true,
	firmware:  "bios",
	hasBooted: true,
}

// The case the check exists for. firmware is an override, so "bios" wins over the
// template's uefi and the build gets as far as VM.start; XAPI then refuses it for
// the vTPM, having named neither the option to change nor where the vTPM came from.
func TestCloneSourceRejectsFirmware_RefusesBIOSOnAVTPMSource(t *testing.T) {
	err := cloneSourceRejectsFirmware(TemplateSource("Windows 11"), "bios", win11Template, false)

	if err == nil {
		t.Fatal("XenCenter disables BIOS for a uefi source carrying a vTPM, so this must be refused too")
	}
	msg := strings.ToLower(err.Error())
	for _, want := range []string{"windows 11", "bios", "uefi", "vtpm"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got: %s", want, err)
		}
	}
	// The way out this arm has that the settled-source one does not, and it is
	// the clone builder's alone: the iso builder's vtpm = false only declines to
	// attach a vTPM, it does not take away the one the source declares.
	if !strings.Contains(msg, "vtpm = false") || !strings.Contains(msg, "clone builder") {
		t.Errorf("the message must offer vtpm = false and say where it works, got: %s", err)
	}
}

// CA-431619. The build has already said it will take the vTPM off the clone, so
// the source declaring one no longer settles anything: what XAPI refuses to
// start is a BIOS VM with a VTPM attached, and by then there is none.
func TestCloneSourceRejectsFirmware_AllowsBIOSOnAVTPMSourceTheBuildStrips(t *testing.T) {
	if err := cloneSourceRejectsFirmware(TemplateSource("Windows 11"), "bios", win11Template, true); err != nil {
		t.Errorf("vtpm = false removes the vTPM and platform:vtpm before VM.start: %s", err)
	}
}

// Removing the vTPM does nothing for a source whose boot mode is settled: the
// disk was laid out for UEFI and a BIOS guest still has nothing to boot.
func TestCloneSourceRejectsFirmware_StillRefusesASettledUEFISourceThatLosesItsVTPM(t *testing.T) {
	settledWithVtpm := bootModeFacts{isHVM: true, firmware: "uefi", hasBooted: true, declaresVtpm: true}

	err := cloneSourceRejectsFirmware(TemplateSource("win11-golden"), "bios", settledWithVtpm, true)

	if err == nil {
		t.Fatal("the settled boot mode is a separate arm, and vtpm = false does not answer it")
	}
	if !strings.Contains(err.Error(), "already settled") {
		t.Errorf("the refusal should be the settled-source one, got: %s", err)
	}
}

// The other arm, and both builders reach it: last_boot_CPU_flags survives
// VM.clone, VM.copy and import, so an ordinary golden template made from an
// installed VM carries it.
func TestCloneSourceRejectsFirmware_RefusesBIOSOnASettledUEFISource(t *testing.T) {
	err := cloneSourceRejectsFirmware(TemplateSource("win11-golden"), "bios", bootedUEFIVM, false)

	if err == nil {
		t.Fatal("a source that has already run under UEFI cannot be re-pointed at BIOS")
	}
	msg := strings.ToLower(err.Error())
	for _, want := range []string{"win11-golden", "bios", "uefi", "already settled"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got: %s", want, err)
		}
	}
	// The two refusals have different ways out, so they must not collapse into
	// one message: this source has no vTPM to talk about.
	if strings.Contains(msg, "vtpm") {
		t.Errorf("the settled-source message is not about the vTPM, got: %s", err)
	}
	// last_boot_CPU_flags is not what "has run" sounds like, and a user given no
	// field name cannot check the claim against their own source.
	if !strings.Contains(msg, "last_boot_cpu_flags") {
		t.Errorf("the message must name the field it read, got: %s", err)
	}
}

// BootModesControl.cs:231-234, the arm on the other side of the same control: a
// source that has run under BIOS has no ESP, so UEFI firmware finds nothing to
// boot. Left out of the first cut of this check, which only looked at "bios".
func TestCloneSourceRejectsFirmware_RefusesUEFIOnASettledBIOSSource(t *testing.T) {
	err := cloneSourceRejectsFirmware(TemplateSource("ubuntu-golden"), "uefi", bootedBIOSVM, false)

	if err == nil {
		t.Fatal("a source that has already run under BIOS cannot be re-pointed at UEFI")
	}
	msg := strings.ToLower(err.Error())
	for _, want := range []string{"ubuntu-golden", "bios", "uefi", "already settled"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error should mention %q, got: %s", want, err)
		}
	}
}

// The golden-image workflow produces both at once: install Windows 11 from the
// built-in template, boot it, sysprep, convert to a template. VM.clone copies
// platform and last_boot_CPU_flags alike (xapi_vm_clone.ml:382, :389) and
// set_is_a_template touches neither. Which message wins is a choice, so it is
// pinned rather than left to the order of the switch.
func TestCloneSourceRejectsFirmware_ReportsTheSettledBootModeFirst(t *testing.T) {
	src := bootModeFacts{isHVM: true, firmware: "uefi", hasBooted: true, declaresVtpm: true}

	err := cloneSourceRejectsFirmware(TemplateSource("win11-sysprepped"), "bios", src, false)
	if err == nil {
		t.Fatal("a source that is both settled and declares a vTPM must still be refused")
	}
	// Either message would be true, and both name "remove firmware" first, which
	// is the remedy that works for this source. What must not happen is the two
	// swapping silently, since only one of them is reached by a reader.
	if !strings.Contains(strings.ToLower(err.Error()), "already settled") {
		t.Errorf("the settled boot mode is the blocker that survives every remedy, got: %s", err)
	}
}

// Every combination XenCenter leaves the requested option enabled for. Each of
// the last five holds an arm true and defeats the guard in front of it, so the
// guard cannot be dropped without one of them failing.
func TestCloneSourceRejectsFirmware_AllowsWhatXenCenterAllows(t *testing.T) {
	cases := map[string]struct {
		firmware string
		src      bootModeFacts
	}{
		"a PV source, which has no firmware to contradict": {
			firmware: "bios", src: bootModeFacts{isHVM: false, firmware: "uefi"}},
		"an HVM source already set to bios": {
			firmware: "bios", src: bootModeFacts{isHVM: true, firmware: "bios"}},
		// "Other install media", and every template predating the key.
		"an HVM source with no firmware at all": {
			firmware: "bios", src: bootModeFacts{isHVM: true}},
		// BootModesControl.cs:212 -- neither arm holds, so the radio button stays
		// enabled and the user is free to install this one under BIOS.
		"a uefi source that is unsettled and has no vTPM": {
			firmware: "bios", src: bootModeFacts{isHVM: true, firmware: "uefi"}},
		// The arms only bite in the direction the source cannot go. Asking a
		// settled BIOS source for bios, or a settled UEFI one for uefi, is asking
		// for what it already does.
		"bios on a settled bios source": {
			firmware: "bios", src: bootedBIOSVM},
		"uefi on a settled uefi source": {
			firmware: "uefi", src: bootedUEFIVM},
		// Page_InstallationMedia.cs:155: the control is not shown for a PV source,
		// however settled it is.
		"bios on a settled PV source": {
			firmware: "bios", src: bootModeFacts{hasBooted: true}},
		"uefi on a settled PV source": {
			firmware: "uefi", src: bootModeFacts{hasBooted: true}},
		// The vTPM arm lives inside the uefi-source branch, because a source that
		// boots BIOS cannot have had a working vTPM to begin with.
		"bios on a bios source declaring a vTPM": {
			firmware: "bios", src: bootModeFacts{isHVM: true, firmware: "bios", declaresVtpm: true}},
		"bios on a PV source declaring a vTPM": {
			firmware: "bios", src: bootModeFacts{declaresVtpm: true}},
		// HVM_boot_params is still readable on a PV source; it is simply not what
		// boots it. Neither key may be acted on once isHVM is false.
		"bios on a PV source whose stale params say uefi and vtpm": {
			firmware: "bios", src: bootModeFacts{firmware: "uefi", declaresVtpm: true}},
		// Nothing stops a vTPM source being asked for the uefi it already boots.
		"uefi on a uefi source declaring a vTPM": {
			firmware: "uefi", src: win11Template},
		// The ordinary UEFI iso build: "Other install media", never started. The
		// hasBooted arm is the only thing standing between it and a refusal, and it
		// is the one arm no other case here holds false.
		"uefi on an unsettled HVM source with no firmware at all": {
			firmware: "uefi", src: bootModeFacts{isHVM: true}},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if err := cloneSourceRejectsFirmware(TemplateSource("a source"), tc.firmware, tc.src, false); err != nil {
				t.Errorf("unexpected error: %s", err)
			}
		})
	}
}

// The behaviour this check must not undo: an unset firmware inherits the source's
// own, which is what a Windows 11 build is meant to do.
func TestCloneSourceRejectsFirmware_LeavesAnUnsetFirmwareAlone(t *testing.T) {
	for name, src := range map[string]bootModeFacts{
		"a vTPM template":       win11Template,
		"a settled uefi source": bootedUEFIVM,
		"a settled bios source": bootedBIOSVM,
	} {
		t.Run(name, func(t *testing.T) {
			if err := cloneSourceRejectsFirmware(TemplateSource("a source"), "", src, false); err != nil {
				t.Errorf("unexpected error: %s", err)
			}
		})
	}
}

// Prepare lower-cases the build's firmware and refuses anything but bios/uefi
// (common_config.go), so only those two are an override here. Anything else must
// fall through untouched rather than be blamed on the source.
func TestCloneSourceRejectsFirmware_LeavesValuesXAPIWillRejectAlone(t *testing.T) {
	for _, firmware := range []string{"BIOS", "Bios", "UEFI", "legacy"} {
		t.Run(firmware, func(t *testing.T) {
			if err := cloneSourceRejectsFirmware(TemplateSource("Windows 11"), firmware, win11Template, false); err != nil {
				t.Errorf("%q is not bios or uefi, so it is not an override to refuse: %s", firmware, err)
			}
			if err := cloneSourceRejectsFirmware(TemplateSource("ubuntu-golden"), firmware, bootedBIOSVM, false); err != nil {
				t.Errorf("%q is not bios or uefi, so it is not an override to refuse: %s", firmware, err)
			}
		})
	}
}

// The source's own values are read the way XenCenter reads them (VM.cs:1893,
// BootModesControl.cs:118-120): a source spelling either in capitals is still the
// source it is, and reading it any other way would quietly turn the refusal off.
func TestBootModeFactsFromRecord_ReadsTheSourceTheWayXenCenterDoes(t *testing.T) {
	cases := map[string]struct {
		record xenapi.VMRecord
		want   bootModeFacts
	}{
		"a Windows 11 template": {
			record: xenapi.VMRecord{
				HVMBootPolicy: "BIOS order",
				HVMBootParams: map[string]string{"firmware": "uefi", "order": "cdn"},
				IsATemplate:   true,
				Platform:      map[string]string{"vtpm": "true", "secureboot": "true"},
			},
			want: bootModeFacts{isHVM: true, firmware: "uefi", declaresVtpm: true},
		},
		"firmware and vtpm in capitals": {
			record: xenapi.VMRecord{
				HVMBootPolicy: "BIOS order",
				HVMBootParams: map[string]string{"firmware": " UEFI "},
				IsATemplate:   true,
				Platform:      map[string]string{"vtpm": "TRUE"},
			},
			want: bootModeFacts{isHVM: true, firmware: "uefi", declaresVtpm: true},
		},
		"a VM that has been started": {
			record: xenapi.VMRecord{
				HVMBootPolicy:    "BIOS order",
				HVMBootParams:    map[string]string{"firmware": "uefi"},
				LastBootCPUFlags: map[string]string{"vendor": "GenuineIntel", "features": "..."},
			},
			want: bootModeFacts{isHVM: true, firmware: "uefi", hasBooted: true},
		},
		// XenCenter reads platform:vtpm only on a template, because the one page
		// that hands it a VM hides the BIOS option anyway (BootOptionsEditPage.cs:57).
		// clone_vm takes a VM, so the key is read wherever it appears.
		"a VM declaring a vTPM is read like a template that does": {
			record: xenapi.VMRecord{
				HVMBootPolicy: "BIOS order",
				HVMBootParams: map[string]string{"firmware": "uefi"},
				Platform:      map[string]string{"vtpm": "true"},
			},
			want: bootModeFacts{isHVM: true, firmware: "uefi", declaresVtpm: true},
		},
		// XAPI's own Vm_platform.is_true takes "1" as well, so the key is read
		// that way here rather than compared against "true".
		"platform saying 1": {
			record: xenapi.VMRecord{
				HVMBootPolicy: "BIOS order",
				HVMBootParams: map[string]string{"firmware": "uefi"},
				IsATemplate:   true,
				Platform:      map[string]string{"vtpm": "1"},
			},
			want: bootModeFacts{isHVM: true, firmware: "uefi", declaresVtpm: true},
		},
		"platform saying anything but true": {
			record: xenapi.VMRecord{
				HVMBootPolicy: "BIOS order",
				HVMBootParams: map[string]string{"firmware": "uefi"},
				IsATemplate:   true,
				Platform:      map[string]string{"vtpm": "false"},
			},
			want: bootModeFacts{isHVM: true, firmware: "uefi"},
		},
		// "Other install media": HVM, no firmware key, nothing else set.
		"a template carrying no firmware at all": {
			record: xenapi.VMRecord{HVMBootPolicy: "BIOS order", IsATemplate: true},
			want:   bootModeFacts{isHVM: true},
		},
		"a PV template": {
			record: xenapi.VMRecord{PVBootloader: "pygrub", IsATemplate: true},
			want:   bootModeFacts{},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := bootModeFactsFromRecord(tc.record); got != tc.want {
				t.Errorf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

// CA-431860. All three are refused by XAPI as a bare VM_BAD_POWER_STATE, which names
// neither the option nor the way out: a suspended non-snapshot is only clonable on a
// pool that opts in with allow_clone_suspended_vm (xapi_vm_lifecycle.ml:74-85).
func TestEnsureCloneSourceIsHalted_RefusesEveryStateButHalted(t *testing.T) {
	for _, state := range []xenapi.VMPowerState{
		xenapi.VMPowerStateRunning,
		xenapi.VMPowerStatePaused,
		xenapi.VMPowerStateSuspended,
	} {
		t.Run(string(state), func(t *testing.T) {
			err := EnsureCloneSourceIsHalted(VMSource("build-src"), xenapi.VMRecord{PowerState: state}, false)

			if err == nil {
				t.Fatalf("a %s source cannot be cloned", state)
			}
			for _, want := range []string{"clone_vm 'build-src'", string(state), "snapshot"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should mention %q, got: %s", want, err)
				}
			}
		})
	}
}

func TestEnsureCloneSourceIsHalted_AllowsAHaltedVM(t *testing.T) {
	err := EnsureCloneSourceIsHalted(VMSource("build-src"),
		xenapi.VMRecord{PowerState: xenapi.VMPowerStateHalted}, false)
	if err != nil {
		t.Errorf("halted is what both calls require: %s", err)
	}
}

// A template is never started, so the check has nothing to say about it. Its record
// still carries whatever power state XAPI left there.
func TestEnsureCloneSourceIsHalted_SkipsATemplate(t *testing.T) {
	err := EnsureCloneSourceIsHalted(TemplateSource("Windows 11"),
		xenapi.VMRecord{IsATemplate: true, PowerState: xenapi.VMPowerStateRunning}, true)
	if err != nil {
		t.Errorf("a template is not subject to the power-state check: %s", err)
	}
}

// A checkpoint of a running VM is Suspended, and VM.clone hard-shuts its clone down
// itself (xapi_vm.ml:756-760), so the build gets the halted VM it needs anyway.
func TestEnsureCloneSourceIsHalted_SkipsASuspendedCheckpoint(t *testing.T) {
	err := EnsureCloneSourceIsHalted(VMSource("nightly-checkpoint"),
		xenapi.VMRecord{IsATemplate: true, IsASnapshot: true, PowerState: xenapi.VMPowerStateSuspended}, false)
	if err != nil {
		t.Errorf("a snapshot is not subject to the power-state check: %s", err)
	}
}

// CA-431860. VM.copy settles the same power state through the API dispatcher rather
// than in-process (xapi_vm.ml:826-830), and the copy inherits is_a_template from the
// snapshot, so XAPI refuses its own hard_shutdown with VM_IS_TEMPLATE once the disks
// have been copied. Refuse before the copy, not after paying for it.
func TestEnsureCloneSourceIsHalted_RefusesASuspendedCheckpointWhenTheBuildMayCopy(t *testing.T) {
	err := EnsureCloneSourceIsHalted(VMSource("nightly-checkpoint"),
		xenapi.VMRecord{IsATemplate: true, IsASnapshot: true, PowerState: xenapi.VMPowerStateSuspended}, true)
	if err == nil {
		t.Fatal("VM.copy cannot finish on a suspended snapshot")
	}
	for _, want := range []string{"clone_vm 'nightly-checkpoint'", "vm_sr", "VM_IS_TEMPLATE"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %s", want, err)
		}
	}
}

// A halted snapshot copies without that step, so vm_sr is no reason to refuse it.
func TestEnsureCloneSourceIsHalted_AllowsAHaltedSnapshotEvenWhenTheBuildMayCopy(t *testing.T) {
	err := EnsureCloneSourceIsHalted(VMSource("nightly"),
		xenapi.VMRecord{IsATemplate: true, IsASnapshot: true, PowerState: xenapi.VMPowerStateHalted}, true)
	if err != nil {
		t.Errorf("a halted snapshot needs no hard_shutdown after the copy: %s", err)
	}
}

// CA-431860. XAPI has no snapshot class -- a snapshot is a VM record carrying
// is_a_snapshot and is_a_template both -- so is_a_template alone cannot separate
// the three kinds a name label can resolve to.
func TestCloneSourceMatches_SeparatesTemplatesVMsAndSnapshots(t *testing.T) {
	realTemplate := xenapi.VMRecord{IsATemplate: true}
	realVM := xenapi.VMRecord{}
	snapshot := xenapi.VMRecord{IsATemplate: true, IsASnapshot: true}
	dom0 := xenapi.VMRecord{IsControlDomain: true}

	cases := map[string]struct {
		src    CloneSource
		record xenapi.VMRecord
		want   bool
	}{
		"clone_template takes a template":         {TemplateSource("x"), realTemplate, true},
		"clone_template refuses a VM":             {TemplateSource("x"), realVM, false},
		"clone_template refuses a snapshot":       {TemplateSource("x"), snapshot, false},
		"clone_vm takes a VM":                     {VMSource("x"), realVM, true},
		"clone_vm takes a snapshot":               {VMSource("x"), snapshot, true},
		"clone_vm refuses a template":             {VMSource("x"), realTemplate, false},
		"clone_vm refuses a control domain":       {VMSource("x"), dom0, false},
		"clone_template refuses a control domain": {TemplateSource("x"), dom0, false},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := tc.src.matches(tc.record); got != tc.want {
				t.Errorf("got %t, want %t", got, tc.want)
			}
		})
	}
}

func TestKindOf_NamesASnapshotAsItself(t *testing.T) {
	cases := map[string]xenapi.VMRecord{
		"template":       {IsATemplate: true},
		"VM":             {},
		"snapshot":       {IsATemplate: true, IsASnapshot: true},
		"control domain": {IsControlDomain: true},
	}

	for want, record := range cases {
		if got := kindOf(record); got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

func TestCloneSource_NamesTheOptionTheBuildSet(t *testing.T) {
	if got := VMSource("build-src").String(); got != "clone_vm 'build-src'" {
		t.Errorf("got %q", got)
	}
	if got := TemplateSource("Windows 11").String(); got != "clone_template 'Windows 11'" {
		t.Errorf("got %q", got)
	}
	if got := VMSource("x").alternative; got != "clone_template" {
		t.Errorf("the other option for clone_vm is clone_template, got %q", got)
	}
	if got := TemplateSource("x").alternative; got != "clone_vm" {
		t.Errorf("the other option for clone_template is clone_vm, got %q", got)
	}
}

// resolverFake answers the two calls ResolveCloneSource makes. records is keyed by
// reference, and its keys are what the name-label lookup returns.
func resolverFake(t *testing.T, records map[string]map[string]interface{}) *fakeXAPI {
	t.Helper()

	refs := make([]interface{}, 0, len(records))
	for ref := range records {
		refs = append(refs, ref)
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].(string) < refs[j].(string) })

	return newFakeXAPI(t, map[string]interface{}{
		"VM.get_by_name_label": refs,
		"VM.get_record": func(args []interface{}) interface{} {
			return records[args[0].(string)]
		},
	})
}

// CA-431860. The name-label a build gives is shared by every kind of VM record, so
// the kind is what picks the one object the option meant.
func TestResolveCloneSource_PicksTheObjectOfTheOptionsOwnKind(t *testing.T) {
	records := map[string]map[string]interface{}{
		"OpaqueRef:the-template": {"is_a_template": true, "power_state": "Halted"},
		"OpaqueRef:the-vm":       {"power_state": "Running"},
		"OpaqueRef:the-snapshot": {"is_a_template": true, "is_a_snapshot": true, "power_state": "Suspended"},
	}

	isoPath := ISOTemplateSource("golden")

	for name, src := range map[string]CloneSource{
		"clone_template":                 TemplateSource("golden"),
		"clone_template on the iso path": isoPath,
	} {
		t.Run(name, func(t *testing.T) {
			ref, record, err := ResolveCloneSource(resolverFake(t, records).conn(), src)
			if err != nil {
				t.Fatalf("unexpected error: %s", err)
			}
			if ref != "OpaqueRef:the-template" {
				t.Errorf("ref = %q, want the template", ref)
			}
			if !record.IsATemplate || record.IsASnapshot {
				t.Errorf("record is not the template's: %+v", record)
			}
		})
	}

	t.Run("clone_vm prefers the VM", func(t *testing.T) {
		vmOnly := map[string]map[string]interface{}{
			"OpaqueRef:the-template": records["OpaqueRef:the-template"],
			"OpaqueRef:the-vm":       records["OpaqueRef:the-vm"],
		}
		ref, record, err := ResolveCloneSource(resolverFake(t, vmOnly).conn(), VMSource("golden"))
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if ref != "OpaqueRef:the-vm" {
			t.Errorf("ref = %q, want the VM", ref)
		}
		if record.PowerState != xenapi.VMPowerStateRunning {
			t.Errorf("the record must be the resolved object's, got power state %q", record.PowerState)
		}
	})

	t.Run("clone_vm takes the snapshot", func(t *testing.T) {
		snapshotOnly := map[string]map[string]interface{}{
			"OpaqueRef:the-template": records["OpaqueRef:the-template"],
			"OpaqueRef:the-snapshot": records["OpaqueRef:the-snapshot"],
		}
		ref, _, err := ResolveCloneSource(resolverFake(t, snapshotOnly).conn(), VMSource("golden"))
		if err != nil {
			t.Fatalf("unexpected error: %s", err)
		}
		if ref != "OpaqueRef:the-snapshot" {
			t.Errorf("ref = %q, want the snapshot", ref)
		}
	})
}

// The boot-mode rule reads the record this returns, so a record that is not the
// resolved object's turns the rule off without saying so.
func TestResolveCloneSource_ReturnsTheResolvedObjectsOwnRecord(t *testing.T) {
	fake := resolverFake(t, map[string]map[string]interface{}{
		"OpaqueRef:win11": {
			"is_a_template":   true,
			"HVM_boot_policy": "BIOS order",
			"HVM_boot_params": map[string]interface{}{"firmware": "uefi"},
			"platform":        map[string]interface{}{"vtpm": "true"},
		},
	})

	_, record, err := ResolveCloneSource(fake.conn(), TemplateSource("Windows 11"))
	if err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	facts := bootModeFactsFromRecord(record)
	if !facts.isHVM || facts.firmware != firmwareUEFI || !facts.declaresVtpm {
		t.Errorf("the record did not carry the source's boot mode: %+v", facts)
	}
}

func TestResolveCloneSource_AsksForTheNameTheOptionNamed(t *testing.T) {
	fake := resolverFake(t, map[string]map[string]interface{}{
		"OpaqueRef:x": {"is_a_template": true},
	})

	if _, _, err := ResolveCloneSource(fake.conn(), TemplateSource("Windows 11")); err != nil {
		t.Fatalf("unexpected error: %s", err)
	}

	args := fake.argsOf(t, "VM.get_by_name_label")
	if len(args) != 1 || args[0] != "Windows 11" {
		t.Errorf("looked up %v, want the name clone_template gave", args)
	}
}

func TestResolveCloneSource_RefusesWhatItCannotResolveToOneObject(t *testing.T) {
	template := map[string]interface{}{"is_a_template": true}

	cases := map[string]struct {
		src     CloneSource
		records map[string]map[string]interface{}
		want    []string
	}{
		"nothing of that name": {
			TemplateSource("missing"), nil,
			[]string{"could not find a template named 'missing'"},
		},
		"two of the same kind": {
			TemplateSource("golden"),
			map[string]map[string]interface{}{"OpaqueRef:a": template, "OpaqueRef:b": template},
			[]string{"found 2 templates named 'golden'", "unique among templates"},
		},
		"only the other kind": {
			TemplateSource("build-src"),
			map[string]map[string]interface{}{"OpaqueRef:vm": {}},
			[]string{"clone_template 'build-src'", "names a VM, not a template", "Use clone_vm instead"},
		},
		"a snapshot named in clone_template": {
			TemplateSource("nightly"),
			map[string]map[string]interface{}{
				"OpaqueRef:snap": {"is_a_template": true, "is_a_snapshot": true},
			},
			[]string{"names a snapshot, not a template", "Use clone_vm instead"},
		},
		"a control domain named in clone_vm": {
			VMSource("Control domain on host: xrtuk-01"),
			map[string]map[string]interface{}{"OpaqueRef:dom0": {"is_control_domain": true}},
			[]string{"names a control domain", "no source here to build from"},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, _, err := ResolveCloneSource(resolverFake(t, tc.records).conn(), tc.src)
			if err == nil {
				t.Fatal("expected an error naming what went wrong")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error should mention %q, got: %s", want, err)
				}
			}
		})
	}
}

// A lookup that fails has to stop the build: treating it as "not found" would send
// the user after a name that is there.
func TestResolveCloneSource_ALookupFailureIsFatal(t *testing.T) {
	fake := newFakeXAPI(t, map[string]interface{}{
		"VM.get_by_name_label": errors.New("SESSION_INVALID"),
	})

	if _, _, err := ResolveCloneSource(fake.conn(), TemplateSource("golden")); err == nil {
		t.Fatal("expected the lookup failure to be reported")
	} else if !strings.Contains(err.Error(), "SESSION_INVALID") {
		t.Errorf("error should carry XAPI's own: %s", err)
	}
}

func TestResolveCloneSource_ARecordFailureIsFatal(t *testing.T) {
	fake := newFakeXAPI(t, map[string]interface{}{
		"VM.get_by_name_label": []interface{}{"OpaqueRef:x"},
		"VM.get_record":        errors.New("HANDLE_INVALID"),
	})

	if _, _, err := ResolveCloneSource(fake.conn(), TemplateSource("golden")); err == nil {
		t.Fatal("expected the record read to be reported")
	} else if !strings.Contains(err.Error(), "HANDLE_INVALID") {
		t.Errorf("error should carry XAPI's own: %s", err)
	}
}
