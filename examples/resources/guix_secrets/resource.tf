resource "guix_secrets" "server" {
  files = {
    "/etc/example/private-key" = {
      content = var.private_key
      mode    = "0600"
    }
  }
}

variable "private_key" {
  type      = string
  sensitive = true
}

# Pass secrets = guix_secrets.server to guix_image. This currently records the
# descriptor and dependency only; installation into the image is not implemented.
