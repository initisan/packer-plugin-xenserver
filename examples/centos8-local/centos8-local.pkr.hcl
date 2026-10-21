packer {
  required_plugins {
    xenserver = {
      version = ">= 0.9.1"
      source  = "github.com/xenserver/xenserver"
    }
  }
}

variable "xenserver_host" {
  type        = string
  description = "The ip or fqdn of your XenServer pool primary (master). This will be pulled from the env var 'PKR_VAR_xenserver_host'"
  sensitive   = true
  default     = null
}

variable "xenserver_password" {
  type        = string
  description = "The password used to interact with your XenServer. This will be pulled from the env var 'PKR_VAR_xenserver_password'"
  sensitive   = true
  default     = null
}

variable "xenserver_username" {
  type        = string
  description = "The username used to interact with your XenServer. This will be pulled from the env var 'PKR_VAR_xenserver_username'"
  sensitive   = true
  default     = null

}

variable "vm_sr" {
  type        = string
  default     = ""
  description = "Name label of the SR the VM disk and the downloaded ISO are created on. Defaults to the pool default SR."
}

locals {
  timestamp = regex_replace(timestamp(), "[- TZ:]", "")
}

source "xenserver-vm-iso" "centos8-local" {
  # CentOS 8 went EOL on 2021-12-31 and was purged from the mirror network; the
  # media only survives on vault.centos.org.
  iso_checksum = "sha256:3b795863001461d4f670b0dedd02d25296b6d64683faceb8f2b60c53ac5ebb3e"
  iso_url      = "https://vault.centos.org/8.5.2111/isos/x86_64/CentOS-8.5.2111-x86_64-dvd1.iso"

  vm_sr = var.vm_sr

  xenserver_host     = var.xenserver_host
  xenserver_password = var.xenserver_password
  xenserver_username = var.xenserver_username

  vm_name        = "packer-centos8-local-${local.timestamp}"
  vm_description = "Build started: ${local.timestamp}\n This was installed from the dvd"
  vm_memory_gb   = 4
  disk_size_gb   = 4
  vcpus          = 2

  # This build has to boot BIOS: the boot_command types at the isolinux prompt
  # and the kickstart writes an MBR bootloader. firmware is left unset, so the
  # VM keeps the firmware of clone_template, and "Other install media" carries
  # none, which XenServer boots as BIOS.
  clone_template = "Other install media"

  http_directory = "examples/http/centos8"
  # <up> first: the isolinux menu preselects "Test this media & install", which
  # md5-verifies the whole DVD before the install starts.
  boot_command = ["<up><tab> text ks=http://{{ .HTTPIP }}:{{ .HTTPPort }}/ks-centos8-local.cfg<enter><wait>"]
  boot_wait    = "10s"

  # Linux guest: the communicator defaults to "none", so SSH has to be named.
  communicator = "ssh"
  ssh_username = "root"
  ssh_password = "centos"
  ssh_timeout  = "10000s"

  # Verify the pool certificate in production: drop this and set
  # ca_cert_path to the pool's PEM instead.
  insecure = true

  output_directory = "packer-centos8-local"
  keep_vm          = "always"
}

build {
  sources = ["xenserver-vm-iso.centos8-local"]
}
