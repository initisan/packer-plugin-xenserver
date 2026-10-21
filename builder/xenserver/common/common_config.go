// Copyright (c) 2026. Citrix Systems, Inc.

//go:generate packer-sdc struct-markdown
package common

import (
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/hashicorp/packer-plugin-sdk/common"
	"github.com/hashicorp/packer-plugin-sdk/multistep"
	packersdk "github.com/hashicorp/packer-plugin-sdk/packer"
	"github.com/hashicorp/packer-plugin-sdk/template/interpolate"

	"xenapi"
)

// BytesPerGB converts the GB-denominated size options (`vm_memory_gb`,
// `disk_size_gb`) to the bytes XAPI expects.
const BytesPerGB = 1024 * 1024 * 1024

// CommonConfig holds the options the two builders share. Each builder embeds it
// with `mapstructure:",squash"` into its own config struct and adds its own
// fields there, so every option here appears in both builders' generated HCL2
// specs -- including `iso_sr`, which only the iso builder reads.
type CommonConfig struct {
	// The XenServer username used to access the pool.
	Username string `mapstructure:"xenserver_username" required:"true"`
	// The XenServer password used to access the pool.
	Password string `mapstructure:"xenserver_password" required:"true"`
	// The address of the XenServer / XCP-ng pool primary (master) to connect
	// to.
	HostIp string `mapstructure:"xenserver_host" required:"true"`

	// The name of the new VM, without a file extension. Defaults to
	// `packer-{{build_name}}-{{timestamp}}`. The empty string is not a name and
	// is rejected: omit the option to get the default.
	VMName *string `mapstructure:"vm_name"`
	// The description of the new VM. Defaults to the empty string.
	VMDescription string `mapstructure:"vm_description"`
	// The name-label of the storage repository (SR) to use for the VM's disks.
	// Defaults to the pool's default SR.
	//
	// On the clone builder that default covers what the build uploads, not the
	// clone's own disks: left unset, they stay on the SR the source's are on,
	// because `VM.clone` cannot place them. Setting this to an SR the source's
	// disks are not already on makes the build copy every one of them in full
	// instead of cloning it copy-on-write, which is what `VM.copy` does to take
	// a target SR, so expect it to take as long as writing that much data. A
	// source whose disks are spread across SRs is gathered onto this one.
	//
	// Every VDI the build uploads — the CD built from `cd_files`, and an ISO
	// fetched from `iso_url` — is created here too, because writing a VDI needs
	// a writable SR.
	VMSR string `mapstructure:"vm_sr"`
	// The name-label of the storage repository (SR) the install ISO named by
	// `iso_name` is looked up in. Nothing is ever written to it, so it can be a
	// read-only ISO library.
	ISOSR string `mapstructure:"iso_sr"`
	// A list of files placed in the root of a CD-ROM attached when the VM
	// boots. Useful for unattended Windows installs (an `Autounattend.xml`).
	// Works with both BIOS and UEFI boot.
	CDFiles []string `mapstructure:"cd_files"`
	// A list of network name-labels to attach to the VM, in order. The first
	// entry becomes the VM's first VIF, the second the second VIF, and so on.
	// Setting it means exactly these NICs and no others, so the VIFs inherited
	// from the template are destroyed first. Left unset, the template's NICs are
	// kept, and the pool's management network is attached only when the template
	// gave the VM none. An explicit empty list is rejected, as is an entry that
	// names no network: omit the option instead.
	NetworkNames []string `mapstructure:"network_names"`

	// The number of vCPUs to assign to the VM. It sets both the VM's maximum
	// vCPU count and its vCPU count at startup. Unset, both builders keep the
	// value of the source they clone; it has no default, so the iso builder
	// requires it on the generic `Other install media` template, which has no
	// value to keep. `0` is not a vCPU count and is rejected: omit the option
	// instead.
	VCPUs *uint `mapstructure:"vcpus"`
	// The number of cores per socket for the VM. Both builders apply it only
	// when `vcpus` is set too, and otherwise keep the source's topology. The
	// one exception is a source topology that does not fit the `vcpus` the
	// build asked for -- it has to divide them exactly, and give no more than
	// 16 sockets -- which XenServer will not run: that VM is given the fewest
	// cores per socket that does fit instead. A `cores_per_socket` the build
	// sets itself is held to the same two rules at validate time. `0` is
	// rejected: omit the option instead.
	CorePerSocket *uint `mapstructure:"cores_per_socket"`
	// The amount of RAM to assign to the VM, in gigabytes. Unset, both builders
	// keep the value of the source they clone; it has no default, so the iso
	// builder requires it on the generic `Other install media` template, which
	// has no value to keep. `0` is not an amount of RAM and is rejected: omit
	// the option instead.
	VMMemoryGB *uint `mapstructure:"vm_memory_gb"`
	// The size of the VM's primary disk, in gigabytes. The iso builder creates
	// the disk at this size; the clone builder grows the source's disk to it.
	// It has no default. Unset, the iso builder takes the size `clone_template`
	// asks for -- its provisioning disks, or failing those the disk it carries
	// on device 0 -- and requires the option on the generic `Other install
	// media` template, which has neither; the clone builder leaves the disk
	// alone.
	//
	// It sizes only the disk on device 0, and fails if the source has none
	// there. A disk can only be grown, so a value below the source's own disk
	// size fails the build rather than producing an image at the wrong size.
	// `0` is not a disk size and is rejected: omit the option instead.
	DiskSizeGB *uint `mapstructure:"disk_size_gb"`
	// Key/value pairs written to the VM's XAPI `other-config`. The plugin writes
	// `xs_packer_plugin` and `xs_packer_plugin_version` after this map, so an
	// entry using either of those key names is silently overridden by the
	// plugin's own value.
	VMOtherConfig map[string]string `mapstructure:"vm_other_config"`
	// The vGPU profile (type) to attach for accelerated graphics.
	VGPUProfile string `mapstructure:"vgpu_profile"`

	// The VM firmware to use: `bios` or `uefi`. Defaults to the firmware of the
	// template the VM is cloned from -- both builders clone, and neither writes
	// this boot parameter unless the build sets it. A template that carries no
	// firmware at all is a BIOS VM, which is what XenServer's generic install
	// templates are.
	//
	// Setting it is an override, not a request: it wins over the source's own
	// firmware, and `VM.start` accepts the result either way. Where the source
	// cannot in fact boot that way, the build is refused before it starts rather
	// than left waiting for a guest that never comes up. That covers a source
	// whose boot mode is already settled -- one that has run under the other
	// firmware, or was cloned or imported from one that has -- and, for
	// `firmware = "bios"`, a source carrying a vTPM, which XenServer implements
	// for UEFI guests only.
	//
	// Both the refusal and the write belong to starting the VM, so a clone build
	// that never starts one -- `communicator = "none"` with no `cd_files` and no
	// `boot_command` -- neither checks the source nor writes the boot parameter,
	// and exports the clone with the firmware it inherited. Setting it is still
	// required there by `secure_boot` and `vtpm`, which are applied to the clone
	// whether or not it boots.
	Firmware string `mapstructure:"firmware"`
	// Enable UEFI Secure Boot. `true` requires `firmware = "uefi"`, which must
	// be set explicitly, and is written as XAPI's `auto` -- "on where the pool
	// has certificates to enforce it with". `VM.start` resolves that against
	// the UEFI certificates the pool actually has, so on a pool with none the
	// VM starts with Secure Boot off.
	//
	// Omit it to keep whatever the template the VM is cloned from has, which is
	// why this is a pointer: an unset option and an explicit `false` have to be
	// told apart. There is no value meaning "unset" to write, so keeping the
	// source's is done by writing nothing -- `false` is a real setting, and
	// writing it over a Secure Boot template is what turns Secure Boot off.
	SecureBoot *bool `mapstructure:"secure_boot"`
	// Give the VM the emulated PCI device through which Windows offers the
	// XenServer PV drivers on Windows Update. A Windows guest needs it for a
	// clean shutdown, reboot and suspend; a Linux guest is PV-native and does
	// not use it.
	//
	// A VM inherits the setting from whatever it was cloned from, and the plugin
	// writes the flag only when it differs from the source, because XenServer
	// warns that changing it on a VM that already has Windows installed is
	// likely to stop the guest booting. What leaving it unset means therefore
	// differs by builder. The iso builder defaults it to true: it installs the
	// guest OS itself, and the generic `Other install media` template has the
	// device off, so a Windows build left alone would come out without it. The
	// clone builder keeps the source template's setting, because that source
	// already has the OS on it.
	HasVendorDevice *bool `mapstructure:"has_vendor_device"`

	// An array of commands typed over VNC when the VM first boots, used to
	// start the OS installer. See the Boot Command section for details.
	BootCommand []string `mapstructure:"boot_command"`
	// The command used to gracefully shut the VM down once provisioning is
	// complete. Defaults to a clean shutdown via the XenAPI.
	ShutdownCommand string `mapstructure:"shutdown_command"`

	// The time to wait after boot before typing the `boot_command` (for example
	// `5s` or `1m30s`). Defaults to 5s.
	BootWait time.Duration `mapstructure:"boot_wait"`
	// How many times to repeat the full `boot_command` sequence. Use this with
	// `boot_key_group_interval` to cover a boot prompt that appears at an
	// unpredictable offset — for example the UEFI "Press any key to boot from
	// CD" window, which is only open for ≈6 s.
	//
	// Defaults to 1 (send once), except on `xenserver-vm-iso` with a UEFI guest
	// and a `boot_command` of a single `<spacebar>`, where it defaults to 20
	// sends 1 s apart to cover that window. Every other command is sent once:
	// each repetition lands in whatever the first one started, so a key that
	// chooses, a typed character and a `<wait>` are all left alone.
	BootKeyGroups int `mapstructure:"boot_key_groups"`
	// How long to wait between successive `boot_command` repetitions, for
	// example `"1s"`. Required with a `boot_key_groups` above 1, which is
	// otherwise sent all at once and covers no more than a single send.
	// Defaults to 0, or to 1 s when `boot_key_groups` takes its UEFI iso
	// default; naming one there replaces the 1 s and keeps the 20 repetitions.
	BootKeyGroupInterval time.Duration `mapstructure:"boot_key_group_interval"`
	// The maximum time to wait for the unattended installation to finish — on the
	// iso builder, long enough for the whole install to run. What the build waits
	// for depends on the communicator: with `ssh` or `winrm` it waits for the VM
	// to report an address to connect to, and with `none` it waits for the guest
	// to power itself off, which is the only completion signal such a build has.
	// Defaults to 200m.
	InstallTimeout time.Duration `mapstructure:"install_timeout"`

	// Path to a local directory served over HTTP to the VM (kickstart,
	// preseed, or autounattend files). An empty value disables the HTTP server.
	HTTPDir string `mapstructure:"http_directory"`
	// The address the HTTP server binds to, and the address the VM is told to
	// fetch from via `{{ .HTTPIP }}`. Defaults to empty, meaning every interface,
	// in which case the address advertised to the VM is derived from the route to
	// `xenserver_host`. Set this when the machine running Packer reaches the VM on
	// a different network from the one it reaches the pool on. Mutually exclusive
	// with `http_interface`.
	HTTPAddress string `mapstructure:"http_bind_address"`
	// The name of a local network interface whose IPv4 address the VM is told to
	// fetch from. An alternative to `http_bind_address` for naming the route to
	// the VM; mutually exclusive with it.
	HTTPInterface string `mapstructure:"http_interface"`

	SSHConfig `mapstructure:",squash"`

	// The local directory into which artifacts are exported. Defaults to
	// `output-{{build_name}}`. It is created when it does not exist, and an
	// existing empty one is used as it stands; when it holds anything at all
	// the build stops before it starts and asks to be re-run with `-force`,
	// which deletes the directory and everything in it first. A build that
	// fails or is cancelled deletes the directory on its way out, unless it is
	// the one it refused to start in. `-force` is not scoped to this directory:
	// it also destroys any VM or template on the pool whose name-label is
	// `vm_name`, and the disks they own.
	OutputDir string `mapstructure:"output_directory"`
	// The output format of the exported VM: `xva`, `vdi_raw`, `vdi_vhd`, or
	// `none`. Defaults to `xva`. `xva_compressed` is still accepted as a
	// deprecated alias of `xva` — XVA exports are always compressed — and
	// warns when used. `vdi_raw` and `vdi_vhd` export each disk individually,
	// raw or as a VHD; XenServer refuses a VHD export of a disk larger than
	// 2040 GiB (`VDI_TOO_LARGE`). Use `none` to export nothing and convert the
	// finished VM into a template instead. That template is the output, so
	// `none` defaults `keep_vm` to `on_success`.
	Format string `mapstructure:"format"`
	// When to keep the build VM: `always`, `never`, or `on_success`. The default
	// follows `format`: `on_success` when `format` is `none`, because the VM is
	// then the only output, and `never` for every format that exports a file.
	KeepVM string `mapstructure:"keep_vm"`
	// Take a snapshot of the VM before finishing the build. Defaults to false.
	CreateSnapshot bool `mapstructure:"create_snapshot"`
	// The name for the snapshot created when `create_snapshot` is true. Setting it
	// on its own is rejected, since it would take no snapshot.
	SnapshotName string `mapstructure:"snapshot_name"`

	// Skip verification of the XenServer TLS certificate. Defaults to false,
	// meaning the certificate is verified. Intended for non-production use only.
	// Named to match the XenServer Terraform provider, and to say plainly which
	// knob it is: it is what `tls.Config.InsecureSkipVerify` is set from.
	//
	// Falls back to the `XENSERVER_INSECURE` environment variable. This is a
	// pointer so that an unset option can be told from an explicit
	// `insecure = false`: only the former defers to the environment. Setting
	// `ca_cert_path` in the template says the same thing another way, so a build
	// that pins a certificate there reports `XENSERVER_INSECURE=true` as ignored
	// rather than quietly stopping verifying. A `ca_cert_path` that came from
	// `XENSERVER_CA_CERT_PATH` is the environment talking, not the template, so
	// `XENSERVER_INSECURE=true` still applies there.
	Insecure *bool `mapstructure:"insecure"`
	// Path to the PEM-encoded CA certificate used to verify the XenServer TLS
	// connection. Required unless `insecure` is true. Falls back to the
	// `XENSERVER_CA_CERT_PATH` environment variable.
	CACertPath string `mapstructure:"ca_cert_path"`

	ctx interpolate.Context
}

