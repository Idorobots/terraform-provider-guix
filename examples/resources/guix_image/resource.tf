resource "guix_image" "server" {
  name          = "server"
  configuration = file("${path.module}/system.scm")
  channels      = file("${path.module}/channels-lock.scm")
  image_type    = "efi-raw"
  system        = "x86_64-linux"

  # Optional source contents for custom image types and target platforms:
  # image_definition  = file("${path.module}/images/custom.scm")
  # system_definition = file("${path.module}/platforms/custom.scm")

  source_files = {
    "management.pub" = guix_key_pair.management.authorized_key
  }
}
