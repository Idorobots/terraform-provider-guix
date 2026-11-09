resource "guix_blueprint" "server" {
  os       = file("${path.module}/system.scm")
  channels = file("${path.module}/channels-lock.scm")
  source_files = {
    "management.pub" = tls_private_key.management.public_key_openssh
  }
}

resource "guix_image" "server" {
  name       = "server"
  blueprint  = guix_blueprint.server
  image_type = "efi-raw"
  system     = "x86_64-linux"

  timeouts = {
    create = "4h"
    update = "4h"
    read   = "30s"
  }
}

# An inline expression may return an image specification. Its definition selects
# the format, so image_type can be omitted. system.scm returns the embedded OS.
resource "guix_blueprint" "from_image_specification" {
  image    = <<-SCM
    (use-modules (gnu image) (gnu system image))
    (os->image (load "system.scm") #:type efi-raw-image-type)
  SCM
  channels = file("${path.module}/channels-lock.scm")

  source_files = {
    "system.scm"     = file("${path.module}/system.scm")
    "management.pub" = tls_private_key.management.public_key_openssh
  }
}

resource "guix_image" "from_image_specification" {
  name      = "image-specification"
  blueprint = guix_blueprint.from_image_specification
  system    = "x86_64-linux"
}
