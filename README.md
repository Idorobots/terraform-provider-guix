# Guix Provider for OpenTofu

`SpartanWorks/guix` builds Guix System images and manages machines using mDNS discovery and `guix deploy` operations.

This project is vibe-coded. I strongly advise against using it for anything serious until further notice.

| Resource | Purpose |
| --- | --- |
| [`guix_blueprint`](docs/resources/blueprint.md) | OS, image and machine specifications, pinned channels, sources and private secrets |
| [`guix_image`](docs/resources/image.md) | Build and provision a bootable image |
| [`guix_host`](docs/resources/host.md) | Discover a running installation, expose its IPv4 address, and reconcile its configuration |

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
  Secrets provisioning requires an already privileged SSH user.
- Image provisioning supports ext2/3/4 and Btrfs. Ext filesystems prefer FUSE
  (`/dev/fuse` and a working `fusermount3` or `fusermount`). Kernel mounting uses
  `pkexec` with a Polkit authentication agent for unprivileged callers.
- Install the [Guix secrets channel](channel/README.md) and include it in pinned
  blueprint channels when provisioning secrets. The extension is required on
  the runner, not the target. Remote secrets copying uses SSH and coreutils.

## Workflow

1. Put Scheme specifications, supporting files, and pinned channels in a
   `guix_blueprint` (for example, `guix describe -f channels > channels-lock.scm`).
2. Pass `blueprint = guix_blueprint.<name>` independently to images and hosts.
   For bootstrap, add `depends_on = [guix_image.<name>]` to the host when image
   creation should finish before the host starts waiting. Run `tofu apply`.
3. While the apply is waiting, find the flashable image under
   `.guix-artifacts/<name>/<image-id>/image` (or the configured `artifact_directory`).
   Inspect `ls -l .guix-artifacts/controller/*/image` in another terminal, manually flash the
   appropriate image to the intended disk, and boot the host.
4. The provider discovers `<hostname>.local`, verifies the pinned SSH host key,
   builds the desired system, provisions attached private files, and deploys if
   the installed system differs.
5. A dependent image can consume `guix_host.controller.ipv4_address`. OpenTofu
   delays that image build until the controller is ready, all within one apply.

The complete [two-host example](examples/two-hosts) combines `tls_private_key`,
Guix SSH authorization, image builds, and an IP-dependent worker configuration.

Plans for new hosts print a `Manual commissioning required` warning describing
the required mDNS and SSH access. No logging environment variable is required.
Existing-host updates and destroy plans do not emit this create-time warning.
During apply, the provider also logs host commissioning instructions at INFO
level (`TF_LOG_PROVIDER=INFO`). Image paths are available on the image resource
and under the GC-root location above.

Images and hosts independently consume a blueprint containing their channels,
source files and specifications. A host can be managed without building an
image, and an image can be built without declaring a host. Each leaf has its own
`system` attribute, defaulting to `x86_64-linux`; set it explicitly for other
architectures.

### Lifecycle and state

- **Blueprints are mutable descriptors.** They have stable identities, and do not
  evaluate Scheme or perform builds or deployments. All three specifications may
  be absent; each consumer validates its own requirements.
- **Images are immutable resources.** Selected source, channels, supporting files,
  image type, architecture or secrets declaration changes replace the image resource.
  The clean base remains in the Guix store; provisioned copies live outside it
  with mode 0600. Secret-only changes reuse the cached base derivation.
- Each image owns a `base-image` GC-root symlink. Its adjacent `image` is the
  flashable private copy, or a symlink to the base when no secrets are attached.
  Destroy deletes the owned copy/symlink and releases the base root. Missing
  artifacts or roots are rebuilt on the next apply. Keep the artifact directory
  on persistent storage accessible to the runner. GC roots do not copy images
  to other runners or artifact stores.
- **An offline host is absent.** After the read timeout (default 30 seconds), an
  unreachable host is removed from state. The next apply waits for it again and
  reconciles it. Authentication errors, host-key mismatches, malformed responses,
  cancellation, and discovery-tool failures return errors and preserve state.
- Changes to the host's selected blueprint specification, channels, supporting
  files, architecture or SSH configuration reconcile it in place with `guix deploy`.
