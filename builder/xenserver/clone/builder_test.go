// Copyright (c) 2026. Citrix Systems, Inc.

package clone

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	xscommon "github.com/xenserver/packer-plugin-xenserver/builder/xenserver/common"
	"github.com/xenserver/packer-plugin-xenserver/builder/xenserver/internal/xstest"
)

// clearConnectionEnv unsets the variables the connection options fall back to, so a
// XENSERVER_HOST in a developer's shell cannot satisfy the checks asserted below.
func clearConnectionEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{
		xscommon.EnvHost,
		xscommon.EnvUsername,
		xscommon.EnvPassword,
		xscommon.EnvCACertPath,
		xscommon.EnvInsecure,
	} {
		t.Setenv(key, "")
	}
}

func testConfig() map[string]interface{} {
	return map[string]interface{}{
		"xenserver_host":     "localhost",
		"xenserver_username": "admin",
		"xenserver_password": "admin",
		"clone_template":     "Windows 11",
		"vm_name":            "foo",
		"shutdown_command":   "yes",
		"ssh_username":       "foo",

		// Named on purpose: these tests are not about the communicator, so they must
		// not ride on its default.
		"communicator": "ssh",

		"packer_build_name": "foo",
		"insecure":          true,
	}
}

// The renamed options live in the shared CommonConfig, but each builder has its own
// Prepare, so the renames have to be exercised through this one too.
func TestBuilderPrepare_RenamedKeysCarryTheirValues(t *testing.T) {
	var b Builder
	config := testConfig()
	config["vm_sr"] = "Local storage"
	config["vm_memory_gb"] = 8
	config["disk_size_gb"] = 60
	config["vcpus"] = 4

	_, _, err := b.Prepare(config)
	if err != nil {
		t.Fatalf("should not have error: %s", err)
	}

	if b.config.VMSR != "Local storage" {
		t.Errorf("vm_sr = %q, want \"Local storage\"", b.config.VMSR)
	}
	xstest.WantUint(t, "vm_memory_gb", b.config.VMMemoryGB, 8)
	xstest.WantUint(t, "disk_size_gb", b.config.DiskSizeGB, 60)
	xstest.WantUint(t, "vcpus", b.config.VCPUs, 4)
}

// Unlike the iso builder, the clone builder applies no vCPU, memory or disk default:
// unset means "keep the source template's", and the steps key off zero to decide that.
func TestBuilderPrepare_UnsetSizingKeepsTheTemplatesValues(t *testing.T) {
	var b Builder
	config := testConfig()

	_, _, err := b.Prepare(config)
	if err != nil {
		t.Fatalf("should not have error: %s", err)
	}

	xstest.WantUnset(t, "vcpus", b.config.VCPUs)
	xstest.WantUnset(t, "vm_memory_gb", b.config.VMMemoryGB)
	xstest.WantUnset(t, "disk_size_gb", b.config.DiskSizeGB)
}

