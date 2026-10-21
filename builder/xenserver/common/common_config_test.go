// Copyright (c) 2026. Citrix Systems, Inc.

package common

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/packer-plugin-sdk/common"
	"github.com/hashicorp/packer-plugin-sdk/template/config"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"
)

func boolPtr(v bool) *bool { return &v }

// connectionEnvVars is every variable the connection options fall back to.
var connectionEnvVars = []string{EnvHost, EnvUsername, EnvPassword, EnvCACertPath, EnvInsecure}

// minimalConfig returns a CommonConfig that passes Prepare's required-field checks, so a
// test can focus on one field.
func minimalConfig(format string) *CommonConfig {
	return &CommonConfig{
		Username: "root",
		Password: "secret",
		HostIp:   "10.0.0.1",
		Format:   format,
		Insecure: boolPtr(true), // otherwise Prepare also demands ca_cert_path
	}
}

func prepare(t *testing.T, c *CommonConfig) ([]string, []error) {
	t.Helper()
	return prepareWithEnv(t, c, nil)
}

// prepareWithEnv runs Prepare with exactly the XENSERVER_* variables in env set and every
// other one cleared -- otherwise one left exported in a developer's shell would quietly
// satisfy the tests that check it is demanded.
func prepareWithEnv(t *testing.T, c *CommonConfig, env map[string]string) ([]string, []error) {
	t.Helper()
	for _, key := range connectionEnvVars {
		t.Setenv(key, env[key])
	}
	return c.Prepare(&common.PackerConfig{PackerBuildName: "test"})
}

// errorsContain reports whether any error mentions substring -- how these tests assert
// that a failure names the option the user has to fix.
func errorsContain(errs []error, substring string) bool {
	for _, err := range errs {
		if strings.Contains(err.Error(), substring) {
			return true
		}
	}
	return false
}

// XVA exports are always gzip-compressed, so "xva_compressed" is normalized away here --
// every consumer downstream of Prepare then only ever sees the four real formats.
func TestCommonConfigPrepare_NormalizesXvaCompressedToXva(t *testing.T) {
	c := minimalConfig("xva_compressed")

	warns, errs := prepare(t, c)

	if len(errs) > 0 {
		t.Fatalf("xva_compressed must remain an accepted input: %v", errs)
	}
	if c.Format != "xva" {
		t.Errorf("Format = %q, want it normalized to \"xva\"", c.Format)
	}

	// Same warn-on-use convention as guest_tools_iso, or nobody migrates off the alias.
	var found bool
	for _, w := range warns {
		if strings.Contains(w, "xva_compressed is deprecated") {
			found = true
		}
	}
	if !found {
		t.Errorf("expected a deprecation warning for xva_compressed, got %v", warns)
	}
}

func TestCommonConfigPrepare_FormatDefaultsToXva(t *testing.T) {
	c := minimalConfig("")

	warns, errs := prepare(t, c)

	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.Format != "xva" {
		t.Errorf("Format = %q, want \"xva\"", c.Format)
	}
	if len(warns) > 0 {
		t.Errorf("the default must not warn, got %v", warns)
	}
}

// Every other accepted format must survive Prepare untouched, and silently —
// normalization applies to the alias only.
func TestCommonConfigPrepare_OtherFormatsAreUnchanged(t *testing.T) {
	for _, format := range []string{"xva", "vdi_raw", "vdi_vhd", "none"} {
		c := minimalConfig(format)
		// format = "none" is only legal with a keep_vm that keeps the template.
		if format == "none" {
			c.KeepVM = "on_success"
		}

		warns, errs := prepare(t, c)

		if len(errs) > 0 {
			t.Errorf("format %q: unexpected errors: %v", format, errs)
			continue
		}
		if c.Format != format {
			t.Errorf("format %q was rewritten to %q", format, c.Format)
		}
		if len(warns) > 0 {
			t.Errorf("format %q must not warn, got %v", format, warns)
		}
	}
}

func TestCommonConfigPrepare_RejectsUnknownFormat(t *testing.T) {
	c := minimalConfig("tarball")

	if _, errs := prepare(t, c); len(errs) == 0 {
		t.Error("an unrecognised format must be rejected")
	}
}

// The enum is matched exactly, so a value that only looks right -- the wrong case, or one
// a copy-paste padded with spaces -- is not the format the user meant. Accepting either
// would mean silently picking a format nobody wrote, and the mistake is invisible until
// the export step. These pin the match as exact rather than assert it should be lenient.
func TestCommonConfigPrepare_RejectsFormatSpellingVariants(t *testing.T) {
	for _, format := range []string{"XVA", "Xva", " xva", "xva ", " xva ", "VDI_RAW"} {
		c := minimalConfig(format)

		_, errs := prepare(t, c)

		if !errorsContain(errs, "format") {
			t.Errorf("format = %q must be rejected by name, got %v", format, errs)
		}
	}
}

