terraform {
  required_version = ">= 1.6"
  required_providers {
    guix = {
      source = "SpartanWorks/guix"
    }
    tls = {
      source  = "hashicorp/tls"
      version = "~> 4.0"
    }
  }
}

provider "guix" {
  artifact_directory = "${path.root}/.guix-artifacts"
}

resource "tls_private_key" "management" {
  algorithm = "ED25519"
}

resource "tls_private_key" "host" {
  for_each  = toset(["controller", "worker"])
  algorithm = "ED25519"
}

resource "guix_secrets" "host" {
  for_each = tls_private_key.host
  files = {
    "/etc/ssh/ssh_host_ed25519_key" = {
      content = each.value.private_key_openssh
    }
  }
}

locals {
  channels = file("${path.module}/channels-lock.scm")
  sources = {
    "management.pub" = tls_private_key.management.public_key_openssh
  }
  controller_configuration = templatefile("${path.module}/system.scm.tftpl", {
    hostname           = "controller"
    controller_address = "127.0.0.1"
  })
  worker_configuration = templatefile("${path.module}/system.scm.tftpl", {
    hostname           = "worker"
    controller_address = guix_host.controller.ipv4_address
  })
}

resource "guix_blueprint" "controller" {
  os           = local.controller_configuration
  channels     = local.channels
  source_files = local.sources
  secrets      = guix_secrets.host["controller"]
}

resource "guix_image" "controller" {
  name      = "controller"
  blueprint = guix_blueprint.controller
}

resource "guix_host" "controller" {
  hostname   = "controller.local"
  blueprint  = guix_blueprint.controller
  depends_on = [guix_image.controller]
  ssh = {
    host_key    = tls_private_key.host["controller"].public_key_openssh
    private_key = tls_private_key.management.private_key_openssh
  }
}

resource "guix_blueprint" "worker" {
  os           = local.worker_configuration
  channels     = local.channels
  source_files = local.sources
  secrets      = guix_secrets.host["worker"]
}

resource "guix_image" "worker" {
  name      = "worker"
  blueprint = guix_blueprint.worker
}

resource "guix_host" "worker" {
  hostname   = "worker.local"
  blueprint  = guix_blueprint.worker
  depends_on = [guix_image.worker]
  ssh = {
    host_key    = tls_private_key.host["worker"].public_key_openssh
    private_key = tls_private_key.management.private_key_openssh
  }
}

output "management_key" {
  value = {
    public_key  = tls_private_key.management.public_key_openssh
    fingerprint = tls_private_key.management.public_key_fingerprint_sha256
  }
}

output "addresses" {
  value = {
    controller = guix_host.controller.ipv4_address
    worker     = guix_host.worker.ipv4_address
  }
}

output "images" {
  value = {
    controller = guix_image.controller.artifact_path
    worker     = guix_image.worker.artifact_path
  }
}
