# Guix secrets channel

This provider-owned Guile channel exposes two provisioning-only commands:

- `guix secrets image`: install private files into a copy of an already-built image.
- `guix secrets deploy`: install private files on an already-running Guix System host.

The commands do not build or reconfigure a target system, run its activation hooks,
restart services, generate keys, decrypt documents or perform boot-time processing.
SOPS/age preparation belongs to the caller; the configured system handles its own
activation and boot-time work.

## Loading the channel

The provider embeds `modules/` in its binary and exposes that exact implementation
to its pinned `guix time-machine` process through `GUIX_EXTENSIONS_PATH`. No extra
entry is needed in a blueprint's channels file. Tool packages and the public
privileged helper can be realized through the pinned Guix daemon; plaintext
payloads never enter those derivations.

For standalone use, add this repository as a channel, pinned to a commit:

```scheme
(use-modules (guix channels))
(cons (channel
       (name 'guix-secrets)
       (url "https://github.com/SpartanWorks/terraform-provider-guix")
       (commit "<commit>"))
      %default-channels)
```

From a checkout, without pulling the channel:

```sh
GUIX_EXTENSIONS_PATH="$PWD/channel/modules" guix secrets --help
```

## File manifest

Both commands accept a **data-only JSON** manifest. Sources are ordinary files,
not Guix file-like objects or store inputs. Relative sources are resolved against
the manifest directory. Payload bytes can be binary, including empty files.

```json
{
  "version": 1,
  "files": [
    {
      "source": "private/ssh_host_ed25519_key",
      "destination": "/etc/ssh/ssh_host_ed25519_key",
      "mode": "0600"
    },
    {
      "source": "private/age-identity.txt",
      "destination": "/etc/sops/age/keys.txt"
    }
  ]
}
```

Destinations must be normalized absolute file paths outside `/gnu/store`.
Duplicate or ancestor/descendant file destinations are rejected. All files are
owned by `root:root`; modes default to `0600` and accept `0000` through `0777`.
New parent directories are root-owned and mode `0700`; existing directories are
preserved. Symlink destinations and symlink parents are rejected. Directory
descriptors anchor writes to the mounted root or host filesystem.

## Image provisioning

```sh
guix secrets image --manifest=files.json --output=private.img \
  /gnu/store/…-image
```

Supported inputs are raw **MBR or GPT disk images with an ext4 root partition**.
The command selects the only ext filesystem, or uses `--root-label=Guix` to
select by filesystem label when several exist. Other formats, encrypted volumes,
and additional destination filesystems are not handled by this backend. Files
are installed into the selected root filesystem.

The command validates the partition bounds, creates a reflinked/sparse private
copy when possible, and mounts the root extent directly with
`fuse2fs -o fakeroot,offset=…`. It uses no loop devices or runner root privileges
on the FUSE path. `fakeroot` permits setting actual root ownership in the image;
it does not change the runner process's identity.

The runner needs access to `/dev/fuse` and a working FUSE mount helper. If FUSE
is unavailable or mounting is denied, `pkexec` launches a public helper built
into the immutable Guix store. Polkit's authentication agent handles the
administrator-password prompt. Only that helper mounts the copy through an
autoclear loop device, installs the payload, and unmounts it. Payloads travel
over stdin; the helper does not load executable code from the private workspace.
Filesystem errors do not trigger privilege escalation.

The input is preserved. The output must be a new path outside `/gnu/store`; it
is published with mode `0600` only after unmounting and waiting for writes to
finish. Boot partitions and partition-table bytes are preserved. Errors or
authentication cancellation prevent publication and clean the private workspace.
If unmounting fails, the workspace is retained and its location is reported.
Abrupt termination can require manual mount/workspace cleanup.

## Running-host provisioning

```sh
guix secrets deploy --manifest=files.json --host=192.0.2.10 \
  --host-key="$(cat host.pub)" --identity=management-key \
  --user=root --port=22
```

The SSH server key is pinned, and management authentication uses the supplied
unencrypted private-key file. `--user` defaults to `root`; other users require
passwordless `sudo -n`. The target needs `/proc` and Guile at
`/run/current-system/profile/bin/guile`, present in the standard Guix System
profile. The extension itself need not be installed on the target.

Only the public installer code appears in the remote command. Payload bytes
travel over SSH stdin as data and never enter a remote store object. Existing
files are replaced using private temporary files in the destination directory,
with ownership and mode set before publication. Replacement is atomic per file,
not a transaction across the whole collection. Removing a manifest entry does
not remove an existing host file. Repeated provisioning installs the same desired
bytes and metadata, without restarting consumers.

## Checks

From the provider repository:

```sh
make test-channel
make test-channel-images
GUIX_EVALUATION=1 go test ./internal/guix -run 'TestChannel|TestProvisioning|TestSecret'
```

The Scheme suite tests file installation and builds/checks the public Polkit
helper. Disk tests use small real MBR/GPT fixtures, testing selection, publication,
fallback routing, cancellation and layout preservation. They also perform a real
unprivileged FUSE installation when the runner permits it; otherwise that case
is skipped. Fallback routing uses a test double and does not open password prompts.
The SSH tests exercise Guile-SSH against an isolated server and the real installer,
including root/sudo command selection, binary contents and host-key rejection.