// The environment variables Prepare falls back to for the connection options.
// They exist so that a credential need never be written into a .pkr.hcl at
// all; an option set in the template always wins over the environment. The
// guest credentials have their own set, in ssh_config.go.
const (
	EnvHost       = "XENSERVER_HOST"
	EnvUsername   = "XENSERVER_USERNAME"
	EnvPassword   = "XENSERVER_PASSWORD"
	EnvCACertPath = "XENSERVER_CA_CERT_PATH"
	EnvInsecure   = "XENSERVER_INSECURE"
)

// InsecureEnabled reports whether the build asked to skip TLS verification.
// Use it rather than dereferencing: a nil Insecure means the option was not
// set, which is false here — verifying is the default.
func (c CommonConfig) InsecureEnabled() bool {
	return c.Insecure != nil && *c.Insecure
}

// applyConnectionEnv fills in the connection options the template left unset
// from the environment, so that credentials can be kept out of the build file.
// A value present in the template always wins: an exported XENSERVER_PASSWORD
// must not silently redirect a build that named its own.
func (c *CommonConfig) applyConnectionEnv() (warns []string, err error) {
	// Read before the environment can fill it in: a ca_cert_path that came from
	// the template is the build file saying it wants this pool verified.
	pinnedByTemplate := c.CACertPath != ""

	applyEnvFallbacks([]envFallback{
		{&c.HostIp, EnvHost},
		{&c.Username, EnvUsername},
		{&c.Password, EnvPassword},
		{&c.CACertPath, EnvCACertPath},
	})

	if c.Insecure == nil {
		if raw := os.Getenv(EnvInsecure); raw != "" {
			v, parseErr := strconv.ParseBool(raw)
			if parseErr != nil {
				// Guessing here would turn a typo into a silently unverified
				// connection, which is the one failure mode this option has.
				return warns, fmt.Errorf("%s must be a boolean (for example \"true\" or \"0\"), got '%s'", EnvInsecure, raw)
			}
			// The environment fills in what the template left unsaid; it does
			// not get to reverse a decision the template already made in other
			// words. A XENSERVER_INSECURE left exported from a lab pool would
			// otherwise stop verifying a build whose whole statement on the
			// subject is the certificate it pinned — and say nothing about it.
			if v && pinnedByTemplate {
				warns = append(warns, fmt.Sprintf("%s=%s was ignored because this build sets ca_cert_path, "+
					"so the pool's certificate is still verified. Set insecure = true in the template to skip "+
					"verification.", EnvInsecure, raw))
			} else {
				c.Insecure = &v
			}
		}
	}

	return warns, nil
}