- Secret-only host updates install files without building or reconfiguring the
  system. Installation also runs on create when the installed system already
  matches. Failed provisioning prevents subsequent Guix deployment.
- Unused blueprint specifications and descriptor identities update leaf state
  without rebuilding images or reconciling hosts.
  Shared source files remain tracked by both consumers, even if a particular
  expression does not load every file.
- Refresh records both the observed system and the last successful deployment.
  An out-of-band rollback schedules reconciliation on the next apply.
- Destroying a host only forgets it. It does not shut down, wipe, or uninstall it.
- An interrupted create can be retried: it inspects the existing installation and
  deploys only when needed. Failed updates retain the prior successful state.

### Management keys and host identity

Generate management SSH keys with `tls_private_key` from the TLS provider. Pass
`tls_private_key.management.public_key_openssh` directly into the blueprint's
source bundle for `openssh-configuration`'s `authorized-keys`. Use the matching
`private_key_openssh` for management connections. The TLS provider also exposes
`public_key_fingerprint_sha256` for displaying or verifying the public key.
Authorization changes take effect when the consuming Guix configurations are
deployed.

`ssh.host_key` is the server's own OpenSSH public key. Obtain it through a trusted
commissioning channel, or provision its private half through `guix_blueprint.secrets`.
It is verified by both inspection and Guix deployment. Management keys do not
substitute for server identity. Private host keys are installed outside the Guix
store. `tls_private_key` and `ssh.private_key` values are stored in OpenTofu
state; use a suitably protected state backend. Temporary deployment identity files
are mode 0600 and removed after the operation.

### Private file provisioning

`guix_blueprint.secrets` accepts sensitive Scheme source evaluating to a
`secrets` record from `(guix secrets)`. Entries are `secret-file` records
with a destination, source file or inline string/bytevector content, and a mode
(default `#o600`). Pass the Scheme expression directly, or reuse one through
Terraform locals, variables or templates:

```hcl
resource "guix_blueprint" "server" {
  os       = file("${path.module}/os.scm")
  channels = file("${path.module}/channels-lock.scm")
  secrets  = file("${path.module}/secrets.scm")
}
```

For example, `secrets.scm` may contain:

```scheme
(use-modules (guix secrets))

(secrets
  (files (list (secret-file
                 (source "private/identity")
                 (destination "/etc/example/identity")))))
```

Private Scheme is excluded from `source_files` and public Guix build inputs and
is evaluated only during apply. Relative sources and load expressions use the
Terraform working directory; use `abspath()` in module-local templates. Inline
secrets remain in sensitive Terraform state. Provisioning has no SOPS/age dependency.
Terraform-generated keys interpolated into Scheme are already tracked: changing
the key changes the expression, replaces image consumers and reprovisions host
consumers in place.

Install the [Guix secrets channel](channel/README.md) and include it in the
blueprint's pinned channels. Apply fails with an installation hint if the
extension is unavailable in that pinned environment. The provider invokes
`guix secrets image` after the base image is built and `guix secrets deploy`
before any required host deployment. These commands only install files: no
activation hooks, service restarts, system reconfiguration or boot-time processing.
Payloads use separate mode-0700 workspaces and mode-0600 files. They are never
embedded in Scheme build expressions, command arguments or public source bundles.

If `guix secrets image` fails, the provider retains the clean image and emits a
warning that the secrets were not provisioned. You must manually install the files
declared in `blueprint.secrets` at their configured destinations before booting.
Cancellation and failed running-host provisioning remain errors.

Files are root-owned with declared modes; new private parent directories use
0700. Image writes reject symlink parents/leaves. Running-host provisioning uses
the effective machine's SSH connection, ordinary shell/coreutils operations and
an already privileged user; permission failures are errors. It needs neither
remote Guile nor a sudo fallback. Replacements are atomic per file. Removing the
secrets declaration or destroying a blueprint/host does not delete remote files. Files installed
before a later deployment failure remain installed; retrying re-applies the
desired files. Terraform tracks changes to the secrets expression through the
blueprint. Refresh observes the host system, not individual remote secrets.
Keep external ciphertext preparation in a separate module and pass its encrypted
output explicitly through `source_files`.

