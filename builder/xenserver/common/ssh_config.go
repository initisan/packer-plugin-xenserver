// Copyright (c) 2026. Citrix Systems, Inc.

//go:generate packer-sdc struct-markdown
package common

import (
	"errors"
	"fmt"

	"github.com/hashicorp/packer-plugin-sdk/communicator"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
)

// SSHConfig carries the SDK communicator options: the builders reach the guest through
// the SDK's communicator, which accepts any guest host key -- the VM is created by this
// build and has no key to pin beforehand.
//
// One of them is the plugin's to decide rather than the template's: the communicator
// defaults to `none` here rather than the SDK's `ssh`, so a template that asks for nothing
// inside the guest is never made to supply an `ssh_username`.
//
// The address is the template's when it names one. `ssh_host` and `winrm_host` are dialled
// as given, and only an unset one falls back to the address the running VM reports; the
// ports are the SDK's to apply: `ssh_port` and `winrm_port` are dialled as given, and an
// unset one takes the SDK default (22 for SSH, 5985 for WinRM, 5986 with `winrm_use_ssl`).
// Either way the build first waits for the guest to report an address, since that wait is
// also how it knows the install has finished.
//
// The guest credentials need not be in the template at all. `ssh_username`,
// `ssh_password`, `ssh_private_key_file`, `winrm_username` and `winrm_password` each fall
// back to a `XENSERVER_`-prefixed environment variable when the template leaves them
// unset, the same contract the connection options have, and a value the template does set
// always wins over the environment. The builder documentation carries the table, under
// "Supplying guest credentials from the environment".
type SSHConfig struct {
	Comm communicator.Config `mapstructure:",squash"`
}

// The environment variables Prepare falls back to for the guest credentials, the
// counterpart to the connection ones in common_config.go. Keeping a password out of a
// build file is the point of both, and the guest's is no less a credential than the
// pool's.
const (
	EnvSSHUsername       = "XENSERVER_SSH_USERNAME"
	EnvSSHPassword       = "XENSERVER_SSH_PASSWORD"
	EnvSSHPrivateKeyFile = "XENSERVER_SSH_PRIVATE_KEY_FILE"
	EnvWinRMUsername     = "XENSERVER_WINRM_USERNAME"
	EnvWinRMPassword     = "XENSERVER_WINRM_PASSWORD"
)

// ApplyCommunicatorDefault fills in an unset communicator. It is idempotent.
//
// Both builders must call it before CommonConfig.Prepare, which validates against the
// communicator: a shutdown_command on a defaulted "none" template would otherwise be
// accepted and then never run, since StepShutdown finds the guest already halted.
func (c *SSHConfig) ApplyCommunicatorDefault() {
	if c.Comm.Type == "" {
		c.Comm.Type = "none"
	}
}

// applyGuestCredentialEnv fills in the guest credentials the template left unset from the
// environment. It is applied whatever the communicator is: the SDK reads only the set
// belonging to the one in use, so a `winrm` build is unaffected by an exported
// XENSERVER_SSH_PASSWORD, and a "none" build reads neither set.
func (c *SSHConfig) applyGuestCredentialEnv() {
	applyEnvFallbacks([]envFallback{
		{&c.Comm.SSHUsername, EnvSSHUsername},
		{&c.Comm.SSHPassword, EnvSSHPassword},
		{&c.Comm.SSHPrivateKeyFile, EnvSSHPrivateKeyFile},
		{&c.Comm.WinRMUser, EnvWinRMUsername},
		{&c.Comm.WinRMPassword, EnvWinRMPassword},
	})
}

func (c *SSHConfig) Prepare(ctx *interpolate.Context) []error {
	// Before c.Comm.Prepare, which defaults to "ssh" and then demands an ssh_username
	// a silent template never gave.
	c.ApplyCommunicatorDefault()

	// Before c.Comm.Prepare too, and for the same reason: it is what reports a missing
	// ssh_username or winrm_username, so a credential filled in afterwards would arrive
	// too late to answer the error it was meant to answer. It also reads
	// ssh_private_key_file from disk, so the fallback has to be in place by then for the
	// key to be loaded at all.
	c.applyGuestCredentialEnv()

	// Packer core populates the redaction filter from HCL `sensitive = true` only, so a
	// guest password that arrived from the environment reaches packer.log unredacted
	// unless it is registered here. Set accumulates; an empty one is skipped by the filter.
	packersdk.LogSecretFilter.Set(c.Comm.SSHPassword, c.Comm.WinRMPassword)

	// Each communicator's own credentials are the SDK's to validate. The plugin used to
	// demand an ssh_username on top, WinRM templates included (CA-430886).
	errs := c.Comm.Prepare(ctx)

	// The SDK also accepts "docker" and "dockerWindowsContainer", which nothing here can
	// connect to: StepConnect halts with "unknown communicator type" after the install.
	switch c.Comm.Type {
	case "ssh", "winrm", "none":
	default:
		errs = append(errs, errors.New("communicator must be one of \"ssh\", \"winrm\" or \"none\", got \""+
			c.Comm.Type+"\""))
	}

	// With one exception: the SDK checks winrm_username and not winrm_password, and its
	// WinRM communicator authenticates with nothing else. Neither builder gives StepConnect
	// a WinRMConfig callback, so no later step can supply one either, and an empty password
	// costs the whole installation plus winrm_timeout before the build says so.
	if c.Comm.Type == "winrm" && c.Comm.WinRMPassword == "" {
		errs = append(errs, fmt.Errorf("winrm_password must be specified when communicator = \"winrm\" "+
			"(or set %s): the WinRM communicator has no other way to authenticate, so the build would "+
			"install and then fail at the connect step.", EnvWinRMPassword))
	}

	return errs
}