// validateHost rejects a xenserver_host that would put the XenAPI session on a
// plaintext channel. The login credentials and the session reference — which
// is a bearer token for the whole pool — both travel in that session, so
// http:// is not a downgrade a template gets to ask for.
func validateHost(host string) error {
	switch {
	case strings.HasPrefix(strings.ToLower(host), "http://"):
		return fmt.Errorf("xenserver_host must not use http://: the XenAPI login and the session "+
			"reference it returns would travel in plaintext. Use https:// or a bare host name, got '%s'", host)
	case strings.HasPrefix(strings.ToLower(host), "https://"), !strings.Contains(host, "://"):
		return nil
	default:
		return fmt.Errorf("xenserver_host must be a host name, an IP address, or an https:// URL, got '%s'", host)
	}
}

// SecureBootEnabled reports whether the build asked for Secure Boot. Use it
// rather than dereferencing: a nil SecureBoot means the option was not set.
func (c CommonConfig) SecureBootEnabled() bool {
	return c.SecureBoot != nil && *c.SecureBoot
}

// HasVendorDeviceEnabled reports whether the VM should carry the emulated vendor
// PCI device, reading a nil HasVendorDevice -- the option not set -- as true.
//
// The iso builder's helper, and only its: that VM comes off an empty install
// template, where Windows needs the device and Linux ignores it, so on is the
// useful default. A clone comes off a template with the OS already installed,
// where writing the flag is itself what breaks the guest, so it reads the pointer
// directly and leaves an unset option to the source (CA-431569).
func (c CommonConfig) HasVendorDeviceEnabled() bool {
	return c.HasVendorDevice == nil || *c.HasVendorDevice
}

