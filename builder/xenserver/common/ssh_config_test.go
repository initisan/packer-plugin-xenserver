// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
)

// guestCredentialEnvVars is every variable the guest credentials fall back to.
var guestCredentialEnvVars = []string{
	EnvSSHUsername, EnvSSHPassword, EnvSSHPrivateKeyFile, EnvWinRMUsername, EnvWinRMPassword,
}

// prepareSSH runs Prepare with every guest-credential variable cleared.
func prepareSSH(t *testing.T, c *SSHConfig) []error {
	t.Helper()
	return prepareSSHWithEnv(t, c, nil)
}

// prepareSSHWithEnv runs Prepare with exactly the XENSERVER_* guest variables in env set
// and every other one cleared -- otherwise one left exported in a developer's shell would
// quietly satisfy the tests that check a credential is demanded.
func prepareSSHWithEnv(t *testing.T, c *SSHConfig, env map[string]string) []error {
	t.Helper()
	for _, key := range guestCredentialEnvVars {
		t.Setenv(key, env[key])
	}
	return c.Prepare(&interpolate.Context{})
}

// writePrivateKey writes a throwaway SSH private key and returns its path. The SDK reads
// and parses ssh_private_key_file during Prepare, so pointing the option at anything else
// fails for a reason that has nothing to do with the fallback under test.
func writePrivateKey(t *testing.T) string {
	t.Helper()

	_, key, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generating a test key: %v", err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatalf("marshalling a test key: %v", err)
	}

	path := filepath.Join(t.TempDir(), "id_ed25519")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), 0o600); err != nil {
		t.Fatalf("writing a test key: %v", err)
	}
	return path
}

// Nothing logs in to the guest of a build with no communicator, so demanding a username
// forced every such template to carry a throwaway one.
func TestSSHConfigPrepare_DoesNotRequireAnSSHUsernameWithoutACommunicator(t *testing.T) {
	c := &SSHConfig{}
	c.Comm.Type = "none"

	if errs := prepareSSH(t, c); len(errs) > 0 {
		t.Fatalf("communicator = \"none\" must not require an ssh_username: %v", errs)
	}
}

// The count is the assertion: the plugin used to append a second copy of the SDK's error.
func TestSSHConfigPrepare_SSHRequiresAnSSHUsernameExactlyOnce(t *testing.T) {
	c := &SSHConfig{}
	c.Comm.Type = "ssh"

	errs := prepareSSH(t, c)

	var found int
	for _, err := range errs {
		if strings.Contains(err.Error(), "ssh_username") {
			found++
		}
	}
	if found != 1 {
		t.Errorf("communicator = \"ssh\" should report a missing ssh_username once, got %d of %v", found, errs)
	}
}

// CA-430886: a WinRM build never opens an SSH connection, yet used to be rejected unless
// the template carried an ssh_username nothing would ever read.
func TestSSHConfigPrepare_WinRMNeedsNoSSHUsername(t *testing.T) {
	c := &SSHConfig{}
	c.Comm.Type = "winrm"
	c.Comm.WinRMUser = "Administrator"
	c.Comm.WinRMPassword = "secret"

	errs := prepareSSH(t, c)

	if len(errs) > 0 {
		t.Fatalf("a WinRM template carrying winrm credentials should be accepted: %v", errs)
	}
	// The SDK's own WinRM defaulting ran.
	if c.Comm.WinRMPort != 5985 {
		t.Errorf("winrm_port = %d, want the SDK default 5985", c.Comm.WinRMPort)
	}
}

// Dropping the ssh_username demand must not take the WinRM one with it.
func TestSSHConfigPrepare_WinRMStillRequiresAWinRMUsername(t *testing.T) {
	c := &SSHConfig{}
	c.Comm.Type = "winrm"

	errs := prepareSSH(t, c)

	var found bool
	for _, err := range errs {
		if strings.Contains(err.Error(), "winrm_username") {
			found = true
		}
	}
	if !found {
		t.Errorf("communicator = \"winrm\" must be told to set winrm_username, got %v", errs)
	}
}

// CA-431592: the SDK checks winrm_username and not winrm_password, so the plugin checks
// the password itself.
func TestSSHConfigPrepare_WinRMRequiresAWinRMPassword(t *testing.T) {
	c := &SSHConfig{}
	c.Comm.Type = "winrm"
	c.Comm.WinRMUser = "Administrator"

	errs := prepareSSH(t, c)

	var found bool
	for _, err := range errs {
		if strings.Contains(err.Error(), "winrm_password") {
			found = true
		}
	}
	if !found {
		t.Errorf("communicator = \"winrm\" must be told to set winrm_password, got %v", errs)
	}
}