// Same exact match for keep_vm, plus the hyphen: "on-success" is the natural misspelling
// of an option whose other two values carry no separator at all.
func TestCommonConfigPrepare_RejectsKeepVMSpellingVariants(t *testing.T) {
	for _, keepVM := range []string{"Never", "NEVER", "on-success", "On_Success", " never ", "always "} {
		c := minimalConfig("xva")
		c.KeepVM = keepVM

		_, errs := prepare(t, c)

		if !errorsContain(errs, "keep_vm") {
			t.Errorf("keep_vm = %q must be rejected by name, got %v", keepVM, errs)
		}
	}
}

// Two ways of answering the same question -- silently preferring one would make the
// ignored setting look effective. The SDK's own HTTPConfig rejects the combination too.
func TestCommonConfigPrepare_RejectsBindAddressWithInterface(t *testing.T) {
	c := minimalConfig("xva")
	c.HTTPAddress = "10.0.0.5"
	c.HTTPInterface = "eth0"

	_, errs := prepare(t, c)

	if len(errs) == 0 {
		t.Fatal("http_bind_address and http_interface must not be accepted together")
	}
}

// http_bind_address is handed straight to the guest as {{ .HTTPIP }} and to net.Listen,
// so a typo must fail at prepare time rather than as an unreachable install much later.
func TestCommonConfigPrepare_RejectsANonAddressBindAddress(t *testing.T) {
	c := minimalConfig("xva")
	c.HTTPAddress = "http://10.0.0.5"

	if _, errs := prepare(t, c); len(errs) == 0 {
		t.Error("http_bind_address must be rejected when it is not an IP address")
	}
}

func TestCommonConfigPrepare_AcceptsAnAddressBindAddress(t *testing.T) {
	for _, addr := range []string{"10.0.0.5", "0.0.0.0", "::"} {
		c := minimalConfig("xva")
		c.HTTPAddress = addr

		if _, errs := prepare(t, c); len(errs) > 0 {
			t.Errorf("http_bind_address %q should be accepted: %v", addr, errs)
		}
	}
}

// HasVendorDeviceEnabled is the one tri-state reader whose unset value is true, so copying
// SecureBootEnabled's `!= nil && *p` shape silently turns the iso builder's default off.
// Only that builder may read it; the clone builder reads the pointer (CA-431569).
func TestCommonConfig_HasVendorDeviceDefaultsToOn(t *testing.T) {
	c := minimalConfig("xva")

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !c.HasVendorDeviceEnabled() {
		t.Error("has_vendor_device defaulted to off; Windows guests need it on")
	}
}

func TestCommonConfig_HasVendorDeviceHonoursAnExplicitValue(t *testing.T) {
	for _, want := range []bool{true, false} {
		c := minimalConfig("xva")
		c.HasVendorDevice = &want

		if _, errs := prepare(t, c); len(errs) > 0 {
			t.Errorf("has_vendor_device=%t: unexpected errors: %v", want, errs)
			continue
		}
		if got := c.HasVendorDeviceEnabled(); got != want {
			t.Errorf("HasVendorDeviceEnabled() = %t, want %t", got, want)
		}
	}
}

