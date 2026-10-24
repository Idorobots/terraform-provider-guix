resource "guix_host" "server" {
  hostname        = "server.local"
  bootstrap_image = guix_image.server.id
  configuration   = file("${path.module}/system.scm")
  channels        = file("${path.module}/channels-lock.scm")

  source_files = {
    "management.pub" = guix_key_pair.management.authorized_key
  }

  ssh = {
    user        = "root"
    host_key    = var.server_host_public_key
    private_key = tls_private_key.management.private_key_openssh
  }

  timeouts = {
    create = "45m"
    read   = "30s"
    update = "30m"
  }
}