#### External secret files

Terraform observes values read by its expressions and data sources. It cannot
discover files read only by Scheme: changing `private/identity` in the example
above leaves the `secrets` string unchanged. Use native Terraform/OpenTofu change
tracking for such inputs.

**Image replacement:** wrap file hashes in the built-in `terraform_data` resource
and reference it from `lifecycle.replace_triggered_by`:

```hcl
resource "terraform_data" "secret_files" {
  input = {
    identity = filesha256("${path.module}/private/identity")
  }
}

resource "guix_image" "server" {
  blueprint = guix_blueprint.server

  lifecycle {
    replace_triggered_by = [terraform_data.secret_files]
  }
}
```

The hash is recalculated during planning. Its change updates `terraform_data`,
which replaces the image resource and provisions a fresh private artifact. The
clean Guix base can still come from the cache. The hash need not be part of the
blueprint or public build inputs, and `terraform_data` needs no extra provider.
For a directory, `fileset()` plus `filesha256()` can form a map of file revisions;
for another external system, a data source can supply its version or digest.

**In-place host provisioning:** `replace_triggered_by` requests resource
replacement, not a host update, and `depends_on` only controls ordering. To
update both consumers without another provider attribute, include a revision
comment in the private Scheme expression:

```hcl
resource "guix_blueprint" "server" {
  os       = file("${path.module}/os.scm")
  channels = file("${path.module}/channels-lock.scm")
  secrets  = <<-SCM
    ; identity revision: ${filesha256("${path.module}/private/identity")}
    ${file("${path.module}/secrets.scm")}
  SCM
}
```

The comment changes the Terraform input while leaving Scheme's meaning intact.
This replaces image consumers and triggers only file provisioning on hosts when
their public configuration is unchanged. It can be used instead of the image's
`terraform_data` trigger when both consumers should react. Inline contents read
with `file()` or `filebase64()` and interpolated into the declaration also cause
ordinary input changes and need no separate revision.

