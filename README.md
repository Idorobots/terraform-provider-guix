# Guix Provider for OpenTofu

`SpartanWorks/guix` builds Guix System images and manages manually commissioned
machines using mDNS discovery and pinned `guix deploy` operations. OpenTofu is
the default CLI for development, documentation generation, and deployment.

| Resource | Purpose |
| --- | --- |
| [`guix_image`](docs/resources/image.md) | Build an immutable bootable image and retain it with an independent Guix GC root |
| [`guix_host`](docs/resources/host.md) | Discover a running installation, expose its IPv4 address, and reconcile its configuration |
| [`guix_key_pair`](docs/resources/key_pair.md) | Name and normalize a management public key for Guix SSH authorization |

## Requirements

- A **Linux** runner with OpenTofu >= 1.6 (tested with 1.13.2).
- Guix installed on the runner, a working Guix daemon, and access to the pinned
  channels and their build dependencies. Guix 1.5 is the initial integration target.
- `avahi-resolve-host-name`, a running Avahi daemon and D-Bus, and multicast
  connectivity to the hosts. A container needs access to these services and the
  local multicast network; installing the CLI alone is insufficient.
- Each host runs Guix System with DHCP/network access, Avahi, OpenSSH, and the
  management public key authorized. Pin its SSH **host** public key separately.
- The deployment user is root or has passwordless sudo. Management private keys
  must be unencrypted, supplied as `ssh.private_key` or `ssh.identity_file`.

## Workflow

1. Supply Scheme source using `file()` or `templatefile()`, and pin channels to
   commits (for example, `guix describe -f channels > channels-lock.scm`).
2. Apply an image and host configuration with `tofu apply`. OpenTofu builds the image and then
   waits for the host to appear.
3. While the apply is waiting, find the image GC root under
   `.guix-artifacts/<name>/<image-id>/image` (or the configured `artifact_directory`).
   Inspect `ls -l .guix-artifacts/controller/*/image` in another terminal, manually flash the
   appropriate image to the intended disk, and boot the host.
4. The provider discovers `<hostname>.local`, verifies the pinned SSH host key,
   builds the desired system, and deploys if the installed system differs.
5. A dependent image can consume `guix_host.controller.ipv4_address`. OpenTofu
   delays that image build until the controller is ready, all within one apply.

The complete [two-host example](examples/two-hosts) combines `tls_private_key`,
Guix SSH authorization, image builds, and an IP-dependent worker configuration.

Plans for new hosts print a `Manual commissioning required` warning in the normal
console output, including during the planning phase of `tofu apply`. Set
`image = guix_image.<name>` on the host to include the image file to flash.
If the image has not been built yet, the warning explains that its path is known
after the build; use the GC-root location described above.
No logging environment variable is required. Existing-host updates and destroy
plans do not emit this create-time warning.

The required `image = guix_image.<name>` object is the host's sole system-definition
input. It supplies the configuration, pinned channels, target architecture,
supporting files, image identity, and commissioning path. Update those values on
the image resource to change the desired host system. Image changes reconcile the
host in place.

During apply, the provider also logs the image path and manual flashing/boot
instructions at INFO level (`TF_LOG_PROVIDER=INFO`), once the image path is known.

### Lifecycle and state

- **Images are immutable.** Source, channels, supporting files, image type, or
  target-system changes replace the image resource. Image bytes live in the Guix
  store; OpenTofu state contains paths, the checksum and source digest.
- Each image resource owns a distinct GC-root symlink, even when Guix deduplicates
  identical images. Destroy removes only that reference. A missing artifact/root
  is detected during refresh and rebuilt on the next apply. Keep the artifact
  directory on persistent storage accessible to the runner. GC roots do not copy
  images to other runners or artifact stores.
- **An offline host is absent.** After the read timeout (default 30 seconds), an
  unreachable host is removed from state. The next apply waits for it again and
  reconciles it. Authentication errors, host-key mismatches, malformed responses,
  cancellation, and discovery-tool failures return errors and preserve state.
- Changes to the host's `image` definition use
  `guix deploy`. Image references establish ordering/provenance and never flash
  disks.
- Refresh records both the observed system and the last successful deployment.
  An out-of-band rollback schedules reconciliation on the next apply.
- Destroying a host only forgets it. It does not shut down, wipe, or uninstall it.
- An interrupted create can be retried: it inspects the existing installation and
  deploys only when needed. Failed updates retain the prior successful state.

### Management keys and host identity

