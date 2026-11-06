resource "guix_blueprint" "server" {
  os       = "(load \"hosts/server.scm\")"
  channels = file("${path.module}/channels-lock.scm")

  source_files = {
    "hosts/server.scm"     = file("${path.module}/hosts/server.scm")
    "hosts/management.pub" = tls_private_key.management.public_key_openssh
  }

  # Sensitive provisioning input, separate from public source_files.
  secrets = <<-SCM
    (use-modules (guix secrets) (guix base64))
    (secrets
      (files (list (secret-file
                     (destination "/etc/example/private-key")
                     (content (base64-decode "${base64encode(var.private_key)}"))))))
  SCM

  # Optional independent image/machine expressions take precedence over os in
  # their respective leaves. A blueprint may also omit all three specifications.
}

variable "private_key" {
  type      = string
  sensitive = true
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