// prepareComm reports what a communicator type ends up as after Prepare. The cases below
// are about the defaulting alone, so errors are ignored.
func prepareComm(t *testing.T, commType string) string {
	t.Helper()
	c := &SSHConfig{}
	c.Comm.Type = commType
	_ = prepareSSH(t, c)
	return c.Comm.Type
}

// The SDK defaults an unset communicator to "ssh"; this plugin defaults to "none", so a
// template that never said how to reach the guest is not asked for credentials to reach it.
func TestSSHConfigPrepare_UnsetCommunicatorDefaultsToNone(t *testing.T) {
	if got := prepareComm(t, ""); got != "none" {
		t.Errorf("unset communicator = %q, want %q", got, "none")
	}
}

// The point of defaulting to "none": no communicator, no credentials of any kind.
func TestSSHConfigPrepare_UnsetCommunicatorNeedsNoCredentials(t *testing.T) {
	c := &SSHConfig{}
	if errs := prepareSSH(t, c); len(errs) != 0 {
		t.Errorf("a template with no communicator should be accepted, got %v", errs)
	}
}

// A template that names a communicator must keep it. The default may only fill a gap.
func TestSSHConfigPrepare_AnExplicitCommunicatorIsKept(t *testing.T) {
	for _, want := range []string{"ssh", "winrm", "none"} {
		t.Run(want, func(t *testing.T) {
			if got := prepareComm(t, want); got != want {
				t.Errorf("communicator = %q survived Prepare as %q", want, got)
			}
		})
	}
}

// CA-431970 (requirement N3.3): the SSH credentials fall back to the environment, so an
// SSH build file need carry none of them.
func TestSSHConfigPrepare_SSHCredentialsFallBackToTheEnvironment(t *testing.T) {
	keyPath := writePrivateKey(t)

	c := &SSHConfig{}
	c.Comm.Type = "ssh"

	errs := prepareSSHWithEnv(t, c, map[string]string{
		EnvSSHUsername:       "builder",
		EnvSSHPassword:       "from-the-environment",
		EnvSSHPrivateKeyFile: keyPath,
	})

	if len(errs) > 0 {
		t.Fatalf("an ssh template whose credentials are all in the environment should be accepted: %v", errs)
	}
	if c.Comm.SSHUsername != "builder" {
		t.Errorf("SSHUsername = %q, want it from %s", c.Comm.SSHUsername, EnvSSHUsername)
	}
	if c.Comm.SSHPassword != "from-the-environment" {
		t.Errorf("SSHPassword = %q, want it from %s", c.Comm.SSHPassword, EnvSSHPassword)
	}
	if c.Comm.SSHPrivateKeyFile != keyPath {
		t.Errorf("SSHPrivateKeyFile = %q, want it from %s", c.Comm.SSHPrivateKeyFile, EnvSSHPrivateKeyFile)
	}
}

// The other half of N3.3: the same for a WinRM build.
func TestSSHConfigPrepare_WinRMCredentialsFallBackToTheEnvironment(t *testing.T) {
	c := &SSHConfig{}
	c.Comm.Type = "winrm"

	errs := prepareSSHWithEnv(t, c, map[string]string{
		EnvWinRMUsername: "Administrator",
		EnvWinRMPassword: "from-the-environment",
	})

	if len(errs) > 0 {
		t.Fatalf("a winrm template whose credentials are all in the environment should be accepted: %v", errs)
	}
	if c.Comm.WinRMUser != "Administrator" {
		t.Errorf("WinRMUser = %q, want it from %s", c.Comm.WinRMUser, EnvWinRMUsername)
	}
	if c.Comm.WinRMPassword != "from-the-environment" {
		t.Errorf("WinRMPassword = %q, want it from %s", c.Comm.WinRMPassword, EnvWinRMPassword)
	}
}

// A credential the template names is the build file's decision; an exported variable must
// not redirect the build to another account. This is the behaviour XENSERVER_PASSWORD
// already has, held to for the guest credentials too.
func TestSSHConfigPrepare_TemplateGuestCredentialsWinOverTheEnvironment(t *testing.T) {
	keyPath := writePrivateKey(t)

	c := &SSHConfig{}
	c.Comm.Type = "ssh"
	c.Comm.SSHUsername = "from-the-template"
	c.Comm.SSHPassword = "template-password"
	c.Comm.SSHPrivateKeyFile = keyPath

	errs := prepareSSHWithEnv(t, c, map[string]string{
		EnvSSHUsername:       "wrong",
		EnvSSHPassword:       "wrong",
		EnvSSHPrivateKeyFile: filepath.Join(t.TempDir(), "does-not-exist"),
	})

	if len(errs) > 0 {
		t.Fatalf("the template's own credentials should be accepted: %v", errs)
	}
	if c.Comm.SSHUsername != "from-the-template" {
		t.Errorf("SSHUsername = %q, want the template's", c.Comm.SSHUsername)
	}
	if c.Comm.SSHPassword != "template-password" {
		t.Errorf("SSHPassword = %q, want the template's", c.Comm.SSHPassword)
	}
	if c.Comm.SSHPrivateKeyFile != keyPath {
		t.Errorf("SSHPrivateKeyFile = %q, want the template's", c.Comm.SSHPrivateKeyFile)
	}
}

