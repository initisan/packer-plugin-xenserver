# -----------------------------------------------------------------------------
# XenServer Clone builder — Windows golden image over WinRM
#
# A complete, self-contained clone-and-provision example. It clones an existing
# Windows template, waits for WinRM, runs a provisioner, and leaves the result
# as a template on the pool.
#
# The golden template must already have XenServer VM Tools installed: with no
# http_directory the build learns the VM's address from XAPI guest metrics alone,
# so without them it waits out install_timeout (200m) and never attempts WinRM.
#
# No secrets are committed. Supply credentials at build time via environment,
# e.g.:
#   export PKR_VAR_xenserver_host=pool-master.example.com
#   export PKR_VAR_xenserver_password='...'
#   export PKR_VAR_winrm_password='...'
#
# Run packer from the plugin directory (management/packer-plugin-xenserver), as
# examples/README.md says:
#   packer init     examples/windows-clone-winrm/windows-clone-winrm.pkr.hcl
#   packer validate examples/windows-clone-winrm/windows-clone-winrm.pkr.hcl
#   packer build    examples/windows-clone-winrm/windows-clone-winrm.pkr.hcl
# -----------------------------------------------------------------------------

packer {
  required_version = ">= 1.7.0"

  required_plugins {
    xenserver = {
      version = ">= 1.0.0"
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

# --- Source and placement ---------------------------------------------------

variable "clone_template" {
  type        = string
  default     = "Windows 11 (64-bit) golden"
  description = "Name label of the source template to clone. Use clone_vm instead to clone a VM."
}

# A named SR is resolved by name-label and has to be unique across the pool, so the
# stock "Local storage" fails on any multi-host pool.
variable "vm_sr" {
  type        = string
  default     = ""
  description = "Name label of the SR for the VM's disks. Empty clones in place on the template's own SR, which is what this example wants; naming an SR the template is not already on makes the build copy its disks in full."
}

variable "virtual_network_name" {
  type    = string
  default = "Pool-wide network associated with eth0"
}

# --- Hardware ---------------------------------------------------------------

variable "vm_name" {
  type    = string
  default = "packer-win11-golden"
}

variable "memory_gb" {
  type    = number
  default = 8
}

variable "cpu" {
  type    = number
  default = 4
}

variable "firmware" {
  type    = string
  default = "uefi"
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

source "xenserver-vm-clone" "windows" {
  # Connection to the pool coordinator
  xenserver_host     = var.xenserver_host
  xenserver_username = var.xenserver_username
  xenserver_password = var.xenserver_password

  # Verify the pool certificate in production: drop this and set
  # ca_cert_path to the pool's PEM instead.
  insecure = true

  # Source and placement. vm_sr is empty here, so the clone's disks stay on the
  # template's own SR and the build is copy-on-write.
  clone_template = var.clone_template
  vm_sr          = var.vm_sr
  # Naming networks on a clone means exactly these NICs: the VIFs inherited from
  # clone_template are destroyed first, so the guest sees new MAC addresses. Drop
  # network_names to keep the template's own.
  network_names = [var.virtual_network_name]

  # Hardware
  vm_name      = var.vm_name
  vm_memory_gb = var.memory_gb
  vcpus        = var.cpu

  # Windows 11 has to boot UEFI, so this example names it. Anywhere else, omit
  # firmware and the clone keeps clone_template's own boot mode. Setting it is an
  # override, and a firmware the source cannot boot with — "bios" against a source
  # that has run under UEFI or carries a vTPM, "uefi" against one that has run
  # under BIOS — is refused before anything is cloned.
  firmware = var.firmware

  # Communicator: WinRM. A clone runs no Windows Setup, so FirstLogonCommands
  # never fire -- the source template must ALREADY have a WinRM HTTPS listener
  # on 5986. Without it the build waits out winrm_timeout and then fails on the
  # connection, with nothing pointing at the template as the cause.
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
  name    = "windows-golden-from-template"
  sources = ["source.xenserver-vm-clone.windows"]

  provisioner "powershell" {
    inline = [
      "Write-Host 'Provisioning the golden image...'",
      # Stand-in for your real provisioning.
      "Get-Service -Name WinRM | Format-List Name, Status"
    ]
  }
}