// Name is the VM's name-label. Prepare always leaves VMName set, so the nil
// case is only reachable from a config that was never prepared.
func (c CommonConfig) Name() string {
	if c.VMName == nil {
		return ""
	}
	return *c.VMName
}

// GetInterpContext returns the interpolation context the embedding builder config
// shares with the SDK's Prepare calls and the boot-command step. The pointer
// receiver keeps it the builder's own context rather than a pointer into a copy.
func (c *CommonConfig) GetInterpContext() *interpolate.Context {
	return &c.ctx
}

func (c *CommonConfig) Prepare(pc *common.PackerConfig) (warns []string, errs []error) {
	if c.BootWait == 0 {
		c.BootWait = 5 * time.Second
	}

	// Unset is left at zero rather than defaulted here: Prepare has no XenAPI session,
	// and the count a UEFI iso build needs cannot be told from an explicit 1 once it has
	// been written over. StepTypeBootCommand resolves it. A negative count is a typo.
	if c.BootKeyGroups < 0 {
		errs = append(errs, fmt.Errorf("boot_key_groups must be at least 1, got %d", c.BootKeyGroups))
	}

	// Repetitions with no gap between them all land at once, so they cover no more of a
	// boot prompt's window than a single send does. The two options only mean anything
	// together, and a build that set one of them was asking to cover a window.
	if c.BootKeyGroups > 1 && c.BootKeyGroupInterval == 0 {
		errs = append(errs, errors.New("boot_key_groups is greater than 1 but "+
			"boot_key_group_interval is not set, so every repetition is sent at once and covers "+
			"no more of the boot prompt than one send would. Set boot_key_group_interval to the "+
			"gap between repetitions, or remove boot_key_groups."))
	}

	if c.InstallTimeout == 0 {
		c.InstallTimeout = 200 * time.Minute
	}

	if c.CDFiles == nil {
		c.CDFiles = make([]string, 0)
	}

	if c.OutputDir == "" {
		c.OutputDir = fmt.Sprintf("output-%s", pc.PackerBuildName)
	}

	// These five options are pointers so that a value the build wrote can be told
	// apart from the option being absent. Their zero values cannot describe a
	// usable VM, and used to be the same thing as "unset", so an explicit
	// vm_name = "" or vcpus = 0 was silently replaced by a default and the build
	// reported success having ignored it (CA-431825).
	if c.VMName != nil && *c.VMName == "" {
		errs = append(errs, errors.New("vm_name must not be empty: it is the VM's name-label and the name of "+
			"the exported file. Remove vm_name to get the default packer-{{build_name}}-{{timestamp}}."))
	}
	for _, o := range []struct {
		name  string
		value *uint
	}{
		{"vcpus", c.VCPUs},
		{"cores_per_socket", c.CorePerSocket},
		{"vm_memory_gb", c.VMMemoryGB},
		{"disk_size_gb", c.DiskSizeGB},
	} {
		if o.value != nil && *o.value == 0 {
			errs = append(errs, fmt.Errorf("%s must be greater than 0. Remove it to take the builder's "+
				"default, which for the clone builder is whatever the source has.", o.name))
		}
	}

	// mapstructure keeps an absent list nil and an explicit [] non-nil, and this is the
	// last place the two can be told apart -- as is an unknown list under `packer
	// validate`, which packer also decodes to [] (hcl2template/shim/values.go).
	if c.NetworkNames != nil && len(c.NetworkNames) == 0 {
		errs = append(errs, errors.New("network_names names no network. Omit the option to take the "+
			"builder's default networking, or list at least one network name-label."))
	}
	for i, name := range c.NetworkNames {
		// The steps destroy the inherited VIFs before looking any of these up, so an
		// entry that cannot resolve costs the VM its networking on the way to failing.
		if strings.TrimSpace(name) == "" {
			errs = append(errs, fmt.Errorf("network_names[%d] names no network. Every entry has to "+
				"be the name-label of a network on the pool.", i))
		}
	}

	if c.VMName == nil {
		// Substitute the timestamp here rather than leaving "{{timestamp}}" for
		// something later to render: nothing renders it. The SDK interpolates the
		// template's own values before Prepare runs, so a default assigned here has
		// already missed that pass, and the literal braces went on to XAPI as the VM's
		// name-label and into the exported file name -- every build that named no
		// vm_name produced `packer-<build>-{{timestamp}}.xva` and collided with the
		// one before it. InitTime is what `{{timestamp}}` expands to, so a vm_name the
		// template does spell that way still comes out identical.
		name := fmt.Sprintf("packer-%s-%d", pc.PackerBuildName, interpolate.InitTime.Unix())
		c.VMName = &name
	}

	if c.Format == "" {
		c.Format = "xva"
	}

	// XVA exports are always gzip-compressed, so "xva_compressed" is only a spelling of
	// "xva". Normalizing here is what keeps every consumer downstream of Prepare from
	// having to know the alias exists: c.Format can only be "xva", "vdi_raw", "vdi_vhd"
	// or "none" by the time a step reads it.
	if c.Format == "xva_compressed" {
		warns = append(warns, "xva_compressed is deprecated: XVA exports are always compressed. "+
			"Use \"xva\" instead — the two are equivalent.")
		c.Format = "xva"
	}

	// format = "none" makes the build VM itself the output, so the usual "never"
	// default would delete the only thing the build produced. Default on the
	// format, and leave an explicit keep_vm to mean exactly what it says.
	if c.KeepVM == "" {
		if c.Format == "none" {
			c.KeepVM = "on_success"
		} else {
			c.KeepVM = "never"
		}
	}

	// The connection options are the ones worth keeping out of a build file, so
	// they fall back to the environment before anything below asks whether they
	// are set.
	envWarns, err := c.applyConnectionEnv()
	warns = append(warns, envWarns...)
	if err != nil {
		errs = append(errs, err)
	}

	// Packer core redacts through this filter and populates it from HCL `sensitive = true`
	// only, so a password that arrived from the environment -- the route the docs
	// recommend -- reaches packer.log unredacted unless it is registered here.
	packersdk.LogSecretFilter.Set(c.Password)

	if c.Username == "" {
		errs = append(errs, fmt.Errorf("xenserver_username must be specified (or set %s).", EnvUsername))
	}

	if c.Password == "" {
		errs = append(errs, fmt.Errorf("xenserver_password must be specified (or set %s).", EnvPassword))
	}

	if c.HostIp == "" {
		errs = append(errs, fmt.Errorf("xenserver_host must be specified (or set %s).", EnvHost))
	} else if err := validateHost(c.HostIp); err != nil {
		errs = append(errs, err)
	}

	switch {
	case c.InsecureEnabled() && c.CACertPath != "":
		// A working combination, just not the one the template looks like it asked for:
		// the certificate is never read and nothing is verified against it.
		warns = append(warns, fmt.Sprintf("insecure = true overrides ca_cert_path '%s': the pool's "+
			"certificate is not verified and that file is never read. Remove insecure to verify "+
			"against it.", c.CACertPath))
	case c.InsecureEnabled():
		// Nothing to verify against, by request.
	case c.CACertPath == "":
		errs = append(errs, fmt.Errorf("ca_cert_path must be specified when insecure is false. "+
			"Point it at the pool's certificate (or set %s), or set insecure = true to skip "+
			"verification, which is for non-production use only.", EnvCACertPath))
	default:
		// Load it here rather than let the first connection do it: the goSDK
		// calls log.Fatal on an unreadable or unparseable CA certificate, which
		// takes the plugin process down without a Packer-level error. This is
		// the same loader the connection uses, so what passes here loads there.
		if _, err := loadCACertPool(c.CACertPath); err != nil {
			errs = append(errs, err)
		}
	}

	for _, f := range c.CDFiles {
		if _, err := os.Stat(f); os.IsNotExist(err) {
			errs = append(errs, fmt.Errorf("cd_files: '%s' does not exist", f))
		}
	}

	errs = append(errs, validateBootCommandWaits(c.BootCommand)...)

	if c.HTTPAddress != "" && c.HTTPInterface != "" {
		errs = append(errs, errors.New("only one of http_bind_address or http_interface may be specified"))
	}

	// The value is both bound with net.Listen and handed to the VM as {{ .HTTPIP }}, so
	// anything that is not a literal address fails far from the mistake.
	if c.HTTPAddress != "" && net.ParseIP(c.HTTPAddress) == nil {
		errs = append(errs, fmt.Errorf("http_bind_address must be an IP address, got '%s'", c.HTTPAddress))
	}

	// shutdown_command runs over the guest communicator; with "none" there is nothing to
	// run it on, and falling back to a hard shutdown would risk the finished image.
	if c.ShutdownCommand != "" && c.Comm.Type == "none" {
		errs = append(errs, errors.New("shutdown_command requires a communicator: it runs inside the guest, "+
			"so it cannot be used with communicator = \"none\". Remove it; where the build does boot the "+
			"guest, have the unattended installation shut it down instead"))
	}

	// A build with no communicator has no login and no provisioners. What it does
	// instead is not the same on both builders -- an iso build always has an
	// installation to wait out, a clone may have nothing to run at all -- so each
	// builder's own Prepare says which it will be. Only the part that holds either way
	// belongs here.
	if c.Comm.Type == "none" {
		warns = append(warns, "communicator = \"none\": the build never logs in to the guest, "+
			"so no provisioner runs.")
	}

	// "xva_compressed" is absent on purpose: the normalization above already rewrote it to "xva".
	// vdi_raw/vdi_vhd differ only in the format= parameter of the same XAPI handler (see
	// step_export.go, and xapi ocaml/xapi/export_raw_vdi.ml: Importexport.Format.of_req).
	switch c.Format {
	case "xva", "vdi_raw", "vdi_vhd", "none":
	default:
		errs = append(errs, errors.New("format must be one of 'xva', 'vdi_raw', 'vdi_vhd', 'none'"))
	}

	switch c.KeepVM {
	case "always", "never", "on_success":
	default:
		errs = append(errs, errors.New("keep_vm must be one of 'always', 'never', 'on_success'"))
	}

	// Both builders compare firmware verbatim against "uefi" and write it verbatim to
	// XAPI, which accepts only "bios" and "uefi". "UEFI" used to fail both: it was
	// refused alongside vtpm/secure_boot, and reached XAPI unchanged otherwise. Empty
	// stays empty -- that is "keep the clone source's" -- but whitespace is not empty,
	// so it falls through to the switch rather than silently becoming it.
	if lowered := strings.ToLower(strings.TrimSpace(c.Firmware)); lowered != "" {
		c.Firmware = lowered
	}

	switch c.Firmware {
	case "", "bios", "uefi":
	default:
		errs = append(errs, fmt.Errorf("firmware must be one of 'bios', 'uefi', got '%s'", c.Firmware))
	}

	// format = "none" makes the build VM itself the output: nothing is exported and
	// the VM is converted to a template instead. keep_vm = "never" then destroys that
	// template on the way out, so the pair asks for a build that produces nothing at
	// all. An unset keep_vm now defaults to "on_success" for this format, so reaching
	// here means the build named the pair on purpose.
	if c.Format == "none" && c.KeepVM == "never" {
		errs = append(errs, errors.New("format = \"none\" with keep_vm = \"never\" produces no output: "+
			"the build VM is converted to a template and then destroyed. Remove keep_vm to keep the "+
			"template, since format = \"none\" defaults it to \"on_success\", or pick an export format."))
	}

	// snapshot_name only names the snapshot create_snapshot takes, so on its own it
	// does nothing, and the build that asked for a named snapshot produces none.
	if c.SnapshotName != "" && !c.CreateSnapshot {
		errs = append(errs, errors.New("snapshot_name is set but create_snapshot is not, so no "+
			"snapshot is taken and snapshot_name has no effect. Set create_snapshot = true to take "+
			"one, or remove snapshot_name."))
	}

	return warns, errs
}