// The CD holding cd_files is uploaded as a new VDI, so it goes to vm_sr. Demanding an
// iso_sr sent it to what is normally a read-only ISO library, failing the build
// (CA-430809).
func TestCommonConfigPrepare_AcceptsCDFilesWithoutAnISOSR(t *testing.T) {
	c := minimalConfig("xva")
	c.CDFiles = []string{filepath.Join(t.TempDir(), "Autounattend.xml")}
	if err := os.WriteFile(c.CDFiles[0], []byte("<unattend/>"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
}

func TestCommonConfigPrepare_AllowsCDFilesWithAnISOSR(t *testing.T) {
	c := minimalConfig("xva")
	c.ISOSR = "ISO library"
	c.CDFiles = []string{filepath.Join(t.TempDir(), "Autounattend.xml")}
	if err := os.WriteFile(c.CDFiles[0], []byte("<unattend/>"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Errorf("unexpected errors: %v", errs)
	}
}

// shutdown_command runs through the guest communicator, so "none" has nothing to run it
// on. Falling through to a hard shutdown would corrupt the image it was meant to finalise.
func TestCommonConfigPrepare_RejectsShutdownCommandWithoutACommunicator(t *testing.T) {
	c := minimalConfig("xva")
	c.ShutdownCommand = "shutdown -h now"
	c.Comm.Type = "none"

	_, errs := prepare(t, c)

	if len(errs) == 0 {
		t.Fatal("shutdown_command with communicator = \"none\" must be rejected")
	}
}

func TestCommonConfigPrepare_AllowsShutdownCommandWithACommunicator(t *testing.T) {
	// An unset type never reaches Prepare: the builders call ApplyCommunicatorDefault,
	// which makes it "none", first.
	for _, commType := range []string{"ssh", "winrm"} {
		c := minimalConfig("xva")
		c.ShutdownCommand = "shutdown -h now"
		c.Comm.Type = commType

		if _, errs := prepare(t, c); len(errs) > 0 {
			t.Errorf("communicator %q: shutdown_command should be accepted: %v", commType, errs)
		}
	}
}

// format = "none" turns the build VM into the output and keep_vm = "never" destroys it.
// The pair is always a mistake, and the cost of finding out at run time is a whole install.
func TestCommonConfigPrepare_RejectsNoneFormatWithKeepVMNever(t *testing.T) {
	c := minimalConfig("none")
	c.KeepVM = "never"

	_, errs := prepare(t, c)

	if len(errs) == 0 {
		t.Fatal("format = \"none\" with keep_vm = \"never\" produces no output and must be rejected")
	}

	var found bool
	for _, err := range errs {
		if strings.Contains(err.Error(), "produces no output") {
			found = true
		}
	}
	if !found {
		t.Errorf("the error should explain that the build produces nothing, got: %v", errs)
	}
}

// format = "none" makes the build VM the output, so an unset keep_vm must not delete
// it. The default is format-aware: "on_success" here, "never" for every format that
// exports a file. Without this, the first thing anyone who wants "retain as a template"
// does is trip a validation error telling them to set an option they never asked about.
func TestCommonConfigPrepare_NoneFormatDefaultsKeepVMToOnSuccess(t *testing.T) {
	c := minimalConfig("none")

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("format = \"none\" on its own must be accepted: %v", errs)
	}
	if c.KeepVM != "on_success" {
		t.Errorf("keep_vm = %q, want \"on_success\" for format = \"none\"", c.KeepVM)
	}
}

// The rule is narrow: it must not catch a format = "none" build that does keep its
// template, which is the whole point of that format.
func TestCommonConfigPrepare_AllowsNoneFormatWhenTheTemplateIsKept(t *testing.T) {
	for _, keep := range []string{"always", "on_success"} {
		c := minimalConfig("none")
		c.KeepVM = keep

		if _, errs := prepare(t, c); len(errs) > 0 {
			t.Errorf("format = \"none\" with keep_vm = %q should be accepted: %v", keep, errs)
		}
	}
}

// keep_vm = "never" is the default and must stay perfectly fine for every format that
// actually exports a file.
func TestCommonConfigPrepare_AllowsKeepVMNeverForExportingFormats(t *testing.T) {
	for _, format := range []string{"xva", "vdi_raw", "vdi_vhd"} {
		c := minimalConfig(format)
		c.KeepVM = "never"

		if _, errs := prepare(t, c); len(errs) > 0 {
			t.Errorf("format %q with keep_vm = \"never\" should be accepted: %v", format, errs)
		}
	}
}

// A build with no communicator has no login, no provisioners and no shutdown_command.
// What follows from that differs per builder, so each builder's Prepare says its own half
// and only the shared half is asserted here.
func TestCommonConfigPrepare_WarnsThatACommunicatorlessBuildRunsNoProvisioner(t *testing.T) {
	c := minimalConfig("xva")
	c.Comm.Type = "none"

	warns, errs := prepare(t, c)

	if len(errs) > 0 {
		t.Fatalf("communicator = \"none\" on its own is valid: %v", errs)
	}
	if !anyContains(warns, "never logs in to the guest") || !anyContains(warns, "no provisioner runs") {
		t.Errorf("expected a warning that nothing logs in and no provisioner runs, got %v", warns)
	}
	if anyContains(warns, "install_timeout") {
		t.Errorf("the consequence is the builder's to state, not this one's: %v", warns)
	}
}

// The warning is for communicator = "none" only; a build that names a communicator must
// stay silent. An unset type never gets here: ApplyCommunicatorDefault makes it "none" first.
func TestCommonConfigPrepare_DoesNotWarnAboutAMissingCommunicator(t *testing.T) {
	for _, commType := range []string{"ssh", "winrm"} {
		c := minimalConfig("xva")
		c.Comm.Type = commType

		warns, _ := prepare(t, c)

		if anyContains(warns, "no provisioner runs") {
			t.Errorf("communicator %q: unexpected communicator-less warning: %v", commType, warns)
		}
	}
}

// The point of the fallback is that a build file need not contain a credential at all.
func TestCommonConfigPrepare_ConnectionOptionsFallBackToTheEnvironment(t *testing.T) {
	caCertPath := writeTestCACert(t)
	c := &CommonConfig{}

	_, errs := prepareWithEnv(t, c, map[string]string{
		EnvHost:       "pool.example.com",
		EnvUsername:   "root",
		EnvPassword:   "from-the-environment",
		EnvCACertPath: caCertPath,
	})

	if len(errs) > 0 {
		t.Fatalf("a config supplied entirely from the environment must be accepted: %v", errs)
	}
	if c.HostIp != "pool.example.com" {
		t.Errorf("HostIp = %q, want it from %s", c.HostIp, EnvHost)
	}
	if c.Username != "root" {
		t.Errorf("Username = %q, want it from %s", c.Username, EnvUsername)
	}
	if c.Password != "from-the-environment" {
		t.Errorf("Password = %q, want it from %s", c.Password, EnvPassword)
	}
	if c.CACertPath != caCertPath {
		t.Errorf("CACertPath = %q, want it from %s", c.CACertPath, EnvCACertPath)
	}
}

// The environment is a fallback, not an override: a variable exported for one build must
// not send another build's credentials to a host its template did not name.
func TestCommonConfigPrepare_TemplateValuesWinOverTheEnvironment(t *testing.T) {
	c := minimalConfig("xva")

	_, errs := prepareWithEnv(t, c, map[string]string{
		EnvHost:     "wrong.example.com",
		EnvUsername: "wrong",
		EnvPassword: "wrong",
	})

	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.HostIp != "10.0.0.1" || c.Username != "root" || c.Password != "secret" {
		t.Errorf("the environment overrode the template: host=%q user=%q password=%q",
			c.HostIp, c.Username, c.Password)
	}
}

// insecure is a pointer so that this case exists at all: an explicit `insecure = false` is
// a decision to verify, and no environment variable gets to reverse it.
func TestCommonConfigPrepare_ExplicitInsecureFalseSurvivesTheEnvironment(t *testing.T) {
	c := minimalConfig("xva")
	c.Insecure = boolPtr(false)
	c.CACertPath = writeTestCACert(t)

	_, errs := prepareWithEnv(t, c, map[string]string{EnvInsecure: "true"})

	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.InsecureEnabled() {
		t.Error("XENSERVER_INSECURE overrode an explicit insecure = false")
	}
}

func TestCommonConfigPrepare_InsecureFallsBackToTheEnvironment(t *testing.T) {
	c := &CommonConfig{Username: "root", Password: "secret", HostIp: "10.0.0.1"}

	// Without the fallback taking effect this fails on the missing ca_cert_path.
	_, errs := prepareWithEnv(t, c, map[string]string{EnvInsecure: "1"})

	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !c.InsecureEnabled() {
		t.Error("XENSERVER_INSECURE=1 did not turn verification off")
	}
}

// The environment fills in what the template left unsaid; it does not get to reverse a
// decision the template made in other words. Pinning a certificate is such a decision.
func TestCommonConfigPrepare_EnvironmentInsecureDoesNotDefeatAPinnedCertificate(t *testing.T) {
	c := minimalConfig("xva")
	c.Insecure = nil
	c.CACertPath = writeTestCACert(t)

	warns, errs := prepareWithEnv(t, c, map[string]string{EnvInsecure: "true"})

	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.InsecureEnabled() {
		t.Error("XENSERVER_INSECURE turned off verification for a build that pinned a certificate")
	}
	if !anyContains(warns, EnvInsecure) {
		t.Errorf("ignoring %s must be said out loud, got %v", EnvInsecure, warns)
	}
}

// The other half of the rule: with nothing pinned there is no decision to
// reverse, so the variable applies and says nothing.
func TestCommonConfigPrepare_EnvironmentInsecureAppliesWithNoCertificatePinned(t *testing.T) {
	c := minimalConfig("xva")
	c.Insecure = nil

	warns, errs := prepareWithEnv(t, c, map[string]string{EnvInsecure: "true"})

	if len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if !c.InsecureEnabled() {
		t.Error("XENSERVER_INSECURE=true did not turn verification off")
	}
	if len(warns) > 0 {
		t.Errorf("unexpected warnings: %v", warns)
	}
}

// Guessing at a non-boolean would turn a typo into a silently unverified connection.
func TestCommonConfigPrepare_RejectsANonBooleanInsecureEnvironmentValue(t *testing.T) {
	c := minimalConfig("xva")
	c.Insecure = nil

	_, errs := prepareWithEnv(t, c, map[string]string{EnvInsecure: "yes-please"})

	if !errorsContain(errs, EnvInsecure) {
		t.Errorf("a non-boolean %s must be rejected by name, got %v", EnvInsecure, errs)
	}
}

// A value that arrives from the environment is not more trustworthy than one written in
// the template, so it has to meet the same checks. This is ordering, not parsing:
// applyConnectionEnv runs before validateHost today, and if it ever moved after it an
// XENSERVER_HOST of http:// would put the login and the pool-wide session reference on a
// plaintext channel -- with every other test still green, because they all supply a legal
// host.
func TestCommonConfigPrepare_RejectsAnInvalidHostFromTheEnvironment(t *testing.T) {
	c := minimalConfig("xva")
	c.HostIp = ""

	_, errs := prepareWithEnv(t, c, map[string]string{EnvHost: "http://10.0.0.1"})

	if !errorsContain(errs, "xenserver_host") {
		t.Errorf("an http:// %s must be rejected by name, got %v", EnvHost, errs)
	}
}

// The same ordering argument for the certificate: a path that came from the environment is
// still loaded by Prepare, or the goSDK's log.Fatal takes the plugin down mid-build.
func TestCommonConfigPrepare_RejectsAnUnusableCACertFromTheEnvironment(t *testing.T) {
	notPEM := filepath.Join(t.TempDir(), "not-a-cert.pem")
	if err := os.WriteFile(notPEM, []byte("this is not a certificate\n"), 0600); err != nil {
		t.Fatalf("could not write the fixture: %s", err)
	}

	for name, path := range map[string]string{
		"missing":   filepath.Join(t.TempDir(), "absent.pem"),
		"not a PEM": notPEM,
	} {
		c := minimalConfig("xva")
		c.Insecure = nil
		c.CACertPath = ""

		_, errs := prepareWithEnv(t, c, map[string]string{EnvCACertPath: path})

		if !errorsContain(errs, "ca_cert_path") {
			t.Errorf("a %s %s must be rejected at prepare time, got %v", name, EnvCACertPath, errs)
		}
	}
}

// Verification is the default, so a build that says nothing about certificates has to be
// told what to trust rather than quietly trusting nothing or everything.
func TestCommonConfigPrepare_RequiresACertificateWhenVerifying(t *testing.T) {
	c := minimalConfig("xva")
	c.Insecure = nil

	_, errs := prepare(t, c)

	if !errorsContain(errs, "ca_cert_path") {
		t.Fatalf("a verifying build with no certificate must be rejected, got %v", errs)
	}
	// A user hitting this needs to know the option can come from the environment.
	if !errorsContain(errs, EnvCACertPath) {
		t.Errorf("the error should also name %s, got %v", EnvCACertPath, errs)
	}
}

func TestCommonConfigPrepare_AcceptsAReadableCACert(t *testing.T) {
	c := minimalConfig("xva")
	c.Insecure = nil
	c.CACertPath = writeTestCACert(t)

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Errorf("a readable PEM certificate should be accepted: %v", errs)
	}
}

// The goSDK calls log.Fatal on a CA certificate it cannot read or parse, taking the plugin
// process down with no Packer-level error, so Prepare has to reject both up front.
func TestCommonConfigPrepare_RejectsAnUnusableCACert(t *testing.T) {
	notPEM := filepath.Join(t.TempDir(), "not-a-cert.pem")
	if err := os.WriteFile(notPEM, []byte("this is not a certificate\n"), 0600); err != nil {
		t.Fatalf("could not write the fixture: %s", err)
	}

	// An empty file and a private key are the two ways this option goes wrong in practice:
	// `xe host-get-server-certificate` redirected into a file that was never written, and
	// the key half of a pair handed over in place of the certificate. Both are readable, so
	// only the PEM parse can catch them.
	empty := filepath.Join(t.TempDir(), "empty.pem")
	if err := os.WriteFile(empty, nil, 0600); err != nil {
		t.Fatalf("could not write the fixture: %s", err)
	}

	for name, path := range map[string]string{
		"missing":       filepath.Join(t.TempDir(), "absent.pem"),
		"not a PEM":     notPEM,
		"empty":         empty,
		"a private key": writeTestPrivateKeyPEM(t),
	} {
		c := minimalConfig("xva")
		c.Insecure = nil
		c.CACertPath = path

		_, errs := prepare(t, c)

		if !errorsContain(errs, "ca_cert_path") {
			t.Errorf("a %s ca_cert_path must be rejected at prepare time, got %v", name, errs)
		}
	}
}

func anyContains(values []string, substr string) bool {
	for _, v := range values {
		if strings.Contains(v, substr) {
			return true
		}
	}
	return false
}

// The session carries the login and then the session reference, a bearer token for the
// whole pool. Neither may travel in the clear, and asking for that is not quietly upgraded.
func TestCommonConfigPrepare_RejectsAPlaintextHost(t *testing.T) {
	for _, host := range []string{"http://10.0.0.1", "HTTP://10.0.0.1", "http://pool.example.com:80"} {
		c := minimalConfig("xva")
		c.HostIp = host

		_, errs := prepare(t, c)

		if !errorsContain(errs, "xenserver_host") {
			t.Errorf("xenserver_host = %q must be rejected, got %v", host, errs)
		}
	}
}

func TestCommonConfigPrepare_AcceptsHTTPSAndBareHosts(t *testing.T) {
	for _, host := range []string{"10.0.0.1", "pool.example.com", "https://pool.example.com", "HTTPS://10.0.0.1"} {
		c := minimalConfig("xva")
		c.HostIp = host

		if _, errs := prepare(t, c); len(errs) > 0 {
			t.Errorf("xenserver_host = %q should be accepted: %v", host, errs)
		}
	}
}

func TestCommonConfigPrepare_RejectsANonHTTPScheme(t *testing.T) {
	c := minimalConfig("xva")
	c.HostIp = "ssh://10.0.0.1"

	if _, errs := prepare(t, c); !errorsContain(errs, "xenserver_host") {
		t.Errorf("an unrecognised scheme must be rejected, got %v", errs)
	}
}

// Prepare runs after the SDK has interpolated the template's own values, and nothing
// renders the default afterwards: a literal "{{timestamp}}" reached XAPI as the VM's
// name-label, so every build that omitted vm_name collided with the last one.
func TestCommonConfigPrepare_DefaultVMNameHasARenderedTimestamp(t *testing.T) {
	c := minimalConfig("xva")

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("should not have errors: %v", errs)
	}

	if strings.Contains(c.Name(), "{{") {
		t.Fatalf("vm_name = %q still carries an un-rendered template", c.Name())
	}
	want := fmt.Sprintf("packer-test-%d", interpolate.InitTime.Unix())
	if c.Name() != want {
		t.Errorf("vm_name = %q, want %q", c.Name(), want)
	}
}

