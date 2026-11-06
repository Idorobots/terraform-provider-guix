"""Small real disk fixtures; no boot, root privileges or password prompts.

Put Guix's e2fsprogs and util-linux bin/sbin directories on PATH, then run:
  python3 -m unittest discover -s channel/tests -p 'test_*.py' -v
"""
import hashlib
import json
import os
from pathlib import Path
import shutil
import subprocess
import tempfile
import unittest

CHANNEL = Path(__file__).resolve().parents[1]
OFFSET = 2 * 1024 * 1024
SIZE = 32 * 1024 * 1024


@unittest.skipUnless(all(shutil.which(x) for x in ("guix", "sfdisk", "mkfs.ext4", "debugfs")),
                     "Guix, util-linux and e2fsprogs are required")
class ImageProvisioning(unittest.TestCase):
    def setUp(self):
        self.temporary = tempfile.TemporaryDirectory(prefix="guix-secrets-fixture-")
        self.addCleanup(self.temporary.cleanup)
        self.directory = Path(self.temporary.name)
        self.base = self.directory / "base.img"
        self.output = self.directory / "private.img"
        self.manifest = self.directory / "files.json"
        (self.directory / "payload").write_bytes(b"PRIVATE-IMAGE-KEY\x00\xff\n")
        self.manifest.write_text(json.dumps({"version": 1, "files": [{
            "source": "payload", "destination": "/etc/private/key", "mode": "0600"}]}))

    def fixture(self, label="dos"):
        with self.base.open("wb") as stream:
            stream.truncate(36 * 1024 * 1024)
        table = (f"label: {label}\nunit: sectors\n"
                 f"start=2048,size=2048,type={'c' if label == 'dos' else 'U'}\n"
                 f"start=4096,size=65536,type={'83' if label == 'dos' else 'L'}\n")
        subprocess.run(["sfdisk", str(self.base)], input=table.encode(),
                       check=True, capture_output=True)
        subprocess.run(["mkfs.ext4", "-q", "-F", "-b", "1024", "-L", "Guix",
                        "-E", f"offset={OFFSET}", str(self.base), "32768"],
                       check=True, capture_output=True)
        with self.base.open("r+b") as stream:
            stream.seek(1024 * 1024)
            stream.write(b"BOOT-PARTITION-MUST-STAY-UNCHANGED")
        return self.base.read_bytes()

    def run_driver(self, action, success=True, extra_env=None):
        result = subprocess.run(["guix", "repl", "-L", str(CHANNEL / "modules"),
                                 str(CHANNEL / "tests/image.scm"), action,
                                 str(self.base), str(self.output), str(self.manifest)],
                                capture_output=True, timeout=60,
                                env={**os.environ, **(extra_env or {})})
        if action == "real" and result.returncode == 77:
            self.skipTest("FUSE mounting is unavailable on this runner")
        if success:
            self.assertEqual(result.returncode, 0, result.stderr.decode())
        else:
            self.assertNotEqual(result.returncode, 0)
        return result

    def assert_published(self, original):
        self.assertEqual(self.output.stat().st_mode & 0o777, 0o600)
        final = self.output.read_bytes()
        self.assertEqual(final[:OFFSET], original[:OFFSET])
        self.assertEqual(final[OFFSET + SIZE:], original[OFFSET + SIZE:])
        self.assertEqual(hashlib.sha256(self.base.read_bytes()).digest(),
                         hashlib.sha256(original).digest())
        self.assertFalse(list(self.directory.glob(".guix-secrets-*")))

    def test_partition_selection_mbr_and_gpt(self):
        for label in ("dos", "gpt"):
            with self.subTest(label=label):
                self.fixture(label)
                result = self.run_driver("inspect")
                self.assertIn(b"(2097152 33554432)", result.stdout)

    def test_fuse_success_does_not_escalate(self):
        original = self.fixture()
        self.run_driver("fuse")
        self.assert_published(original)
        self.assertNotEqual(self.output.read_bytes(), original)

    def test_polkit_fallback_publication(self):
        original = self.fixture("gpt")
        self.run_driver("polkit")
        self.assert_published(original)

    def test_authentication_cancellation_cleans_private_copy(self):
        self.fixture()
        self.run_driver("cancel", success=False)
        self.assertFalse(self.output.exists())
        self.assertFalse(list(self.directory.glob(".guix-secrets-*")))

    def test_pkexec_receives_immutable_helper_and_payload_on_stdin(self):
        self.fixture()
        binaries = self.directory / "bin"
        binaries.mkdir()
        pkexec = binaries / "pkexec"
        # Exercise the actual Polkit invocation without opening an auth prompt.
        pkexec.write_text("""#!/usr/bin/env python3
import os, pathlib, sys
assert len(sys.argv) == 2 and sys.argv[1].startswith('/gnu/store/')
assert b'PRIVATE-IMAGE-KEY' not in pathlib.Path(sys.argv[1]).read_bytes()
assert sys.stdin.buffer.read()
pathlib.Path(os.environ['PKEXEC_TEST_LOG']).write_text(sys.argv[1])
sys.exit(126)
""")
        pkexec.chmod(0o700)
        log = self.directory / "pkexec.log"
        self.run_driver("pkexec", success=False, extra_env={
            "PATH": str(binaries) + os.pathsep + os.environ["PATH"],
            "PKEXEC_TEST_LOG": str(log)})
        self.assertTrue(log.read_text().startswith("/gnu/store/"))
        self.assertFalse(self.output.exists())
        self.assertFalse(list(self.directory.glob(".guix-secrets-*")))

    def test_filesystem_failure_does_not_escalate(self):
        self.fixture()
        result = self.run_driver("corrupt", success=False)
        self.assertNotEqual(result.returncode, 99, "filesystem error opened Polkit")
        self.assertFalse(self.output.exists())
        self.assertFalse(list(self.directory.glob(".guix-secrets-*")))

    def test_existing_output_is_not_overwritten(self):
        self.fixture()
        self.output.write_bytes(b"operator-owned-output")
        result = subprocess.run(["guix", "secrets", "image",
                                 "--manifest=" + str(self.manifest),
                                 "--output=" + str(self.output), str(self.base)],
                                env={**os.environ, "GUIX_EXTENSIONS_PATH": str(CHANNEL / "modules")},
                                capture_output=True, timeout=30)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn(b"output must be a new path", result.stderr)
        self.assertEqual(self.output.read_bytes(), b"operator-owned-output")

    def test_ambiguous_root_requires_label(self):
        self.fixture()
        # A second ext filesystem in the boot extent makes implicit selection
        # ambiguous. The label must select the same root extent as before.
        subprocess.run(["mkfs.ext4", "-q", "-F", "-b", "1024", "-O", "^has_journal",
                        "-L", "Other", "-E", "offset=1048576", str(self.base), "1024"],
                       check=True, capture_output=True)
        self.run_driver("inspect", success=False)
        self.assertIn(b"(2097152 33554432)", self.run_driver("select").stdout)

    def test_real_unprivileged_fuse_installation(self):
        if not os.access("/dev/fuse", os.R_OK | os.W_OK):
            self.skipTest("runner has no accessible /dev/fuse")
        original = self.fixture()
        self.run_driver("real")
        self.assert_published(original)
        partition = self.directory / "root.ext4"
        partition.write_bytes(self.output.read_bytes()[OFFSET:OFFSET + SIZE])
        info = subprocess.run(["debugfs", "-R", "stat /etc/private/key", str(partition)],
                              check=True, capture_output=True).stdout.decode()
        self.assertRegex(info, r"User:\s+0\s+Group:\s+0")
        self.assertRegex(info, r"Mode:\s+0600")
        extracted = self.directory / "extracted"
        subprocess.run(["debugfs", "-R", f"dump /etc/private/key {extracted}", str(partition)],
                       check=True, capture_output=True)
        self.assertEqual(extracted.read_bytes(), (self.directory / "payload").read_bytes())
        subprocess.run(["e2fsck", "-fn", str(partition)], check=True, capture_output=True)


if __name__ == "__main__":
    unittest.main()
