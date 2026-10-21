// Copyright (c) 2026. Citrix Systems, Inc.

package main

import (
	"reflect"
	"sort"
	"testing"

	"github.com/hashicorp/hcl/v2/hcldec"
)

// The xva builder was removed in CP-313739 because it could not run: builder.go never put
// "config" in the state bag that stepImportInstance then asserted on.
func TestPluginSet_RegistersOnlyIsoAndClone(t *testing.T) {
	want := []string{"vm-clone", "vm-iso"}

	builders := pluginSet().Builders
	got := make([]string, 0, len(builders))
	for name := range builders {
		got = append(got, name)
	}
	sort.Strings(got)

	if !reflect.DeepEqual(got, want) {
		t.Fatalf("registered builders = %v, want %v", got, want)
	}
}

// isoOnlyKeys are the options only the iso builder implements. Naming one on a clone is a
// validation error, not the silent no-op the pre-split monolithic Config allowed.
//
// vtpm left this list in CA-430086: it is declared per builder because a clone's `false`
// also removes the source's vTPM (unset/true/false are three requests, hence *bool) while
// the iso builder's plain bool only declines to add one. Both render as one cty.Bool.
var isoOnlyKeys = []string{
	"iso_checksum",
	"iso_urls",
	"iso_url",
	"iso_target_path",
	"iso_target_extension",
	"iso_name",
	"guest_tools_iso",
}

// cloneOnlyKeys are the options only the clone builder implements. clone_vm is one
// because the iso builder installs onto a fresh disk from install media, so it has no
// VM or snapshot to clone (CA-431860).
var cloneOnlyKeys = []string{
	"clone_vm",
}

// renamedKeys maps every community field name CP-313735 renamed to its replacement. The old
// spelling has to disappear from the spec; only `vTPM` still reaches Prepare, whose
// mapstructure decode matches case-insensitively (see iso's OldVTPMSpellingStillDecodes).
var renamedKeys = map[string]string{
	"remote_host":            "xenserver_host",
	"remote_username":        "xenserver_username",
	"remote_password":        "xenserver_password",
	"sr_name":                "vm_sr",
	"sr_iso_name":            "iso_sr",
	"vm_memory":              "vm_memory_gb",
	"disk_size":              "disk_size_gb",
	"tools_iso_name":         "guest_tools_iso",
	"vTPM":                   "vtpm",
	"server_cert":            "ca_cert_path",
	"server_cert_file":       "ca_cert_path",
	"skip_cert_verification": "insecure",
}

// droppedKeys are the options CP-313735 removed outright: unread, duplicating an SDK
// option, or exposing a purely local implementation detail.
var droppedKeys = []string{
	"floppy_files",
	"export_network_names",
	"ip_getter",
	"platform_args",
	"ssh_skip_nat_mapping",
	"host_port_min",
	"host_port_max",
	"http_port_min",
	"http_port_max",
	"ssh_host_port_min",
	"ssh_host_port_max",
	"source_path",
	"convert_to_template",
	"ssh_key_path",
	"vcpus_max",
	"vcpus_atstartup",
	// CP-313736: folded into iso_checksum's "type:hash" prefix.
	"iso_checksum_type",
}

// builderSpecs returns each registered builder's HCL2 spec: the surface Packer decodes a
// template against, and therefore where "valid on this builder" is really decided.
func builderSpecs(t *testing.T) map[string]hcldec.ObjectSpec {
	t.Helper()

	specs := make(map[string]hcldec.ObjectSpec)
	for name, builder := range pluginSet().Builders {
		specs[name] = builder.ConfigSpec()
	}
	return specs
}

func missingFrom(spec hcldec.ObjectSpec, keys []string) []string {
	var missing []string
	for _, key := range keys {
		if _, ok := spec[key]; !ok {
			missing = append(missing, key)
		}
	}
	sort.Strings(missing)
	return missing
}

// The split is only worth anything if the specs really differ by the options each builder
// implements alone, and nothing else.
func TestBuilderSpecs_DifferOnlyByEachBuildersOwnKeys(t *testing.T) {
	specs := builderSpecs(t)

	var isoOnly, cloneOnly []string
	for key := range specs["vm-iso"] {
		if _, ok := specs["vm-clone"][key]; !ok {
			isoOnly = append(isoOnly, key)
		}
	}
	for key := range specs["vm-clone"] {
		if _, ok := specs["vm-iso"][key]; !ok {
			cloneOnly = append(cloneOnly, key)
		}
	}
	sort.Strings(isoOnly)
	sort.Strings(cloneOnly)

	wantISOOnly := append([]string(nil), isoOnlyKeys...)
	sort.Strings(wantISOOnly)
	wantCloneOnly := append([]string(nil), cloneOnlyKeys...)
	sort.Strings(wantCloneOnly)

	if !reflect.DeepEqual(isoOnly, wantISOOnly) {
		t.Errorf("attributes only on the iso builder = %v, want %v", isoOnly, wantISOOnly)
	}
	if !reflect.DeepEqual(cloneOnly, wantCloneOnly) {
		t.Errorf("attributes only on the clone builder = %v, want %v", cloneOnly, wantCloneOnly)
	}
}

