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

variable "host_public_keys" {
  description = "Pinned SSH server public keys, obtained or provisioned through a trusted commissioning channel. These are separate from the management key."
  type = object({
    controller = string
    worker     = string
  })
}

provider "guix" {
  artifact_directory = "${path.root}/.guix-artifacts"
}

resource "tls_private_key" "management" {
  algorithm = "ED25519"
}

resource "guix_key_pair" "management" {
  name       = "homelab-management"
  public_key = tls_private_key.management.public_key_openssh
}

locals {
  channels = file("${path.module}/channels-lock.scm")
  sources = {
    "management.pub" = guix_key_pair.management.authorized_key
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

resource "guix_image" "controller" {
  name          = "controller"
  configuration = local.controller_configuration
  channels      = local.channels
  source_files  = local.sources
}

resource "guix_host" "controller" {
  hostname = "controller.local"
  image    = guix_image.controller
  ssh = {
    host_key    = var.host_public_keys.controller
    private_key = tls_private_key.management.private_key_openssh
  }
}

resource "guix_image" "worker" {
  name          = "worker"
  configuration = local.worker_configuration
  channels      = local.channels
  source_files  = local.sources
}

resource "guix_host" "worker" {
  hostname = "worker.local"
  image    = guix_image.worker
  ssh = {
    host_key    = var.host_public_keys.worker
    private_key = tls_private_key.management.private_key_openssh
  }
}

output "management_key" {
  value = {
    public_key  = guix_key_pair.management.authorized_key
    fingerprint = guix_key_pair.management.fingerprint
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