`guix_key_pair` takes an OpenSSH **public** key, such as
`tls_private_key.management.public_key_openssh`. Its `authorized_key` output is
passed into a Guix source bundle for `openssh-configuration`'s `authorized-keys`.
The matching private key authenticates management connections. Deleting or
rotating the logical key resource changes host authorization when the consuming
Guix configurations are subsequently deployed.

`ssh.host_key` is the server's own OpenSSH public key. Obtain it through a trusted
commissioning channel, or provision the corresponding host identity separately.
It is verified by both inspection and Guix deployment. Management keys do not
substitute for server identity. Host private-key provisioning is an operator
commissioning step; this provider does not place host private keys in Guix store
objects. `tls_private_key` and `ssh.private_key` values are stored in OpenTofu
state; use a suitably protected state backend. Temporary deployment identity files
are mode 0600 and removed after the operation.

### Source bundles

`configuration` must evaluate to a Guix `operating-system` object. `source_files`
maps relative paths to contents, so changes to imported Scheme modules and local
assets participate in OpenTofu planning. The provider creates an isolated
workspace and adds it to the Guix load path. Relative assets should use Guix
`local-file`. All needed files must be included explicitly; paths outside the
bundle are not automatically tracked. The top-level names `configuration.scm`,
`channels.scm`, `machines.scm`, and `identity` are reserved. Pin every channel to a
commit to make builds and deployments reproducible.

### Import

Configure provider-level SSH credentials and the host's pinned key, then run:

```shell
tofu import guix_host.controller controller.local
```

The host must be reachable. Configure the required `image = guix_image.<name>`
object in the host block. Import adopts the observed system but does not infer
which image defines it; the image object is populated from configuration on the
next apply. Image artifacts and logical key pairs are created from configuration
rather than imported.

## Development

Use Go >= 1.25.8 and install `tofu` on PATH. This repository uses the Terraform
Plugin Framework and the standard provider protocol, which OpenTofu supports.

```shell
go install .
go test ./...
make testacc
go vet ./...
make generate
```

Acceptance tests run the real OpenTofu CLI against deterministic, injected
Guix/discovery/SSH backends; they need no hardware or Guix daemon. Tests cover
delayed discovery, import, in-place deployment, rollback drift, IP-dependent
image replacement, offline semantics, key-pair integration, cancellation, and
independent image ownership. Both `make testacc` and direct
`TF_ACC=1 go test ./internal/provider` invocations default to `tofu` and
`registry.opentofu.org/spartanworks/guix`. Set `TOFU=/absolute/path/to/tofu` to use
a different OpenTofu binary. Documentation generation also honors `TOFU` and
exports the schema with OpenTofu; it does not require or download Terraform.

The upstream test library retains its `TF_ACC_*` environment variable names.
To run the Terraform compatibility check explicitly, set both the CLI path and
registry hostname:

```shell
TF_ACC=1 TF_ACC_TERRAFORM_PATH=/absolute/path/to/terraform \
  TF_ACC_PROVIDER_HOST=registry.terraform.io go test ./internal/provider
```

CI runs OpenTofu 1.13.2 as the primary CLI and Terraform 1.13.5 as a compatibility
check. The HCL `terraform { ... }` block and `terraform-provider-guix` binary name
are retained because OpenTofu uses the same configuration and provider conventions.

To exercise actual Guix builds against your own configuration, see
[`internal/guix/integration_test.go`](internal/guix/integration_test.go). Real-host
SSH checks are opt-in in [`internal/remote/ssh_test.go`](internal/remote/ssh_test.go).

For local OpenTofu development, configure a CLI development override in a
`.tofurc` file. Use the directory reported by `go env GOBIN`; if that is empty,
use the absolute path printed by `go env GOPATH`, followed by `/bin`:

```hcl
provider_installation {
  dev_overrides {
    "registry.opentofu.org/spartanworks/guix" = "/absolute/path/to/go/bin"
  }
  direct {}
}
```

Set `TF_CLI_CONFIG_FILE` to that file. The development override loads the local
binary when running `tofu plan`/`tofu apply`; it does not publish the provider to
the registry. Initialize other providers (such as `hashicorp/tls`) separately if
needed while this provider is unpublished.

## Architecture

- `internal/provider`: provider schemas and lifecycles.
- `internal/guix`: pinned commands, source isolation, image retention, deployment.
- `internal/discovery`: a small `Resolver` interface with an Avahi implementation.
- `internal/remote`: native Go SSH inspection with pinned host keys.

Replacing Avahi with native Go mDNS requires implementing `Resolver.Resolve` and
changing client construction, without altering resource lifecycles or HCL.
Discovery backends return `discovery.ErrUnavailable` for reachability failures and
ordinary errors for broken tooling or malformed responses.
