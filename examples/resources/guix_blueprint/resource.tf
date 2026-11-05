resource "guix_blueprint" "server" {
  os       = "(load \"hosts/server.scm\")"
  channels = file("${path.module}/channels-lock.scm")

  source_files = {
    "hosts/server.scm"     = file("${path.module}/hosts/server.scm")
    "hosts/management.pub" = guix_key_pair.management.authorized_key
  }

  secrets = guix_secrets.server

  # Optional independent image/machine expressions take precedence over os in
  # their respective leaves. A blueprint may also omit all three specifications.
}

resource "guix_image" "server" {
  blueprint = guix_blueprint.server
  system    = "x86_64-linux"
}

resource "guix_host" "server" {
  hostname  = "server.local"
  blueprint = guix_blueprint.server
  system    = "x86_64-linux"
  ssh = {
    host_key    = var.server_host_public_key
    private_key = tls_private_key.management.private_key_openssh
  }
}
