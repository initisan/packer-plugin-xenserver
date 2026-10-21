// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"

	"xenapi"

	"github.com/xenserver/packer-plugin-xenserver/version"
)

const (
	// ProvenanceKey marks a VM or template as built by this plugin, queryable with
	// `xe vm-list other-config:xs_packer_plugin=true`; for templates use `xe template-list`
	// with the same filter, since vm-list matches is-a-template=false by default.
	ProvenanceKey = "xs_packer_plugin"

	// ProvenanceVersionKey records the plugin release that produced the object.
	ProvenanceVersionKey = "xs_packer_plugin_version"

	// ProvenanceValue is the value written to ProvenanceKey.
	ProvenanceValue = "true"
)

// PluginVersion returns the release string stamped into other-config: the goreleaser
// ldflag on a release build, the in-code constant otherwise.
func PluginVersion() string {
	return version.PluginVersion.FormattedVersion()
}

// BuildProvenanceOtherConfig returns a new other-config map -- neither input is modified
// -- holding current, then the user-supplied vm_other_config, then the provenance keys.
//
// The order matters: both builders create their VM with VM.clone, so current can carry a
// stale provenance stamp from a template this plugin itself built. Writing the provenance
// keys last makes them authoritative over that and over any colliding user key.
func BuildProvenanceOtherConfig(current, userSupplied map[string]string) map[string]string {
	merged := make(map[string]string, len(current)+len(userSupplied)+2)
	for key, value := range current {
		merged[key] = value
	}
	for key, value := range userSupplied {
		merged[key] = value
	}
	merged[ProvenanceKey] = ProvenanceValue
	merged[ProvenanceVersionKey] = PluginVersion()
	return merged
}

// StampVM writes the build-provenance keys, merged with userSupplied, onto instance. Call
// it for every VM the plugin creates, unconditionally: an empty vm_other_config must
// still produce a stamped VM. StepSetVmToTemplate leaves the same XAPI object, so the
// stamp survives and needs no second write.
func StampVM(c *Connection, instance xenapi.VMRef, userSupplied map[string]string) error {
	current, err := xenapi.VM.GetOtherConfig(c.GetSession(), instance)
	if err != nil {
		return fmt.Errorf("error getting VM other-config: %s", err.Error())
	}

	err = xenapi.VM.SetOtherConfig(c.GetSession(), instance, BuildProvenanceOtherConfig(current, userSupplied))
	if err != nil {
		return fmt.Errorf("error setting VM other-config: %s", err.Error())
	}

	return nil
}
