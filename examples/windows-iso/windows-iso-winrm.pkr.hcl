# -----------------------------------------------------------------------------
# XenServer ISO builder — Windows golden image over WinRM
#
# Installs Windows from an ISO that already sits on an ISO SR, waits for WinRM,
# runs a provisioner, and leaves the result as a template on the pool — once you
# add the cd_files answer file and VM Tools MSI described below. Without them
# Setup stays interactive and the build fails at install_timeout with no address,
# never having attempted WinRM.
#
# No secrets are committed. Supply credentials at build time via environment,
# e.g.:
#   export PKR_VAR_xenserver_host=pool-master.example.com
#   export PKR_VAR_xenserver_password='...'
#   export PKR_VAR_winrm_password='...'
#
# Run packer from the plugin directory (management/packer-plugin-xenserver), which is
# what the cd_files paths below are relative to:
#   packer init     examples/windows-iso/windows-iso-winrm.pkr.hcl
#   packer validate examples/windows-iso/windows-iso-winrm.pkr.hcl
#   packer build    examples/windows-iso/windows-iso-winrm.pkr.hcl
# -----------------------------------------------------------------------------

packer {
  required_plugins {
    xenserver = {
      version = ">= 0.9.1"
      source  = "github.com/xenserver/xenserver"
    }
  }
}

# --- Connection -------------------------------------------------------------

variable "xenserver_host" {
  type        = string
  description = "Hostname or IP of the pool coordinator (master)."
}

variable "xenserver_username" {
  type    = string
  default = "root"
}

variable "xenserver_password" {
  type      = string
  sensitive = true
}

# --- Install media and placement --------------------------------------------

variable "iso_name" {
  type        = string
  description = "Name label of the Windows install ISO, as it appears on iso_sr."
}

variable "iso_sr" {
  type        = string
  description = "Name label of the SR holding the install ISO."
}

# Empty means the pool's default SR. Naming one instead resolves it by
# name-label, which must be unique across the pool: "Local storage" is the stock
# name-label of every host's local SR, so on a multi-host pool it is rejected
# with "Found more than one SR with the name 'Local storage'".
variable "vm_sr" {
  type    = string
  default = ""
}

variable "clone_template" {
  type        = string
  default     = "Windows 10 (64-bit)"
  description = "XenServer template the build VM starts from; it sets the guest-OS hints."
}

variable "virtual_network_name" {
  type    = string
  default = "Pool-wide network associated with eth0"
}

# --- Hardware ---------------------------------------------------------------

variable "vm_name" {
  type    = string
  default = "packer-windows-golden"
}

variable "vm_description" {
  type    = string
  default = "Created by Packer"
}

variable "memory_gb" {
  type    = number
  default = 8
}

variable "disk_size_gb" {
  type    = number
  default = 50
}

variable "cpu" {
  type    = number
  default = 4
}

# Overrides the boot mode the clone would otherwise inherit from clone_template;
# the build refuses one the template's boot mode rules out. Trimmed and lower-cased
# first, so "UEFI" works; anything but bios/uefi is refused at validate time. Drop
# this from the source block below to inherit the template's, unless vtpm or
# secure_boot is true: those need "uefi".
variable "firmware" {
  type    = string
  default = "uefi"
}

# secure_boot = false is a real setting, not "leave it alone": it writes
# platform:secureboot = "false" on the clone and turns Secure Boot off on a
# clone_template that had it on. To inherit the template's setting, delete the
# secure_boot line from the source block below. true requires firmware = "uefi".
variable "secure_boot" {
  type    = bool
  default = false
}

# Windows 11 requires UEFI plus a vTPM; Windows Server does not.
#
# vtpm = false means "add none", not "have none": VM.clone copies the vTPM of
# whatever clone_template names, and this builder leaves an inherited one alone.
# To build without a vTPM, name a clone_template that has neither a vTPM nor
# platform:vtpm = "true", which XenServer makes a fresh one from at start.
variable "vtpm" {
  type    = bool
  default = false
}

# --- Communicator -----------------------------------------------------------

variable "winrm_username" {
  type    = string
  default = "Administrator"
}

variable "winrm_password" {
  type      = string
  sensitive = true
}

source "xenserver-vm-iso" "windows" {
  # Connection to the pool coordinator
  xenserver_host     = var.xenserver_host
  xenserver_username = var.xenserver_username
  xenserver_password = var.xenserver_password
  # Verify the pool certificate in production: drop this and set
  # ca_cert_path to the pool's PEM instead.
  insecure = true

  # Install media, already present on an ISO SR
  iso_name = var.iso_name
  iso_sr   = var.iso_sr

  # Placement
  vm_sr          = var.vm_sr
  clone_template = var.clone_template
  network_names  = [var.virtual_network_name]

  # Hardware
  vm_name          = var.vm_name
  vm_description   = var.vm_description
  vm_memory_gb     = var.memory_gb
  disk_size_gb     = var.disk_size_gb
  vcpus            = var.cpu
  cores_per_socket = var.cpu
  firmware         = var.firmware
  secure_boot      = var.secure_boot
  vtpm             = var.vtpm

  # Send a key repeatedly over a bounded window to catch the UEFI "Press any
  # key to boot from CD" prompt (≈6 s, unpredictable offset after POST).
  # A fixed boot_wait is unreliable across hosts with different POST times.
  boot_wait               = "3s"
  boot_command            = ["<spacebar>"]
  boot_key_groups         = 20
  boot_key_group_interval = "1s"

  # For a fully unattended install, put Autounattend.xml — and the XenServer VM
  # Tools MSI, which the answer file must install: with no http_directory the build
  # learns the VM's address only from XAPI guest metrics, which the tools provide —
  # on a second CD; Windows Setup reads the answer file from the CD
  # root. You supply both files: this repository ships neither, and an entry that
  # is not on disk is rejected before the build starts with
  # "cd_files: '<path>' does not exist". Paths are resolved against the directory
  # packer runs in, so stage them under this example and, running packer from the
  # plugin directory as examples/README.md says, name them the way the other
  # examples do:
  #   cd_files = [
  #     "examples/windows-iso/windows/Autounattend.xml",
  #     "examples/windows-iso/windows/managementagentx64.msi",
  #   ]

  # Communicator: WinRM
  communicator   = "winrm"
  winrm_username = var.winrm_username
  winrm_password = var.winrm_password
  winrm_use_ssl  = true
  winrm_insecure = true
  winrm_timeout  = "30m"

  # Output and retention: format = "none" exports no artifact and leaves the
  # finished VM as a template; keep it only on success.
  format  = "none"
  keep_vm = "on_success"
}

build {
  name    = "windows-golden-from-iso"
  sources = ["source.xenserver-vm-iso.windows"]

  provisioner "powershell" {
    inline = [
      "Write-Host 'Provisioning the golden image...'",
      "Get-Service -Name WinRM | Format-List Name, Status"
    ]
  }
}
