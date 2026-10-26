resource "guix_image" "server" {
  name          = "server"
  configuration = file("${path.module}/system.scm")
  channels      = file("${path.module}/channels-lock.scm")
  image_type    = "efi-raw"
  system        = "x86_64-linux"

  source_files = {
    "management.pub" = guix_key_pair.management.authorized_key
  }
}
