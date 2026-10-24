resource "tls_private_key" "management" {
  algorithm = "ED25519"
}

resource "guix_key_pair" "management" {
  name       = "management"
  public_key = tls_private_key.management.public_key_openssh
}
