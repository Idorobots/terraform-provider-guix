resource "tls_private_key" "management" {
  algorithm = "ED25519"
}

resource "guix_key_pair" "management" {
  name       = "management"
  public_key = tls_private_key.management.public_key_openssh
}

output "management_key" {
  value = {
    public_key  = guix_key_pair.management.authorized_key
    fingerprint = guix_key_pair.management.fingerprint
  }
}