func TestBuilderPrepare_RejectsOldAndDroppedKeys(t *testing.T) {
	gone := map[string]interface{}{
		"remote_host":            "localhost",
		"remote_username":        "admin",
		"remote_password":        "admin",
		"sr_name":                "Local storage",
		"vm_memory":              1024,
		"disk_size":              40000,
		"server_cert":            "/etc/ssl/certs/pool.pem",
		"server_cert_file":       "/etc/ssl/certs/pool.pem",
		"skip_cert_verification": true,
		"export_network_names":   []string{"foo"},
		"floppy_files":           []string{"foo"},
		"ip_getter":              "tools",
		"ssh_skip_nat_mapping":   true,
		"ssh_key_path":           "/tmp/id_rsa",
		"convert_to_template":    true,
		"vcpus_max":              4,
		"vcpus_atstartup":        2,
		// Folded into iso_checksum's type prefix; gone from both builders.
		"iso_checksum_type": "md5",
	}

	for key, value := range gone {
		var b Builder
		config := testConfig()
		config[key] = value

		_, _, err := b.Prepare(config)
		if err == nil {
			t.Errorf("%s: must be rejected as unknown", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s: error should name the unknown key, got: %s", key, err)
		}
	}
}

// The whole point of giving each builder its own config struct: an option the clone
// builder does not implement has to fail validation instead of decoding into a field
// nothing reads. Every key here used to be accepted and then silently ignored.
func TestBuilderPrepare_RejectsISOOnlyKeys(t *testing.T) {
	isoOnly := map[string]interface{}{
		"iso_checksum":    "d41d8cd98f00b204e9800998ecf8427e",
		"iso_url":         "http://www.packer.io/foo.iso",
		"iso_urls":        []string{"http://www.packer.io/foo.iso"},
		"iso_name":        "Win11.iso",
		"guest_tools_iso": "xs-tools.iso",
		// These two only describe where a downloaded ISO is put, so on a builder that
		// downloads nothing they are not merely unused -- they are evidence the template
		// was written for the other builder.
		"iso_target_path":      "/tmp/win11.iso",
		"iso_target_extension": "iso",
	}

	for key, value := range isoOnly {
		var b Builder
		config := testConfig()
		config[key] = value

		_, _, err := b.Prepare(config)
		if err == nil {
			t.Errorf("%s: an iso-only option must be rejected by the clone builder", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s: error should name the unknown key, got: %s", key, err)
		}
	}
}

// Every duration option is decoded by the same mapstructure hook, so one malformed value
// per field is enough -- what is under test is that Prepare surfaces the decode error at
// all. It did not always: the error hconfig.Decode returns was discarded, and with it went
// every unknown key and every unparseable duration. Both builders decode separately, so
// the iso builder's copy of this test does not cover this one.
func TestBuilderPrepare_RejectsMalformedDurations(t *testing.T) {
	for _, key := range []string{
		"boot_wait",
		"boot_key_group_interval",
		"install_timeout",
		"winrm_timeout",
		"pause_before_connecting",
	} {
		var b Builder
		config := testConfig()
		config[key] = "5 seconds"

		_, _, err := b.Prepare(config)
		if err == nil {
			t.Errorf("%s: a malformed duration must be rejected", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s: error should name the field, got: %s", key, err)
		}
	}
}

// A communicator the SDK does not implement leaves the build with no way into the guest,
// which is worth refusing up front rather than after a clone has been made.
func TestBuilderPrepare_RejectsAnUnknownCommunicator(t *testing.T) {
	var b Builder
	config := testConfig()
	config["communicator"] = "telnet"

	_, _, err := b.Prepare(config)
	if err == nil {
		t.Fatal("an unrecognised communicator must be rejected")
	}
	if !strings.Contains(err.Error(), "telnet") {
		t.Errorf("error should name the rejected value, got: %s", err)
	}
}

// The communicator defaults to "none", and shutdown_command has nothing to run on with no
// communicator. Every other test here says so by naming "none" explicitly; a template that
// simply omits the option arrives at the same place through ApplyCommunicatorDefault, and
// that is the route a user takes by accident.
func TestBuilderPrepare_RejectsShutdownCommandWhenTheCommunicatorIsOmitted(t *testing.T) {
	var b Builder
	config := testConfig()
	delete(config, "communicator")
	delete(config, "ssh_username")

	_, _, err := b.Prepare(config)
	if err == nil {
		t.Fatal("shutdown_command with no communicator must be rejected")
	}
	if !strings.Contains(err.Error(), "shutdown_command") {
		t.Errorf("error should name shutdown_command, got: %s", err)
	}
}

func TestBuilderPrepare_MissingRequiredFields(t *testing.T) {
	clearConnectionEnv(t)

	for _, key := range []string{"clone_template", "xenserver_host", "xenserver_username", "xenserver_password"} {
		var b Builder
		config := testConfig()
		delete(config, key)

		_, _, err := b.Prepare(config)
		if err == nil {
			t.Errorf("%s: a missing required option must be rejected", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("%s: error should name the missing option, got: %s", key, err)
		}
	}
}

// Each builder has its own Prepare, so the certificate rules have to be exercised
// through this one too.
func TestBuilderPrepare_MissingCACertPathIsRejected(t *testing.T) {
	clearConnectionEnv(t)

	var b Builder
	config := testConfig()
	delete(config, "insecure")

	_, _, err := b.Prepare(config)
	if err == nil {
		t.Fatal("a build that verifies certificates but names none must be rejected")
	}
	if !strings.Contains(err.Error(), "ca_cert_path") {
		t.Errorf("error should name ca_cert_path, got: %s", err)
	}
}

func TestBuilderPrepare_CACertPathIsAccepted(t *testing.T) {
	clearConnectionEnv(t)
	caCertPath := xstest.WriteCACert(t)

	var b Builder
	config := testConfig()
	delete(config, "insecure")
	config["ca_cert_path"] = caCertPath

	if _, _, err := b.Prepare(config); err != nil {
		t.Fatalf("should not have error: %s", err)
	}
	if b.config.CACertPath != caCertPath {
		t.Errorf("ca_cert_path = %q, want %q", b.config.CACertPath, caCertPath)
	}
}

// The environment is a fallback for the connection options in this builder as
// well, and an unset `insecure` must not turn verification off here either.
func TestBuilderPrepare_ConnectionOptionsFallBackToTheEnvironment(t *testing.T) {
	clearConnectionEnv(t)
	t.Setenv(xscommon.EnvPassword, "from-the-environment")
	t.Setenv(xscommon.EnvCACertPath, xstest.WriteCACert(t))

	var b Builder
	config := testConfig()
	delete(config, "xenserver_password")
	delete(config, "insecure")

	if _, _, err := b.Prepare(config); err != nil {
		t.Fatalf("should not have error: %s", err)
	}
	if b.config.Password != "from-the-environment" {
		t.Errorf("xenserver_password = %q, want it from the environment", b.config.Password)
	}
	if b.config.InsecureEnabled() {
		t.Error("verification was left off when nothing asked for that")
	}
}

// A clone inherits its firmware, so `secure_boot` alone cannot be honoured: the builder
// has no way to know the template is UEFI. It used to decode happily and then write
// secureboot="false", the opposite of what was asked.
func TestBuilderPrepare_SecureBootWithoutUEFIIsRejected(t *testing.T) {
	for _, firmware := range []string{"", "bios"} {
		var b Builder
		config := testConfig()
		config["secure_boot"] = true
		if firmware != "" {
			config["firmware"] = firmware
		}

		_, _, err := b.Prepare(config)
		if err == nil {
			t.Errorf("firmware=%q: secure_boot without UEFI must be rejected", firmware)
			continue
		}
		if !strings.Contains(err.Error(), "secure_boot") || !strings.Contains(err.Error(), "uefi") {
			t.Errorf("firmware=%q: error should name secure_boot and uefi, got: %s", firmware, err)
		}
	}
}

func TestBuilderPrepare_SecureBootWithUEFIIsAccepted(t *testing.T) {
	var b Builder
	config := testConfig()
	config["firmware"] = "uefi"
	config["secure_boot"] = true

	_, warns, err := b.Prepare(config)
	if len(warns) > 0 {
		t.Fatalf("bad: %#v", warns)
	}
	if err != nil {
		t.Fatalf("should not have error: %s", err)
	}
	if !b.config.SecureBootEnabled() || b.config.Firmware != "uefi" {
		t.Errorf("secure_boot=%t firmware=%q, want true/uefi", b.config.SecureBootEnabled(), b.config.Firmware)
	}
}

// Leaving firmware unset has to stay valid: that is how a clone says "keep the
// template's". No shipped example exercises it, so this is the only guard against
// someone giving the clone builder an iso-style default.
func TestBuilderPrepare_UnsetFirmwareIsValid(t *testing.T) {
	var b Builder
	config := testConfig()

	_, _, err := b.Prepare(config)
	if err != nil {
		t.Fatalf("should not have error: %s", err)
	}
	if b.config.Firmware != "" {
		t.Errorf("firmware = %q, want it left empty so the template's value is kept", b.config.Firmware)
	}
}

// CA-430878 GAP 1 on this builder: the normalisation lives in CommonConfig.Prepare, but
// it is the guard below that reads the result, and that guard is the clone builder's own.
func TestBuilderPrepare_FirmwareCaseIsNormalised(t *testing.T) {
	var b Builder
	config := testConfig()
	config["firmware"] = "UEFI"
	config["secure_boot"] = true

	_, _, err := b.Prepare(config)

	if err != nil {
		t.Fatalf("firmware = \"UEFI\" with secure_boot must be accepted (UEFI is UEFI); got: %s", err)
	}
	if b.config.Firmware != "uefi" {
		t.Errorf("firmware = %q, want it normalised to \"uefi\"", b.config.Firmware)
	}
}

// CA-430878 GAP 2 on this builder, which never defaulted firmware at all, so an unknown
// value reached the VM's HVM boot params with nothing in between.
func TestBuilderPrepare_FirmwareEnumIsValidated(t *testing.T) {
	var b Builder
	config := testConfig()
	config["firmware"] = "efi" // neither bios nor uefi

	_, _, err := b.Prepare(config)

	if err == nil {
		t.Fatal("firmware = \"efi\" must be rejected: firmware must be one of bios/uefi")
	}
	if !strings.Contains(err.Error(), "firmware") {
		t.Errorf("error should name firmware, got: %s", err)
	}
}

// CA-430878 GAP 4 on this builder. stepCloneInstance caught it after the clone already
// existed; validate is where it costs nothing.
func TestBuilderPrepare_CorePerSocketNotDividingVCPUsIsRejected(t *testing.T) {
	var b Builder
	config := testConfig()
	config["vcpus"] = 4
	config["cores_per_socket"] = 8

	_, _, err := b.Prepare(config)

	if err == nil {
		t.Fatal("cores_per_socket = 8 with vcpus = 4 must be rejected")
	}
	if !strings.Contains(err.Error(), "cores_per_socket") {
		t.Errorf("error should name cores_per_socket, got: %s", err)
	}
}

// CP-314723. Both builders validate the pair on the same two rules, and this one divides
// cleanly, so it clears the check above and fails below the toolstack instead (CA-198276).
func TestBuilderPrepare_TooManySocketsIsRejected(t *testing.T) {
	var b Builder
	config := testConfig()
	config["vcpus"] = 64
	config["cores_per_socket"] = 1

	_, _, err := b.Prepare(config)

	if err == nil {
		t.Fatal("64 sockets is beyond what XenServer supports and must be rejected at validate time")
	}
	if !strings.Contains(err.Error(), "sockets") {
		t.Errorf("error should say how many sockets the pair gives, got: %s", err)
	}
}

// The other half of GAP 4 here: unlike the iso builder, a clone defaults neither value,
// so one on its own means "keep the template's" and must not be measured against a zero.
func TestBuilderPrepare_CorePerSocketAloneIsValidOnAClone(t *testing.T) {
	for name, key := range map[string]string{
		"cores_per_socket alone": "cores_per_socket",
		"vcpus alone":            "vcpus",
	} {
		var b Builder
		config := testConfig()
		config[key] = 3

		if _, _, err := b.Prepare(config); err != nil {
			t.Errorf("%s: a clone inherits the other half, so this must be accepted: %s", name, err)
		}
	}
}

// secure_boot is a *bool so a clone can tell "not mentioned" (inherit) from "explicitly
// off". Collapsing the two into a plain bool is what made every clone write
// secureboot="false".
func TestBuilderPrepare_SecureBootIsTriState(t *testing.T) {
	unset := testConfig()

	off := testConfig()
	off["secure_boot"] = false

	on := testConfig()
	on["secure_boot"] = true
	on["firmware"] = "uefi"

	for name, tc := range map[string]struct {
		config  map[string]interface{}
		wantSet bool
		wantOn  bool
	}{
		"unset":          {unset, false, false},
		"explicit false": {off, true, false},
		"explicit true":  {on, true, true},
	} {
		var b Builder
		if _, _, err := b.Prepare(tc.config); err != nil {
			t.Errorf("%s: should not have error: %s", name, err)
			continue
		}
		if got := b.config.SecureBoot != nil; got != tc.wantSet {
			t.Errorf("%s: secure_boot set = %t, want %t", name, got, tc.wantSet)
		}
		if got := b.config.SecureBootEnabled(); got != tc.wantOn {
			t.Errorf("%s: SecureBootEnabled() = %t, want %t", name, got, tc.wantOn)
		}
	}
}

// The same tri-state shape as secure_boot, and on a clone the same meaning for the unset
// case: keep the source's. stepCloneInstance reads the pointer, so Prepare must not
// collapse it -- a default assigned here is a flag written under an installed Windows,
// which is what XenServer warns stops the guest booting (CA-431569).
func TestBuilderPrepare_HasVendorDeviceIsTriStateAndInheritsWhenUnset(t *testing.T) {
	unset := testConfig()

	off := testConfig()
	off["has_vendor_device"] = false

	on := testConfig()
	on["has_vendor_device"] = true

	for name, tc := range map[string]struct {
		config  map[string]interface{}
		wantSet bool
		wantOn  bool
	}{
		"unset":          {unset, false, false},
		"explicit false": {off, true, false},
		"explicit true":  {on, true, true},
	} {
		var b Builder
		if _, _, err := b.Prepare(tc.config); err != nil {
			t.Errorf("%s: should not have error: %s", name, err)
			continue
		}
		if got := b.config.HasVendorDevice != nil; got != tc.wantSet {
			t.Errorf("%s: has_vendor_device set = %t, want %t", name, got, tc.wantSet)
		}
		if tc.wantSet && *b.config.HasVendorDevice != tc.wantOn {
			t.Errorf("%s: has_vendor_device = %t, want %t", name, *b.config.HasVendorDevice, tc.wantOn)
		}
	}
}

// vtpm carries the same UEFI rule as secure_boot, for a sharper reason: XAPI checks
// firmware not when the vTPM is created but at start time, with a bare NOT_IMPLEMENTED.
// Without this check the build would clone, attach and then die naming neither option.
func TestBuilderPrepare_VTPMWithoutUEFIIsRejected(t *testing.T) {
	for _, firmware := range []string{"", "bios"} {
		var b Builder
		config := testConfig()
		config["vtpm"] = true
		if firmware != "" {
			config["firmware"] = firmware
		}

		_, _, err := b.Prepare(config)
		if err == nil {
			t.Errorf("firmware=%q: vtpm without UEFI must be rejected", firmware)
			continue
		}
		if !strings.Contains(err.Error(), "vtpm") || !strings.Contains(err.Error(), "uefi") {
			t.Errorf("firmware=%q: error should name vtpm and uefi, got: %s", firmware, err)
		}
	}
}

func TestBuilderPrepare_VTPMWithUEFIIsAccepted(t *testing.T) {
	var b Builder
	config := testConfig()
	config["firmware"] = "uefi"
	config["vtpm"] = true

	_, warns, err := b.Prepare(config)
	if len(warns) > 0 {
		t.Fatalf("bad: %#v", warns)
	}
	if err != nil {
		t.Fatalf("should not have error: %s", err)
	}
	if !b.config.VTPMEnabled() {
		t.Error("vtpm = false, want true")
	}
}

// The same tri-state shape as secure_boot: a clone inherits its source's vTPM, so "not
// mentioned" keeps it while an explicit false destroys it. The explicit false needs no
// firmware -- removing a vTPM is valid on a BIOS template, only true carries the rule.
func TestBuilderPrepare_VTPMIsTriState(t *testing.T) {
	unset := testConfig()

	off := testConfig()
	off["vtpm"] = false

	on := testConfig()
	on["vtpm"] = true
	on["firmware"] = "uefi"

	for name, tc := range map[string]struct {
		config  map[string]interface{}
		wantSet bool
		wantOn  bool
	}{
		"unset":          {unset, false, false},
		"explicit false": {off, true, false},
		"explicit true":  {on, true, true},
	} {
		var b Builder
		if _, _, err := b.Prepare(tc.config); err != nil {
			t.Errorf("%s: should not have error: %s", name, err)
			continue
		}
		if got := b.config.VTPM != nil; got != tc.wantSet {
			t.Errorf("%s: vtpm set = %t, want %t", name, got, tc.wantSet)
		}
		if got := b.config.VTPMEnabled(); got != tc.wantOn {
			t.Errorf("%s: VTPMEnabled() = %t, want %t", name, got, tc.wantOn)
		}
	}
}

// These options moved from the monolithic Config into CommonConfig. The clone builder
// has to keep decoding them; a tag lost in the move would leave them silently unset.
func TestBuilderPrepare_SharedOptionsMovedIntoCommonConfig(t *testing.T) {
	var b Builder
	config := testConfig()
	config["cores_per_socket"] = 2
	config["vgpu_profile"] = "GRID T4-2Q"
	config["install_timeout"] = "45m"
	config["firmware"] = "uefi"
	config["secure_boot"] = true
	config["create_snapshot"] = true
	config["snapshot_name"] = "pre-export"
	config["vm_other_config"] = map[string]string{"owner": "packer"}

	_, _, err := b.Prepare(config)
	if err != nil {
		t.Fatalf("should not have error: %s", err)
	}

	xstest.WantUint(t, "cores_per_socket", b.config.CorePerSocket, 2)
	if b.config.VGPUProfile != "GRID T4-2Q" {
		t.Errorf("vgpu_profile = %q, want \"GRID T4-2Q\"", b.config.VGPUProfile)
	}
	if b.config.InstallTimeout != 45*time.Minute {
		t.Errorf("install_timeout = %s, want 45m", b.config.InstallTimeout)
	}
	if b.config.Firmware != "uefi" {
		t.Errorf("firmware = %q, want \"uefi\"", b.config.Firmware)
	}
	if !b.config.SecureBootEnabled() {
		t.Error("secure_boot = false, want true")
	}
	if !b.config.CreateSnapshot {
		t.Error("create_snapshot = false, want true")
	}
	if b.config.SnapshotName != "pre-export" {
		t.Errorf("snapshot_name = %q, want \"pre-export\"", b.config.SnapshotName)
	}
	if b.config.VMOtherConfig["owner"] != "packer" {
		t.Errorf("vm_other_config = %v, want owner=packer", b.config.VMOtherConfig)
	}
}

// install_timeout is defaulted in CommonConfig.Prepare now that both builders
// share it, rather than separately in each builder.
func TestBuilderPrepare_InstallTimeoutDefault(t *testing.T) {
	var b Builder
	config := testConfig()

	_, _, err := b.Prepare(config)
	if err != nil {
		t.Fatalf("should not have error: %s", err)
	}

	if b.config.InstallTimeout != 200*time.Minute {
		t.Errorf("install_timeout = %s, want 200m", b.config.InstallTimeout)
	}
}

func TestBuilderPrepare_Format(t *testing.T) {
	var b Builder
	config := testConfig()

	// Bad
	config["format"] = "foo"
	_, warns, err := b.Prepare(config)
	if len(warns) > 0 {
		t.Fatalf("bad: %#v", warns)
	}
	if err == nil {
		t.Fatal("should have error")
	}

	// Good
	for _, format := range []string{"xva", "vdi_raw"} {
		config["format"] = format
		b = Builder{}
		_, warns, err = b.Prepare(config)
		if len(warns) > 0 {
			t.Fatalf("bad: %#v", warns)
		}
		if err != nil {
			t.Fatalf("%s: should not have error: %s", format, err)
		}
		if b.config.Format != format {
			t.Errorf("format = %q, want %q", b.config.Format, format)
		}
	}
}

// CA-430805: the minimal clone template -- required fields only -- has to stay valid and
// say plainly that it will not start the VM. Rejecting it would take away the one thing
// such a template can usefully do: clone a template and export it. The deleted
// ssh_username matters too: both builders wire the same SSHConfig.Prepare, which must not
// demand one for communicator = "none". shutdown_command runs inside the guest, so it is
// rejected for this communicator.
func TestBuilderPrepare_CommunicatorNoneWithNothingStagedWarnsItWillNotBoot(t *testing.T) {
	var b Builder
	config := testConfig()
	delete(config, "ssh_username")
	delete(config, "shutdown_command")
	config["communicator"] = "none"

	_, warns, err := b.Prepare(config)

	if err != nil {
		t.Fatalf("a minimal clone must stay valid: %s", err)
	}
	if b.config.bootsTheGuest() {
		t.Error("with nothing staged and no communicator the VM must not be started")
	}
	if !anyWarning(warns, "never starts it") {
		t.Errorf("expected a warning that the build does not start the VM, got %v", warns)
	}
}

func TestBuilderPrepare_CommunicatorNoneWithATriggerWarnsItWillWait(t *testing.T) {
	var b Builder
	config := testConfig()
	delete(config, "ssh_username")
	delete(config, "shutdown_command")
	config["communicator"] = "none"
	config["boot_command"] = []string{"<enter>"}

	_, warns, err := b.Prepare(config)

	if err != nil {
		t.Fatalf("should be accepted: %s", err)
	}
	if !b.config.bootsTheGuest() {
		t.Error("the VM has to be started for this to mean anything")
	}
	if !anyWarning(warns, "install_timeout") {
		t.Errorf("expected a warning naming install_timeout, got %v", warns)
	}
}

// A communicator logs in, provisions and shuts the guest down, so the VM is started
// whatever else the template does or does not stage.
func TestBuilderPrepare_ARealCommunicatorAlwaysBootsTheGuest(t *testing.T) {
	var b Builder
	config := testConfig()

	if _, _, err := b.Prepare(config); err != nil {
		t.Fatalf("should not have error: %s", err)
	}
	if !b.config.bootsTheGuest() {
		t.Error("communicator = \"ssh\" needs a running guest to log in to")
	}
}

func anyWarning(warns []string, substr string) bool {
	for _, w := range warns {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// No cores_per_socket default either: it must stay unset so the step, which writes the
// topology only when both are set, keeps the source template's.
// TestBuilderPrepare_UnsetSizingKeepsTheTemplatesValues covers the other three fields.
func TestBuilderPrepare_UnsetCorePerSocketKeepsTheTemplatesTopology(t *testing.T) {
	var b Builder
	config := testConfig()

	_, _, err := b.Prepare(config)
	if err != nil {
		t.Fatalf("should not have error: %s", err)
	}
	xstest.WantUnset(t, "cores_per_socket", b.config.CorePerSocket)
}

// CA-430886: a WinRM clone never logs in over ssh, yet used to be rejected unless the
// template also carried an ssh_username nothing would read. ssh_config_test.go covers the
// mechanism; this pins it through a whole template going through the builder's Prepare.
func TestBuilderPrepare_WinRMNeedsNoSSHUsername(t *testing.T) {
	var b Builder
	config := testConfig()
	config["communicator"] = "winrm"
	config["winrm_username"] = "Administrator"
	config["winrm_password"] = "secret"
	delete(config, "ssh_username")

	_, _, err := b.Prepare(config)

	if err != nil {
		t.Fatalf("a WinRM clone with winrm credentials and no ssh_username should be accepted: %s", err)
	}
}

// CA-431807. XAPI refuses VM.snapshot on a template, so converting first made
// create_snapshot = true impossible for any build that also set format = "none" -- the
// only setting that makes the conversion run at all.
func TestFinalStepsSnapshotsBeforeConverting(t *testing.T) {
	var order []string
	for _, step := range finalSteps() {
		order = append(order, fmt.Sprintf("%T", step))
	}

	want := []string{
		"*common.StepShutdown",
		"*common.StepDetachVdi",
		"*common.StepCreateSnapshot",
		"*common.StepSetVmToTemplate",
		"*common.StepExport",
	}
	if !reflect.DeepEqual(order, want) {
		t.Errorf("final steps = %v, want %v", order, want)
	}
}

// CA-431825. Each of these is a value the build typed, not an option it left out, and
// none of them describes a usable VM. They used to be the same value as "unset", so the
// source template's default was applied and the build reported success having ignored the setting.
func TestBuilderPrepare_ExplicitZeroSizingIsRejected(t *testing.T) {
	for _, tc := range []struct {
		option string
		value  interface{}
	}{
		{"vm_name", ""},
		{"vcpus", 0},
		{"cores_per_socket", 0},
		{"vm_memory_gb", 0},
		{"disk_size_gb", 0},
	} {
		t.Run(tc.option, func(t *testing.T) {
			var b Builder
			config := testConfig()
			config[tc.option] = tc.value

			_, _, err := b.Prepare(config)
			if err == nil {
				t.Fatalf("%s = %#v must be rejected, not replaced by a default", tc.option, tc.value)
			}
			if !strings.Contains(err.Error(), tc.option) {
				t.Errorf("error should name %s, got: %s", tc.option, err)
			}
		})
	}
}

// CA-431860. One overloaded option could not say which kind a name meant, so a
// source VM was unbuildable whenever a template shared its name-label.
func TestBuilderPrepare_CloneTemplateAndCloneVMAreExclusive(t *testing.T) {
	clearConnectionEnv(t)

	var b Builder
	config := testConfig()
	config["clone_vm"] = "win11-build-src"

	_, _, err := b.Prepare(config)
	if err == nil {
		t.Fatal("naming both a source template and a source VM must be rejected")
	}
	for _, want := range []string{"only one", "clone_template", "clone_vm"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q, got: %s", want, err)
		}
	}
}

func TestBuilderPrepare_AcceptsCloneVMOnItsOwn(t *testing.T) {
	clearConnectionEnv(t)

	var b Builder
	config := testConfig()
	delete(config, "clone_template")
	config["clone_vm"] = "win11-build-src"

	if _, _, err := b.Prepare(config); err != nil {
		t.Errorf("clone_vm alone is one half of a required one-of pair: %s", err)
	}
}
