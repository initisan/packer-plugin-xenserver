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
  description = "The ip or fqdn of your XenServer pool primary (master). Leave unset to take it from the XENSERVER_HOST environment variable."
  sensitive   = true
  default     = null
}

variable "xenserver_password" {
  type        = string
  description = "The password used to interact with your XenServer. Leave unset to take it from the XENSERVER_PASSWORD environment variable."
  sensitive   = true
  default     = null
}

variable "xenserver_username" {
  type        = string
  description = "The username used to interact with your XenServer. Leave unset to take it from the XENSERVER_USERNAME environment variable."
  sensitive   = true
  default     = null

}

variable "vm_sr" {
  type        = string
  default     = ""
  description = "Name label of the SR the VM disk, the cd_files CD and the downloaded ISO are created on. Defaults to the pool default SR."
}

locals {
  timestamp = regex_replace(timestamp(), "[- TZ:]", "")
}


source "xenserver-vm-iso" "ubuntu-2004" {
  # 20.04 went EOL on 2025-05-31; releases.ubuntu.com only carries supported
  # releases, so the images moved to old-releases.ubuntu.com. 20.04.5 is the
  # last live-server image published there.
  iso_checksum = "sha256:5035be37a7e9abbdc09f0d257f3e33416c1a0fb322ba860d42d74aa75c3468d4"
  iso_url      = "https://old-releases.ubuntu.com/releases/20.04/ubuntu-20.04.5-live-server-amd64.iso"

  vm_sr = var.vm_sr

  xenserver_host     = var.xenserver_host
  xenserver_password = var.xenserver_password
  xenserver_username = var.xenserver_username

  vm_name        = "packer-ubuntu-2004-${local.timestamp}"
  vm_description = "Build started: ${local.timestamp}"
  vm_memory_gb   = 4
  disk_size_gb   = 20
  vcpus          = 2

  # The generic template: it describes no guest OS, so it brings no sizing or
  # firmware of its own.
  clone_template = "Other install media"

  # The builder labels this CD "cddata" and has no cd_label option; cloud-init's
  # NoCloud datasource looks for "cidata", so the seed is not auto-detected and no
  # boot_command here points the installer at it. See "Ubuntu" in examples/README.md.
  cd_files = [
    "examples/http/ubuntu-2004/meta-data",
    "examples/http/ubuntu-2004/user-data",
  ]

  # Linux guest: the builder defaults the communicator to "none", so SSH has to be named.
  communicator           = "ssh"
  ssh_username           = "testuser"
  ssh_password           = "ubuntu"
  ssh_timeout            = "60m"
  ssh_handshake_attempts = 100

  # Verify the pool certificate in production: drop this and set
  # ca_cert_path to the pool's PEM instead.
  insecure = true

  output_directory = "packer-ubuntu-2004-iso"
  keep_vm          = "always"
}

build {
  sources = ["xenserver-vm-iso.ubuntu-2004"]
}
