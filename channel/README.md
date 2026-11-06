# Guix secrets channel

This provider-owned Guile channel supplies `(guix secrets)` records and two
provisioning-only commands. They install files into an already-built image or
an existing host. They do not build/reconfigure the target, run activation hooks,
restart services, generate keys, decrypt documents or perform boot-time work.

## Installation

Add this repository to your channels and pin its commit:

```scheme
(use-modules (guix channels))
(cons (channel
       (name 'guix-secrets)
       (url "https://github.com/SpartanWorks/terraform-provider-guix")
       (commit "<commit>"))
      %default-channels)
```

Install with `guix pull -C channels.scm`. For Terraform, include the channel in
`blueprint.channels` as well: the extension must be discoverable inside the
pinned `guix time-machine` environment. The provider checks this during apply
before building an image or contacting a host for reconciliation. It does not
bundle, extract, or inject extension code.

For checkout development, normal Guix extension discovery can be configured by
the caller:

```sh
GUIX_EXTENSIONS_PATH="$PWD/channel/modules" guix secrets --help
```

## Secrets declarations

Both commands accept a Scheme file evaluating to a `secrets-configuration`:

```scheme
(use-modules (guix secrets))

(secrets-configuration
  (files
    (list
      (secret-file
        (source "private/ssh_host_ed25519_key")
        (destination "/etc/ssh/ssh_host_ed25519_key"))
      (secret-file
        (content #vu8(0 1 255))
        (destination "/etc/example/binary-key")
        (mode #o640)))))
```

Each `secret-file` has exactly one `source` (ordinary local file) or `content`
(string or bytevector), an absolute `destination`, and a numeric `mode` defaulting
to `#o600`. Binary and empty contents are supported. Sources are not Guix
file-like objects and never become store inputs. Relative sources and explicit
load expressions resolve against the secrets declaration's directory. Generated
declarations may select a base directory with `--secrets-directory=DIR`.

Declarations are interpreted locally without compiling/caching private source.
They are trusted Scheme code, like OS and machine declarations. Payloads are not
evaluated remotely. Files are root-owned; destinations must be normalized absolute
file paths outside `/gnu/store`, and modes range from `#o000` to `#o777`.
Duplicate destinations are errors. Entries always describe files, not directories.

## Images

```sh
# OS declaration, with the same image-type selection as guix system image:
guix secrets image --image-type=mbr-hybrid-raw \
  --artifact=clean.img --output=private.img --secrets=secrets.scm os.scm

# Explicit image declaration:
guix secrets image --artifact=clean.img --output=private.img \
  --secrets=secrets.scm image.scm
```

Supports raw MBR/GPT disk images containing **ext2/ext3/ext4 or Btrfs** destination
filesystems. An OS is converted using the selected Guix image-type constructor;
omitting the type uses pinned Guix's default. An image record supplies its own
layout. The root comes from Guix's declared `boot` partition flag, not a scan for
an arbitrary ext filesystem. Actual geometry, filesystem types and labels are
checked against the artifact before mounting. Other declared filesystems identify
their partitions by label or explicit filesystem UUID. Guix's image-specific
filesystem rewriting is respected. Btrfs `subvol`/`subvolid` options select the
appropriate declared child subvolume. Root follows Guix's effective image OS
and mounts the image's default subvolume, rather than replaying host-specific
root options which Guix replaces when creating an image.

Each destination is routed to its longest matching declared mount point. Required
filesystems/subvolumes are mounted independently, and paths are relative to that
mount. An unsupported, external or unmappable destination filesystem is an error;
it never silently writes into the underlying root directory. Encrypted,
multi-device and non-raw image layouts are outside this backend's scope.

The command copies the input with reflink/sparse support, then prefers `fuse2fs`
for ext filesystems when `/dev/fuse` and a mount helper are usable. Btrfs and
unavailable/denied FUSE mounting use a kernel mount. An unprivileged caller uses
`pkexec` and Polkit's authentication agent to run an immutable store-built public
helper. An already-root caller mounts directly. Requests and payloads reach the
helper as data on stdin. Filesystem/install errors do not trigger elevation.
The runner needs the relevant kernel filesystem support and, for unprivileged
kernel mounting, a working Polkit authentication agent.

Local image writes use anchored directory descriptors, reject symlink parents
and leaves, preserve existing directory metadata, and create new parents as
root-owned mode `0700`. The input remains unchanged. Output must be a new path
outside `/gnu/store`, and is published mode `0600` only after unmounting/flushing.
Errors prevent publication. If unmounting fails, the private workspace is retained
and reported; abrupt termination may require manual cleanup.

## Existing hosts

```sh
guix secrets deploy --secrets=secrets.scm machines.scm other-machines.scm
```

Each machine file evaluates to a managed SSH `machine` or list of machines. The
same secrets are installed on each selected machine. Connection parameters and
host-key verification come from `machine-ssh-configuration`, including a supplied
session when present. No SSH credentials are accepted as command-line options.

The configured user must already have permission to create root-owned files.
Deployment uses SSH stdin for raw bytes and ordinary shell/coreutils operations
for private temporary files, ownership, mode and atomic per-file replacement.
The byte count is checked before publication to reject incomplete transfers.
There is no sudo fallback, remote Guile loader, or target extension requirement.
New parents use mode `0700`; existing directory metadata is preserved. Copy or
permission failures are errors. Replacement is not transactional across files or
machines, and removing an entry does not delete an installed file.

## Terraform change tracking

`guix_secrets.secrets` contains the private Scheme expression. Inline contents
change that expression naturally. For external source files, explicitly supply
`file_digests = { name = filesha256(path) }`. Terraform recalculates the digests
during planning; changes replace image consumers and update host consumers.
Digest keys are change-trigger names, not a source-file allowlist or checksum
verification protocol. Scheme is evaluated only during apply. Relative paths in
provider-generated declarations use the Terraform working directory; use
`abspath()` in module templates for module-local sources. Neither declarations
nor file digests enter public build inputs.

## Checks

```sh
make test-channel
go test ./...
```

The Scheme suite uses records, ordinary temporary files, and mocked filesystem
and SSH interfaces. It does not format images, mount filesystems, create loop
devices, build helpers or request authentication. Real filesystem/elevation
verification is a separate manual integration activity.
