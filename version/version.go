// Copyright (c) 2026. Citrix Systems, Inc.

package version

import (
	"github.com/hashicorp/packer-plugin-sdk/version"
)

var (
	// Version is the main version number that is being run at the moment.
	Version = "v0.9.1"

	// VersionPrerelease marks Version as a pre-release ("dev", "beta", "rc1");
	// empty means a final release.
	VersionPrerelease = ""

	VersionMetadata = ""

	// PluginVersion is used by the plugin set to allow Packer to recognize
	// what version this plugin is.
	PluginVersion = version.NewPluginVersion(Version, VersionPrerelease, VersionMetadata)
)
