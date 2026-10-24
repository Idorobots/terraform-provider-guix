# Two manually commissioned hosts

1. Adapt `system.scm.tftpl` to your hardware, storage layout, boot mode, and
   networking. This example targets x86_64 UEFI systems. Disk labels must match
   the image and the installed system; check them before deployment.
2. Generate and commit a channel lock:
   `guix describe -f channels > channels-lock.scm`. Ensure each channel has a
   fixed `commit`, rather than only a branch.
3. Obtain each server's SSH host public key through trusted commissioning, or
   provision a known host identity separately. Set `host_public_keys` in a
   variable file. For a first installation using automatically generated host
   keys, you can build the images first, boot, obtain the host keys via the local
   console, and then apply the complete configuration.
4. Initialize providers with `tofu init` and run `tofu apply`. For an unpublished local
   build, follow the development-override instructions in the repository README.
5. In another terminal, inspect `.guix-artifacts/controller/*/image`. When the controller
   image is ready, manually flash and boot it. OpenTofu discovers
   `controller.local`, verifies SSH, and reconciles its system.
6. The controller address makes the worker configuration known. OpenTofu builds
   its image under `.guix-artifacts/worker/*/image`. Flash and boot the worker to
   complete the same apply.

`tls_private_key.management` generates a management identity. Only its public key
is included in the images; the private key is used for SSH and deployment.
The worker's `/etc/controller-address` contains the discovered controller IPv4
address. A controller address change refreshes the dependency and plans a new
worker image plus in-place worker deployment. It does not reflash the worker.

For services that can resolve hostnames dynamically, using `controller.local`
instead of embedding an IP can avoid address-driven rebuilds. The IP reference
here intentionally demonstrates OpenTofu's dependency and unknown-value handling.