// Uint returns a pointer to v, for assigning a builder default to one of the
// pointer-typed sizing options.
func Uint(v uint) *uint {
	return &v
}

// ValidateCPUTopology rejects a vcpus and cores_per_socket pair XenServer cannot build a
// VM from, on the two grounds XenCenter refuses one (XenModel VM.cs:1547-1557). Each
// builder calls it after applying its own defaults, which are not knowable here. Either
// option unset leaves nothing to check: the VM keeps the source's.
func ValidateCPUTopology(vcpus, coresPerSocket *uint) error {
	// A zero either side is reported by CommonConfig.Prepare, which names the
	// option; saying it again here as a topology error would only confuse.
	if vcpus == nil || coresPerSocket == nil || *vcpus == 0 || *coresPerSocket == 0 {
		return nil
	}

	if *vcpus%*coresPerSocket != 0 {
		return fmt.Errorf("cores_per_socket (%d) must divide vcpus (%d) exactly: XenServer derives the "+
			"socket count from the two and refuses to start a VM whose vcpus is not a multiple of it",
			*coresPerSocket, *vcpus)
	}

	// XAPI does not check this one, so it fails below the toolstack rather than at
	// VM.start: Xen's hard limit is maxSockets (CA-198276).
	if sockets := *vcpus / *coresPerSocket; sockets > maxSockets {
		return fmt.Errorf("cores_per_socket (%d) over vcpus (%d) gives the VM %d sockets, more than the %d "+
			"XenServer supports: raise cores_per_socket, or lower vcpus", *coresPerSocket, *vcpus,
			sockets, maxSockets)
	}

	return nil
}

