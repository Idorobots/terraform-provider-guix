resource "guix_host" "server" {
  hostname = "server.local"
  image    = guix_image.server

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
