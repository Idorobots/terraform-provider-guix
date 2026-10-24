# Examples

- [`two-hosts`](two-hosts): complete controller → discovered IP → worker image →
  worker host workflow with `tls_private_key` management authentication.
- [`resources/guix_image`](resources/guix_image): image build.
- [`resources/guix_host`](resources/guix_host): host reconciliation and import.
- [`resources/guix_key_pair`](resources/guix_key_pair): named management public key.

`provider/provider.tf` and `resources/*/resource.tf` are also used to generate the
provider's schema reference documentation.