// The WinRM half of the same rule, kept separate because a config is one communicator or
// the other and the SDK only validates the set in use.
func TestSSHConfigPrepare_TemplateWinRMCredentialsWinOverTheEnvironment(t *testing.T) {
	c := &SSHConfig{}
	c.Comm.Type = "winrm"
	c.Comm.WinRMUser = "from-the-template"
	c.Comm.WinRMPassword = "template-password"

	errs := prepareSSHWithEnv(t, c, map[string]string{
		EnvWinRMUsername: "wrong",
		EnvWinRMPassword: "wrong",
	})

	if len(errs) > 0 {
		t.Fatalf("the template's own credentials should be accepted: %v", errs)
	}
	if c.Comm.WinRMUser != "from-the-template" {
		t.Errorf("WinRMUser = %q, want the template's", c.Comm.WinRMUser)
	}
	if c.Comm.WinRMPassword != "template-password" {
		t.Errorf("WinRMPassword = %q, want the template's", c.Comm.WinRMPassword)
	}
}

// The fallback has to land before c.Comm.Prepare, which is what reports the missing
// credential -- a value filled in afterwards would arrive too late to stop the error.
func TestSSHConfigPrepare_EnvironmentCredentialsSatisfyTheRequiredChecks(t *testing.T) {
	t.Run("ssh_username", func(t *testing.T) {
		c := &SSHConfig{}
		c.Comm.Type = "ssh"

		errs := prepareSSHWithEnv(t, c, map[string]string{EnvSSHUsername: "builder"})

		if errorsContain(errs, "ssh_username") {
			t.Errorf("%s should satisfy the ssh_username check, got %v", EnvSSHUsername, errs)
		}
	})

	t.Run("winrm_username", func(t *testing.T) {
		c := &SSHConfig{}
		c.Comm.Type = "winrm"

		errs := prepareSSHWithEnv(t, c, map[string]string{
			EnvWinRMUsername: "Administrator",
			EnvWinRMPassword: "secret",
		})

		if errorsContain(errs, "winrm_username") {
			t.Errorf("%s should satisfy the winrm_username check, got %v", EnvWinRMUsername, errs)
		}
	})

	t.Run("winrm_password", func(t *testing.T) {
		c := &SSHConfig{}
		c.Comm.Type = "winrm"
		c.Comm.WinRMUser = "Administrator"

		errs := prepareSSHWithEnv(t, c, map[string]string{EnvWinRMPassword: "secret"})

		if errorsContain(errs, "winrm_password") {
			t.Errorf("%s should satisfy the winrm_password check, got %v", EnvWinRMPassword, errs)
		}
	})
}

// A build told to set winrm_password should be told where else it can come from, the way
// the connection errors already name their variable.
func TestSSHConfigPrepare_MissingWinRMPasswordNamesTheEnvironmentVariable(t *testing.T) {
	c := &SSHConfig{}
	c.Comm.Type = "winrm"
	c.Comm.WinRMUser = "Administrator"

	errs := prepareSSH(t, c)

	if !errorsContain(errs, EnvWinRMPassword) {
		t.Errorf("the winrm_password error should also name %s, got %v", EnvWinRMPassword, errs)
	}
}

// An exported SSH credential must not reach a WinRM build, or a pool-wide export would
// change what an unrelated template authenticates with. The SDK reads only the set
// belonging to the communicator in use, which is what makes the unconditional fallback safe.
func TestSSHConfigPrepare_SSHEnvironmentDoesNotAffectAWinRMBuild(t *testing.T) {
	c := &SSHConfig{}
	c.Comm.Type = "winrm"
	c.Comm.WinRMUser = "Administrator"
	c.Comm.WinRMPassword = "secret"

	errs := prepareSSHWithEnv(t, c, map[string]string{
		EnvSSHUsername: "builder",
		EnvSSHPassword: "ssh-password",
	})

	if len(errs) > 0 {
		t.Fatalf("an exported SSH credential should not disturb a winrm build: %v", errs)
	}
	if c.Comm.WinRMUser != "Administrator" || c.Comm.WinRMPassword != "secret" {
		t.Errorf("the WinRM credentials changed: user %q, password %q", c.Comm.WinRMUser, c.Comm.WinRMPassword)
	}
}