Files read by these functions must be available when Terraform evaluates the
configuration. Hashes/revisions detect changes; a `timestamp()` trigger would
instead cause an action on every apply, including when files are unchanged.
See the [OpenTofu built-in resource documentation](https://opentofu.org/docs/language/resources/tf-data/)
and [Terraform lifecycle reference](https://developer.hashicorp.com/terraform/language/meta-arguments/lifecycle#replace_triggered_by).

### Source bundles

Blueprint `os`, `image`, and `machine` are independently optional Scheme source
strings evaluating to their respective Guix record types. Supply raw source,
`file()`, `templatefile()`, or an explicit `(load "...")`; literal filenames are
not loaded automatically. Each operation evaluates only its selected expression
using the pinned Guix version:

| Consumer | Preferred specification | Fallback | Missing-input error |
| --- | --- | --- | --- |
| `guix_image` | `blueprint.image` | `blueprint.os` | Neither is provided |
| `guix_host` | `blueprint.machine` | `blueprint.os` | Neither is provided |

A wrong result type is an error, not a reason to fall back. Unknown preferred
values defer selection until resolved. Known missing inputs fail planning;
Scheme result types are checked when the selected expression is evaluated.

A host using `machine` builds the machine's operating system for comparison and
deploys a single inherited machine record. The machine must use Guix's managed
SSH environment. Its deployment policies, such as `build-locally?`, `authorize?`
and `allow-downgrades?`, are preserved. The host's discovered address, effective
host/provider SSH settings and leaf `system` override the corresponding machine
fields; a supplied SSH session is cleared so those overrides take effect. With
`os`, the provider constructs the SSH machine record itself. Keep private keys
in SSH settings or the private `secrets` expression rather than public OS/image/machine source strings.

`image_type` is optional. With an OS result, omitting it uses the pinned Guix
version's default image type; setting it passes `--image-type` to Guix. With an
image result, the specification determines its own format and layout, following
native Guix behavior. Set `image_type = "efi-raw"` to select an EFI raw image
explicitly when using an OS specification.

`source_files` maps relative paths to contents, so changes to imported Scheme modules and local
assets participate in OpenTofu planning. The provider creates an isolated
workspace and adds it to the Guix load path. Relative assets should use Guix
`local-file`. All needed files must be included explicitly; paths outside the
bundle are not automatically tracked. The top-level names `specification.scm`, `configuration.scm`,
`channels.scm`, `machines.scm`, and `identity` are reserved. Pin every channel to a
commit to make builds and deployments reproducible.

For a bundled file that returns an image or an OS, preserve its relative assets
with an explicit load:

```hcl
resource "guix_blueprint" "server" {
  os       = "(load \"hosts/server.scm\")"
  channels = file("${path.module}/channels-lock.scm")
  source_files = {
    "hosts/server.scm" = file("${path.module}/hosts/server.scm")
    # Include supporting modules and assets at their relative paths.
  }
}
```

### Custom image types and platforms

Distribute custom image types and platforms through a Guix channel and include
that channel, pinned to a commit, in `channels-lock.scm` alongside pinned Guix.
`guix time-machine` creates and caches an isolated profile containing those
channels; no global `guix pull` is needed on the runner.

Use Guix's standard discovery layout in the channel repository:

```text
gnu/system/images/homelab.scm  # (gnu system images homelab), exporting image types
guix/platforms/homelab.scm     # (guix platforms homelab), exporting custom platforms
```

For example, `gnu/system/images/homelab.scm` can export a minimal custom image
type based on Guix's EFI image:

```scheme
(define-module (gnu system images homelab)
  #:use-module (gnu image)
  #:use-module (gnu system image)
  #:export (homelab-image-type))

(define homelab-image-type
  (image-type
    (inherit efi-raw-image-type)
    (name 'homelab-efi-raw)))
```

```hcl
resource "guix_blueprint" "homelab" {
  os       = file("${path.module}/system.scm")
  channels = file("${path.module}/channels-lock.scm")
}

resource "guix_image" "homelab" {
  blueprint  = guix_blueprint.homelab
  image_type = "homelab-efi-raw"
  system     = "aarch64-linux"
}
```

Guix discovers the exported types and platforms from the pinned channel profile.
`image_type` matches an image-type record's `name`, not its Scheme binding name;
`system` matches a platform record's `system` field. Built-in systems such as
`aarch64-linux` need no custom platform. The blueprint's specifications define
the operating system, image or machine, and its `source_files` supplies supporting modules and assets.
Adding files to `source_files` makes them importable through `-L`, but does not
register them for image/platform discovery inside a pinned channel profile.

Publish definition changes to the channel and update its pinned commit to use
them. Changes to blueprint `channels` replace consuming images and reconcile
consuming hosts using those same channels.

### Cross-compiling images

Set `target = "aarch64-linux-gnu"` on `guix_image` to pass Guix's `--target`
for the image build instead of `--system`. Guix builds the cross tools for the
runner's native architecture. Set `system = "aarch64-linux"` independently on
the host leaf for native host system builds and deployment. Changing `target`
replaces the image.

The target applies only to image creation. Host reconciliation and `guix deploy`
still build for `system` and require a native builder, offload, or emulation for
that architecture.

### Import

Configure provider-level SSH credentials and the host's pinned key, then run:

```shell
tofu import guix_host.controller controller.local
```

The host must be reachable. Configure the required `blueprint = guix_blueprint.<name>`
object and the correct `system` in the host block. Import adopts the observed
system but does not infer its blueprint; the blueprint is populated from
configuration on the next apply. Image artifacts and logical descriptors are
created from configuration rather than imported.

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
image replacement, offline semantics, cancellation, and
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
To exercise a custom image type or platform, include its pinned channel in
`GUIX_TEST_CHANNELS` and select it with `GUIX_TEST_IMAGE_TYPE` or `GUIX_TEST_SYSTEM`.

To evaluate generated machine definitions against installed Guix without building
an image or contacting a host, run:

```shell
GUIX_EVALUATION=1 go test ./internal/guix -run Specification
```

These checks exercise inline and file-loaded OS/image/machine specifications,
relative assets, connection overrides, policy inheritance and invalid specification diagnostics. They use the installed Guix; the
opt-in full integration test uses the pinned channels supplied by the operator.

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