// A vm_name the template did set is interpolated by the SDK before Prepare ever sees it,
// so Prepare must leave it exactly alone.
func TestCommonConfigPrepare_AnExplicitVMNameIsLeftAlone(t *testing.T) {
	c := minimalConfig("xva")
	name := "my-golden-image"
	c.VMName = &name

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("should not have errors: %v", errs)
	}
	if c.Name() != "my-golden-image" {
		t.Errorf("vm_name = %q, want it untouched", c.Name())
	}
}

// A negative boot_key_groups makes StepTypeBootCommand send the boot command zero times,
// so the unattended install never starts.
func TestCommonConfigPrepare_RejectsANegativeBootKeyGroups(t *testing.T) {
	c := minimalConfig("xva")
	c.BootKeyGroups = -1

	_, errs := prepare(t, c)

	if len(errs) == 0 {
		t.Fatal("boot_key_groups = -1 must be rejected at validate time")
	}
}

// No username and no XENSERVER_USERNAME to fall back on: the error must name both ways to
// supply one.
func TestCommonConfigPrepare_RequiresAUsername(t *testing.T) {
	c := minimalConfig("xva")
	c.Username = ""

	_, errs := prepare(t, c)

	if !errorsContain(errs, "xenserver_username") {
		t.Fatalf("a build with no xenserver_username must be rejected, got %v", errs)
	}
	if !errorsContain(errs, EnvUsername) {
		t.Errorf("the error should also name %s, got %v", EnvUsername, errs)
	}
}