// ShouldKeepVM reports whether cleanup must leave the build VM in place.
func (c CommonConfig) ShouldKeepVM(state multistep.StateBag) bool {
	switch c.KeepVM {
	case "always":
		return true
	case "never":
		return false
	case "on_success":
		// only keep instance if build was successful
		_, cancelled := state.GetOk(multistep.StateCancelled)
		_, halted := state.GetOk(multistep.StateHalted)
		return !(cancelled || halted)
	default:
		panic(fmt.Sprintf("Unknown keep_vm value '%s'", c.KeepVM))
	}
}

// nullRef is how XAPI spells an unset object reference on the wire.
const nullRef = "OpaqueRef:NULL"

func (config CommonConfig) GetSR(c *Connection) (xenapi.SRRef, error) {
	if config.VMSR != "" {
		return findSRByNameLabel(c, config.VMSR)
	}

	hostRef, err := c.session.GetThisHost(c.ref)
	if err != nil {
		return "", err
	}

	pools, err := xenapi.Pool.GetAllRecords(c.session)
	if err != nil {
		return "", err
	}

	for _, pool := range pools {
		if pool.Master == hostRef {
			// A pool with no default SR hands back a null reference, which
			// VDI.create reports as an invalid handle — say which option
			// fixes it instead.
			if pool.DefaultSR == "" || pool.DefaultSR == nullRef {
				return "", errors.New("the pool has no default SR: set vm_sr to the name-label of " +
					"the SR the build should use")
			}
			return pool.DefaultSR, nil
		}
	}

	return "", fmt.Errorf("no pool on this connection has host '%s' as its primary, so its default SR "+
		"was never looked at: point xenserver_host at the pool primary, or set vm_sr", hostRef)
}

// findSRByNameLabel resolves a single SR from its name-label, which has to be
// unique across the pool.
func findSRByNameLabel(c *Connection, nameLabel string) (xenapi.SRRef, error) {
	var srRef xenapi.SRRef

	srs, err := xenapi.SR.GetByNameLabel(c.session, nameLabel)
	if err != nil {
		return srRef, err
	}

	switch {
	case len(srs) == 0:
		return srRef, fmt.Errorf("Couldn't find a SR with the specified name-label '%s'", nameLabel)
	case len(srs) > 1:
		return srRef, fmt.Errorf("Found more than one SR with the name '%s'. The name must be unique", nameLabel)
	}

	return srs[0], nil
}