// A shared option has to mean the same thing on both builders. Comparing the specs and not
// just the key sets catches a field duplicated per builder with a different cty type; Go's
// bool and *bool both render cty.Bool, which is how vtpm's per-builder split passes.
func TestBuilderSpecs_SharedKeysAreIdenticalOnBothBuilders(t *testing.T) {
	specs := builderSpecs(t)

	shared := 0
	for key, isoSpec := range specs["vm-iso"] {
		cloneSpec, ok := specs["vm-clone"][key]
		if !ok {
			continue
		}
		shared++
		if !reflect.DeepEqual(isoSpec, cloneSpec) {
			t.Errorf("%s: iso spec %#v differs from clone spec %#v", key, isoSpec, cloneSpec)
		}
	}

	// The loop above walks only the overlap, so an overlap of one satisfies it. Pin the size
	// too: an option dropped from the clone builder, or a new iso-only option missing from
	// isoOnlyKeys, fails here.
	if want := len(specs["vm-iso"]) - len(isoOnlyKeys); shared != want {
		t.Errorf("%d shared attributes, want %d (the iso spec minus its %d iso-only options)",
			shared, want, len(isoOnlyKeys))
	}
}

// The generated spec is where a rename is really decided: a mapstructure tag that never
// reaches it is not a rename. Both builders generate their own spec, so check both.
func TestBuilderSpecs_RenamedKeysReplaceTheOldOnes(t *testing.T) {
	for builder, spec := range builderSpecs(t) {
		for old, renamed := range renamedKeys {
			// tools_iso_name -> guest_tools_iso is iso-only, so neither spelling
			// belongs on the clone builder; DifferOnlyByEachBuildersOwnKeys covers the
			// new one reaching the iso spec. vTPM -> vtpm is checked on both since
			// CA-430086 put vtpm on the clone builder too.
			_, present := spec[renamed]
			if !present && !isISOOnly(renamed) {
				t.Errorf("%s: spec is missing the renamed attribute %q (was %q)", builder, renamed, old)
			}
			if _, ok := spec[old]; ok {
				t.Errorf("%s: spec still exposes the old attribute %q; it must be replaced by %q", builder, old, renamed)
			}
		}
	}
}

func isISOOnly(key string) bool {
	for _, isoKey := range isoOnlyKeys {
		if key == isoKey {
			return true
		}
	}
	return false
}

func TestBuilderSpecs_DroppedKeysAreGone(t *testing.T) {
	for builder, spec := range builderSpecs(t) {
		for _, dropped := range droppedKeys {
			if _, ok := spec[dropped]; ok {
				t.Errorf("%s: spec still exposes the dropped attribute %q", builder, dropped)
			}
		}
	}
}

// ssh_key_path was dropped in favour of the SDK's own option, so the replacement has to
// be present or key-based auth is gone.
func TestBuilderSpecs_SSHPrivateKeyFileReplacesSSHKeyPath(t *testing.T) {
	for builder, spec := range builderSpecs(t) {
		if _, ok := spec["ssh_private_key_file"]; !ok {
			t.Errorf("%s: spec is missing ssh_private_key_file, the SDK option that replaces ssh_key_path", builder)
		}
	}
}

// The revamp in 63feaf3 dropped the mapstructure tags on VCPUsMax and VCPUsAtStartup, so
// packer-sdc derived the HCL keys from the Go field names and generated
// "vcp_us_max"/"vcp_us_at_startup" -- unknown keys, so every HCL2 template failed to
// validate. Both fields are now one tagged `vcpus`; guard that it stays that way.
func TestBuilderSpecs_VCPUKeys(t *testing.T) {
	for builder, spec := range builderSpecs(t) {
		if _, ok := spec["vcpus"]; !ok {
			t.Errorf("%s: spec is missing the \"vcpus\" attribute", builder)
		}
		for _, mangled := range []string{"vcp_us_max", "vcp_us_at_startup", "vcp_us"} {
			if _, ok := spec[mangled]; ok {
				t.Errorf("%s: spec exposes mangled attribute %q; its mapstructure tag is missing", builder, mangled)
			}
		}
	}
}

// Splitting the config moved eleven options into CommonConfig. A mapstructure tag lost on
// the way would drop the option from both builders at once, which no per-builder test
// would notice.
func TestBuilderSpecs_OptionsMovedIntoCommonConfigSurviveOnBothBuilders(t *testing.T) {
	moved := []string{
		"vcpus", "cores_per_socket", "vm_memory_gb", "disk_size_gb",
		"vm_other_config", "vgpu_profile", "install_timeout",
		"firmware", "secure_boot", "create_snapshot", "snapshot_name",
	}

	for builder, spec := range builderSpecs(t) {
		if missing := missingFrom(spec, moved); len(missing) > 0 {
			t.Errorf("%s: spec is missing shared attributes %v", builder, missing)
		}
	}
}
