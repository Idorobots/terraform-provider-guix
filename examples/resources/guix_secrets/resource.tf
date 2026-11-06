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

# Pass secrets = guix_secrets.server to guix_blueprint. Image consumers install
# these files in a private copy after building; hosts install them before any
# required Guix deployment. The files never enter the Guix store.