// The password is the option most worth keeping out of a build file, but a build still
// needs one somewhere.
func TestCommonConfigPrepare_RequiresAPassword(t *testing.T) {
	c := minimalConfig("xva")
	c.Password = ""

	_, errs := prepare(t, c)

	if !errorsContain(errs, "xenserver_password") {
		t.Fatalf("a build with no xenserver_password must be rejected, got %v", errs)
	}
	if !errorsContain(errs, EnvPassword) {
		t.Errorf("the error should also name %s, got %v", EnvPassword, errs)
	}
}

// An empty host never reaches validateHost, so it has its own error -- which still has to
// name the environment fallback.
func TestCommonConfigPrepare_RequiresAHost(t *testing.T) {
	c := minimalConfig("xva")
	c.HostIp = ""

	_, errs := prepare(t, c)

	if !errorsContain(errs, "xenserver_host") {
		t.Fatalf("a build with no xenserver_host must be rejected, got %v", errs)
	}
	if !errorsContain(errs, EnvHost) {
		t.Errorf("the error should also name %s, got %v", EnvHost, errs)
	}
}

// insecure = true is a decision to skip verification, so ca_cert_path is neither required
// nor read: a build can carry a stale or missing one and Prepare still accepts it.
func TestCommonConfigPrepare_InsecureSkipsCACertValidation(t *testing.T) {
	c := minimalConfig("xva") // minimalConfig already sets insecure = true
	c.CACertPath = filepath.Join(t.TempDir(), "does-not-exist.pem")

	_, errs := prepare(t, c)

	if len(errs) > 0 {
		t.Fatalf("insecure = true must not require or validate ca_cert_path: %v", errs)
	}
	if !c.InsecureEnabled() {
		t.Error("insecure = true was not honoured")
	}
}

