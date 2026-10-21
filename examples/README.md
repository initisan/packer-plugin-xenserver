## Examples

Examples of building a machine image with common guest operating systems, to get new users up and running with the packer builder.

For an exhaustive list of configuration options see the [xenserver-vm-iso](../docs/builders/xenserver-vm-iso.mdx) and [xenserver-vm-clone](../docs/builders/xenserver-vm-clone.mdx) documentation. This doc covers only what is specific to each example.

### Layout

Each example lives in its own directory and declares every variable it uses, so it
is valid on its own:

| Directory | Builder | Guest |
| --- | --- | --- |
| `centos8-local/` | `xenserver-vm-iso` | CentOS 8, kickstart from the local DVD |
| `centos8-netinstall/` | `xenserver-vm-iso` | CentOS 8, kickstart from an external repository |
| `ubuntu-2004/` | `xenserver-vm-iso` | Ubuntu 20.04, autoinstall seed staged with `cd_files` (skeleton — see the caveat under [Ubuntu](#ubuntu)) |
| `windows-iso/` | `xenserver-vm-iso` | Windows, install from an ISO on an ISO SR, WinRM |
| `windows-clone-winrm/` | `xenserver-vm-clone` | Windows, clone an existing template, WinRM |

Keeping one example per directory matters: Packer loads *every* `*.pkr.hcl` in a
directory as a single configuration, so two self-contained templates side by side
would collide on their `variable` and `locals` declarations.

### Running the examples

Run packer from the plugin directory (`management/packer-plugin-xenserver`) — the
`http_directory` and `cd_files` paths in these templates are relative to it.

1. Export those vars:
```
PKR_VAR_xenserver_host
PKR_VAR_xenserver_password
PKR_VAR_xenserver_username
PKR_VAR_vm_sr
```
`PKR_VAR_xenserver_host` must be the resource pool primary, aka the master.
`PKR_VAR_vm_sr` names a writable SR: the VM's disk goes there, and so does every
image the build uploads. Leave it unset and the ISO example uses the pool's default
SR, while the clone example clones in place on the template's own SR — naming an SR
the template is not already on makes that build copy its disks in full rather than
clone them copy-on-write. The lookup is by name-label and rejects a name that matches
more than one SR (`Found more than one SR with the name '...'`), so the stock
`Local storage` — the name every host's local SR carries — is unusable on a pool of
more than one host. The Windows ISO example needs `PKR_VAR_iso_name` and
`PKR_VAR_iso_sr` as well — neither has a default, and that one installs from an ISO
already staged on an ISO library, which is only ever read from.

2. Run `packer init path/to/definition.pkr.hcl` to download the xenserver plugin

3. Run `packer build path/to/definition.pkr.hcl`
so for example:
`packer build examples/centos8-netinstall/centos8-netinstall.pkr.hcl`

The three Linux examples pin `output_directory` to a fixed name instead of taking the
`output-<build_name>` default, so running one of them twice stops before it starts:
the second run finds the first run's artifact still there. Delete the directory, or
re-run with `packer build -force`, which deletes it for you. `-force` is safe for these
three — it also destroys any VM or template on the pool whose name-label matches
`vm_name`, and theirs carry a timestamp, so it matches nothing left behind.

Either a single file or a whole example directory can be passed to `packer validate`
and `packer build`:

```
packer validate examples/centos8-netinstall
packer validate examples/centos8-netinstall/centos8-netinstall.pkr.hcl
```

### Choosing a clone_template

Every example names one, because both builders require a source to clone and neither
substitutes one. The Linux examples name `Other install media`, the generic template:
it describes no guest OS, so it carries no sizing, firmware or vTPM worth inheriting.
The sizing options have no defaults of their own, so a build naming it must set
`vcpus`, `vm_memory_gb` and `disk_size_gb` itself or fail validation. Naming the OS
template instead — `xe template-list params=name-label` lists what the pool has, and
so does XenCenter's New VM wizard — lets XenServer size and configure the virtual
hardware for that guest, and `vcpus`, `cores_per_socket`, `vm_memory_gb` and
`disk_size_gb` can then all come out: the iso builder reads the disk size off the
template it clones. The Windows examples name an OS template, through a
`clone_template` variable you can repoint with `PKR_VAR_clone_template` if your pool
labels it differently, and still set the sizing themselves, so the VM comes out the
same shape whatever that template is configured with.

### Ubuntu

The Ubuntu example is built around the [autoinstall tool](https://ubuntu.com/server/docs/install/autoinstall) for configuring the VM template. Please see the [autoinstall docs](https://ubuntu.com/server/docs/install/autoinstall-reference) for an exhaustive list of what is supported.

The Ubuntu example does not use Packer's HTTP server. It lists the [user-data](http/ubuntu-2004/user-data) and [meta-data](http/ubuntu-2004/meta-data) files in `cd_files`, so the builder writes them to a second CD and attaches that to the VM.

Treat this example as a skeleton rather than a working unattended build. The builder fixes the
CD's volume label at `cddata` and has no `cd_label` option, while cloud-init's NoCloud
datasource looks for a filesystem labelled `cidata`, so the seed is not found by label; and
the example sets no `boot_command`, so nothing adds the `autoinstall ds=...` kernel arguments
that would point the installer at it either. As it stands the installer comes up interactive and
no address ever reaches the build: it sets no `http_directory`, so Packer starts no HTTP server
whose requests the builder could snoop, and the autoinstall that never ran never installed the
guest tools XAPI would otherwise report an address from. The build stops at `Step: Wait for VM's
IP to become known to us.` and fails with `No IP address for the VM within 3h20m0s
(install_timeout)` — the 200-minute default, since the template does not set `install_timeout`.
Its `ssh_timeout` is never consulted: the build never gets as far as connecting. For a template that
does drive autoinstall end to end, see `Ubuntu (autoinstall, from an ISO URL)` on the
[xenserver-vm-iso](../docs/builders/xenserver-vm-iso.mdx) page: it serves `user-data` and
`meta-data` from `http_directory` and types `autoinstall ds=nocloud-net;s=http://...` in a
`boot_command`.

### CentOS

The CentOS examples use kickstart files to configure the VM template. Please see the [kickstart documentation](https://access.redhat.com/documentation/en-us/red_hat_enterprise_linux/7/html/installation_guide/sect-kickstart-syntax) for the options that are supported.

Packer will create a http server to serve the files as specified from the `http_directory` specified in the builder configuration. This is where the kickstart config files ([local](http/centos8/ks-centos8-local.cfg), [netinstall](http/centos8/ks-centos8-netinstall.cfg)) must be present.

### Windows

Both Windows examples connect over WinRM, so the guest must have WinRM enabled and
reachable before Packer can connect. Each ships a `*.pkrvars.hcl.example` to copy and
fill in; keep passwords out of the var-file and pass them via `PKR_VAR_xenserver_password`
and `PKR_VAR_winrm_password`. Packer auto-loads only `*.auto.pkrvars.hcl`, so name the copy
that way or pass it with `-var-file=<path>` — a plain `*.pkrvars.hcl` next to the template
is ignored, and the build stops on unset variables.

Before it can connect, the build has to learn the VM's address, and neither example sets
`http_directory` — so Packer runs no HTTP server whose requests it could snoop, and the only
remaining source is XAPI guest metrics. That means **XenServer VM Tools must be installed and
running in the guest**: in `windows-iso/` the answer file has to install the MSI you staged on
the `cd_files` CD, and in `windows-clone-winrm/` the golden template has to have them already.
Without them the build clones, boots, and then sits until `install_timeout` (200 minutes by
default) before failing with `No IP address for the VM within 3h20m0s (install_timeout)`,
never having attempted WinRM.

- `windows-iso/` installs from an ISO already present on an ISO SR. For a fully
  unattended install, put `Autounattend.xml` — and the XenServer VM Tools MSI — on a
  second CD via `cd_files`; Windows Setup reads the answer file from the CD root. You
  supply both files — the repository ships neither, and the commented-out `cd_files`
  line in that template is an example path, not a file already present. Like every other
  path in these templates it is resolved relative to the directory you run `packer` from,
  so point it at wherever you put the files.
- `windows-clone-winrm/` skips the install entirely and clones an existing golden
  template, which is the faster path for patching or re-customising an image.

Neither declares an `ssh_username` variable any more — the builder stopped asking WinRM
templates for one (CA-430886). Drop it from an older invocation: passing
`-var ssh_username=...` to a template that no longer declares it is an error, and leaving
the key in a copied var-file is a warning.

Windows 11 requires UEFI firmware and a vTPM (`firmware = "uefi"`, `vtpm = true`);
Windows Server does not. Both builders take `vtpm`, and both refuse `vtpm = true` unless
`firmware = "uefi"` is set explicitly, since each one clones a source and otherwise inherits
that source's firmware. On `xenserver-vm-iso` it is a plain bool: `true` adds a vTPM when the
VM has none, `false` adds none — it cannot remove one the source template brought with it. On
`xenserver-vm-clone` it is three-way, because a clone inherits its source's vTPM: leave it
unset to keep whatever the source has, `true` attaches one when the source had none, and
`false` destroys the inherited vTPM — and with it anything sealed to that vTPM, a BitLocker
key above all, with no way back — as well as removing `platform:vtpm`, the key XenServer
would otherwise manufacture a fresh vTPM from at the next start.
