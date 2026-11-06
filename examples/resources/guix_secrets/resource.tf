resource "guix_secrets" "server" {
  secrets = <<-SCM
    (use-modules (guix secrets) (guix base64))
    (secrets-configuration
      (files (list (secret-file
                     (destination "/etc/example/private-key")
                     (content (base64-decode "${base64encode(var.private_key)}"))
                     (mode #o600)))))
  SCM
}

variable "private_key" {
  type      = string
  sensitive = true
}

# Pass secrets = guix_secrets.server to guix_blueprint. Image consumers install
# these files in a private copy after building; hosts install them before any
# required Guix deployment. The files never enter the Guix store.
# For source files referenced by Scheme, add explicit change triggers:
# file_digests = { identity = filesha256("/absolute/path/to/identity") }