// The builders decode boot_wait, but the 5s default is applied here. In nanoseconds
// because this file does not import "time".
func TestCommonConfigPrepare_BootWaitDefaultsToFiveSeconds(t *testing.T) {
	const fiveSeconds = 5 * 1000 * 1000 * 1000

	c := minimalConfig("xva")

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.BootWait != fiveSeconds {
		t.Errorf("boot_wait = %d, want %d (5s)", c.BootWait, fiveSeconds)
	}
}

// The builders have their own TestBuilderPrepare_InstallTimeoutDefault, but the 200m
// default lives here. In nanoseconds because this file does not import "time".
func TestCommonConfigPrepare_InstallTimeoutDefaultsTo200Minutes(t *testing.T) {
	const twoHundredMinutes = 200 * 60 * 1000 * 1000 * 1000

	c := minimalConfig("xva")

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.InstallTimeout != twoHundredMinutes {
		t.Errorf("install_timeout = %s, want 200m", c.InstallTimeout)
	}
}

// boot_key_group_interval has no default and no validation -- typeCommands reads it at run
// time -- so it only has to survive Prepare.
func TestCommonConfigPrepare_BootKeyGroupIntervalIsPreserved(t *testing.T) {
	const interval = 1500 * 1000 * 1000 // 1.5s

	c := minimalConfig("xva")
	c.BootKeyGroupInterval = interval

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.BootKeyGroupInterval != interval {
		t.Errorf("boot_key_group_interval = %s, want 1.5s; Prepare must leave it untouched", c.BootKeyGroupInterval)
	}
}

// http_directory is passed straight through to StepHTTPServer: Prepare neither defaults
// nor validates it.
func TestCommonConfigPrepare_HTTPDirectoryIsPreserved(t *testing.T) {
	c := minimalConfig("xva")
	c.HTTPDir = "http"

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.HTTPDir != "http" {
		t.Errorf("http_directory = %q, want it left as %q", c.HTTPDir, "http")
	}
}

// Only http_interface and http_bind_address *together* are rejected; on its own it has no
// further validation.
func TestCommonConfigPrepare_AcceptsHTTPInterfaceAlone(t *testing.T) {
	c := minimalConfig("xva")
	c.HTTPInterface = "eth0"

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Errorf("http_interface on its own must be accepted: %v", errs)
	}
	if c.HTTPInterface != "eth0" {
		t.Errorf("http_interface = %q, want it preserved", c.HTTPInterface)
	}
}

// keep_vm feeds ShouldKeepVM's switch, whose default panics, so an unrecognised value has
// to be caught at validate time rather than mid-build.
func TestCommonConfigPrepare_RejectsUnknownKeepVM(t *testing.T) {
	c := minimalConfig("xva")
	c.KeepVM = "sometimes"

	_, errs := prepare(t, c)

	if !errorsContain(errs, "keep_vm must be one of") {
		t.Errorf("an unrecognised keep_vm must be rejected, got %v", errs)
	}
}

// The "never" default is what makes an exporting build clean up its VM. It applies to
// every format except "none", which defaults to "on_success" instead.
func TestCommonConfigPrepare_KeepVMDefaultsToNever(t *testing.T) {
	c := minimalConfig("xva")

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.KeepVM != "never" {
		t.Errorf("keep_vm = %q, want the default \"never\"", c.KeepVM)
	}
}

// An unset output_directory defaults to output-<build_name>. prepareWithEnv builds with
// PackerBuildName "test", so the default is deterministically "output-test".
func TestCommonConfigPrepare_OutputDirDefaultsToOutputBuildName(t *testing.T) {
	c := minimalConfig("xva")

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.OutputDir != "output-test" {
		t.Errorf("output_directory = %q, want the default \"output-test\" (output-<build_name>)", c.OutputDir)
	}
}

// Prepare only fills the default when the value is empty.
func TestCommonConfigPrepare_OutputDirIsPreservedWhenSet(t *testing.T) {
	c := minimalConfig("xva")
	c.OutputDir = "my-artifacts"

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if c.OutputDir != "my-artifacts" {
		t.Errorf("output_directory = %q, want it left as the value set (\"my-artifacts\")", c.OutputDir)
	}
}

// The intended pairing: neither field is defaulted or validated, so both survive Prepare.
func TestCommonConfigPrepare_CreateSnapshotWithSnapshotNameIsAccepted(t *testing.T) {
	c := minimalConfig("xva")
	c.CreateSnapshot = true
	c.SnapshotName = "pre-sysprep"

	if _, errs := prepare(t, c); len(errs) > 0 {
		t.Fatalf("create_snapshot with a snapshot_name must be accepted: %v", errs)
	}
	if !c.CreateSnapshot {
		t.Error("create_snapshot was cleared by Prepare")
	}
	if c.SnapshotName != "pre-sysprep" {
		t.Errorf("snapshot_name = %q, want it preserved as \"pre-sysprep\"", c.SnapshotName)
	}
}

// A snapshot_name that will never be used points at a mistake, and is usually set because
// something downstream consumes the snapshot -- which would otherwise find it missing after
// the build rather than at validate time. Rejected rather than warned, to match the other
// two options that cannot take effect on their own: format = "none" with keep_vm = "never",
// and network_names = [].
func TestCommonConfigPrepare_RejectsSnapshotNameWithoutCreateSnapshot(t *testing.T) {
	c := minimalConfig("xva")
	c.CreateSnapshot = false
	c.SnapshotName = "pre-sysprep"

	_, errs := prepare(t, c)

	if !errorsContain(errs, "snapshot_name") {
		t.Errorf("snapshot_name set without create_snapshot should be rejected, got %v", errs)
	}
}

// cd_files names local files, so a path that does not exist is worth catching before the
// CD upload.
func TestCommonConfigPrepare_RejectsANonExistentCDFile(t *testing.T) {
	c := minimalConfig("xva")
	// t.TempDir() exists but the file inside it is never created.
	c.CDFiles = []string{filepath.Join(t.TempDir(), "Autounattend.xml")}

	_, errs := prepare(t, c)

	if !errorsContain(errs, "does not exist") {
		t.Errorf("a cd_files entry that does not exist should be rejected, got %v", errs)
	}
}

func TestCommonConfigDecode_TellsAnEmptyNetworkNamesFromAnAbsentOne(t *testing.T) {
	var absent, empty CommonConfig

	if err := config.Decode(&absent, nil, map[string]interface{}{}); err != nil {
		t.Fatal(err)
	}
	if absent.NetworkNames != nil {
		t.Errorf("an absent network_names decoded to %#v, want nil", absent.NetworkNames)
	}

	if err := config.Decode(&empty, nil, map[string]interface{}{"network_names": []interface{}{}}); err != nil {
		t.Fatal(err)
	}
	if empty.NetworkNames == nil || len(empty.NetworkNames) != 0 {
		t.Errorf("an explicit network_names = [] decoded to %#v, want a non-nil empty slice", empty.NetworkNames)
	}
}

func TestCommonConfigPrepare_RejectsAnEmptyNetworkNames(t *testing.T) {
	c := minimalConfig("xva")
	c.NetworkNames = []string{}

	_, errs := prepare(t, c)

	if !errorsContain(errs, "network_names") {
		t.Errorf("network_names = [] should be rejected, got %v", errs)
	}
}

func TestCommonConfigPrepare_AcceptsAnAbsentNetworkNames(t *testing.T) {
	c := minimalConfig("xva")

	_, errs := prepare(t, c)

	if len(errs) > 0 {
		t.Errorf("an absent network_names should be accepted, got %v", errs)
	}
}

func TestCommonConfigPrepare_AcceptsANamedNetwork(t *testing.T) {
	c := minimalConfig("xva")
	c.NetworkNames = []string{"Pool-wide network associated with eth0"}

	_, errs := prepare(t, c)

	if len(errs) > 0 {
		t.Errorf("a named network should be accepted, got %v", errs)
	}
}

func TestCommonConfigPrepare_RejectsAnEmptyNetworkNamesEntry(t *testing.T) {
	for _, name := range []string{"", " "} {
		c := minimalConfig("xva")
		c.NetworkNames = []string{"Pool-wide network associated with eth0", name}

		_, errs := prepare(t, c)

		if !errorsContain(errs, "network_names[1]") {
			t.Errorf("network_names entry %q should be rejected, got %v", name, errs)
		}
	}
}
