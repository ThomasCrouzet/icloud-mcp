#!/usr/bin/env python3
"""Offline tests for scripts/mcpb.py. Run from the repository root:

    python3 -m unittest scripts/test_mcpb.py

The tests use synthetic executables and archives. They do not use the network.
"""

import json
import os
import stat
import sys
import tarfile
import tempfile
import unittest
import zipfile
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import mcpb  # noqa: E402

VERSION = "0.5.1"


def write_server(path, version=VERSION):
    data = {
        "$schema": "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json",
        "name": "io.github.ThomasCrouzet/icloud-mcp",
        "title": "icloud-mcp",
        "description": "stdio MCP for iCloud Calendar (CalDAV), with optional Contacts and Mail.",
        "repository": {
            "url": "https://github.com/ThomasCrouzet/icloud-mcp",
            "source": "github",
        },
        "version": version,
    }
    path.write_text(json.dumps(data), encoding="utf-8")


def write_binary(path, version=VERSION):
    path.write_text(
        "#!/usr/bin/env python3\n"
        "import sys\n"
        "if sys.argv[1:] == ['-version']:\n"
        f"    print('v{version}')\n"
        "    raise SystemExit(0)\n"
        "raise SystemExit(2)\n",
        encoding="utf-8",
    )
    path.chmod(0o755)


def write_source(root, version=VERSION):
    source = root / "source"
    source.mkdir()
    write_binary(source / "icloud-mcp", version)
    (source / "LICENSE").write_text("MIT License\n", encoding="utf-8")
    (source / "THIRD_PARTY_NOTICES.md").write_text(
        "Third party notices.\n", encoding="utf-8"
    )
    return source


def write_zip(path, entries):
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        for name, data, mode in entries:
            info = zipfile.ZipInfo(name, date_time=mcpb.ZIP_TIMESTAMP)
            info.compress_type = zipfile.ZIP_DEFLATED
            info.create_system = 3
            info.external_attr = (stat.S_IFREG | mode) << 16
            archive.writestr(info, data)


def read_entries(path):
    with zipfile.ZipFile(path) as archive:
        return [
            (info.filename, archive.read(info.filename), (info.external_attr >> 16) & 0o777)
            for info in archive.infolist()
        ]


class BundleTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        self.server = self.root / "server.json"
        write_server(self.server)
        self.source = write_source(self.root)

    def tearDown(self):
        self.temp.cleanup()

    def build(self, source=None, target="linux/amd64", version=VERSION, output=None,
              allow_windows=False):
        output = output or self.root / mcpb.bundle_filename(version, *target.split("/"))
        return mcpb.build_bundle(
            source or self.source,
            target,
            version,
            self.server,
            output,
            allow_windows,
        )

    def test_build_and_verify_success(self):
        summary = self.build()
        self.assertEqual(summary["status"], "ok")
        verified = mcpb.verify_bundle(Path(summary["bundle"]), self.server)
        self.assertEqual(verified["status"], "ok")
        self.assertEqual(verified["version"], VERSION)
        self.assertEqual(verified["binaryVersion"], f"v{VERSION}")
        self.assertEqual(
            verified["entries"],
            [
                "LICENSE",
                "THIRD_PARTY_NOTICES.md",
                "manifest.json",
                "server/icloud-mcp",
            ],
        )
        with zipfile.ZipFile(summary["bundle"]) as archive:
            manifest = json.loads(archive.read("manifest.json"))
        self.assertEqual(manifest["manifest_version"], "0.4")
        self.assertEqual(manifest["server"]["type"], "binary")
        self.assertEqual(
            manifest["server"]["mcp_config"]["command"],
            "${__dirname}/server/icloud-mcp",
        )
        self.assertEqual(manifest["user_config"]["read_only"]["default"], "true")
        self.assertTrue(manifest["user_config"]["icloud_password"]["sensitive"])

    def test_build_from_release_archive(self):
        archive = self.root / "release.tar.gz"
        with tarfile.open(archive, "w:gz") as handle:
            for name in ("icloud-mcp", "LICENSE", "THIRD_PARTY_NOTICES.md"):
                handle.add(self.source / name, arcname=name)
        summary = self.build(source=archive)
        self.assertEqual(mcpb.verify_bundle(Path(summary["bundle"]), self.server)["status"], "ok")

    def test_deterministic_rebuild(self):
        first = self.build(output=self.root / "first.mcpb")
        second = self.build(output=self.root / "second.mcpb")
        self.assertEqual(first["sha256"], second["sha256"])
        self.assertEqual(
            (self.root / "first.mcpb").read_bytes(),
            (self.root / "second.mcpb").read_bytes(),
        )

    def test_no_exec_skips_bundled_binary(self):
        source = self.source
        (source / "icloud-mcp").write_text(
            "#!/usr/bin/env python3\nraise SystemExit(7)\n", encoding="utf-8"
        )
        (source / "icloud-mcp").chmod(0o755)
        bundle = self.root / "no-exec.mcpb"
        mcpb.build_bundle(source, "linux/amd64", VERSION, self.server, bundle)
        with self.assertRaises(mcpb.BundleError):
            mcpb.verify_bundle(bundle, self.server)
        summary = mcpb.verify_bundle(bundle, self.server, execute=False)
        self.assertEqual(summary["binaryVersion"], "not-run")
        self.assertEqual(summary["status"], "ok")

    def test_version_mismatch_fails(self):
        self.build()
        other = self.root / "other.json"
        write_server(other, "0.5.2")
        bundle = self.root / mcpb.bundle_filename(VERSION, "linux", "amd64")
        with self.assertRaises(mcpb.BundleError):
            mcpb.verify_bundle(bundle, other)
        with self.assertRaises(mcpb.BundleError):
            self.build(version="0.5.2", output=self.root / "bad.mcpb")

    def test_missing_binary_fails(self):
        (self.source / "icloud-mcp").unlink()
        with self.assertRaises(mcpb.BundleError):
            self.build(output=self.root / "bad.mcpb")

    def test_unsupported_target_fails(self):
        with self.assertRaises(mcpb.BundleError):
            self.build(target="linux/386", output=self.root / "bad.mcpb")

    def test_windows_bundle_requires_override(self):
        with self.assertRaises(mcpb.BundleError):
            self.build(target="windows/amd64", output=self.root / "bad.mcpb")
        summary = self.build(
            target="windows/amd64",
            allow_windows=True,
            output=self.root / "windows.mcpb",
        )
        with zipfile.ZipFile(summary["bundle"]) as archive:
            manifest = json.loads(archive.read("manifest.json"))
        self.assertEqual(manifest["server"]["entry_point"], "server/icloud-mcp.exe")
        self.assertEqual(manifest["compatibility"]["platforms"], ["win32"])

    def test_absolute_path_entry_is_rejected(self):
        source = self.build(output=self.root / "good.mcpb")
        entries = []
        for name, data, mode in read_entries(source["bundle"]):
            if name == "server/icloud-mcp":
                name = "/server/icloud-mcp"
            entries.append((name, data, mode))
        write_zip(self.root / "absolute.mcpb", entries)
        with self.assertRaises(mcpb.BundleError):
            mcpb.verify_bundle(self.root / "absolute.mcpb", self.server)

        entries = []
        for name, data, mode in read_entries(source["bundle"]):
            if name == "server/icloud-mcp":
                name = "../server/icloud-mcp"
            entries.append((name, data, mode))
        write_zip(self.root / "traversal.mcpb", entries)
        with self.assertRaises(mcpb.BundleError):
            mcpb.verify_bundle(self.root / "traversal.mcpb", self.server)

    def test_archive_traversal_is_rejected(self):
        archive = self.root / "evil.tar.gz"
        payload = self.root / "payload"
        payload.write_bytes(b"x")
        with tarfile.open(archive, "w:gz") as handle:
            info = tarfile.TarInfo("../evil")
            info.size = 1
            with payload.open("rb") as source:
                handle.addfile(info, source)
        with self.assertRaises(mcpb.BundleError):
            self.build(source=archive, output=self.root / "bad.mcpb")

    def test_manifest_absolute_path_is_rejected(self):
        source = self.build(output=self.root / "good.mcpb")
        entries = []
        for name, data, mode in read_entries(source["bundle"]):
            if name == "manifest.json":
                manifest = json.loads(data)
                manifest["homepage"] = "file:///home/user/private"
                data = json.dumps(manifest).encode("utf-8")
            entries.append((name, data, mode))
        write_zip(self.root / "absolute-manifest.mcpb", entries)
        with self.assertRaises(mcpb.BundleError):
            mcpb.verify_bundle(self.root / "absolute-manifest.mcpb", self.server)

    def test_credential_like_value_is_rejected(self):
        source = self.build(output=self.root / "good.mcpb")
        entries = []
        for name, data, mode in read_entries(source["bundle"]):
            if name == "manifest.json":
                manifest = json.loads(data)
                manifest["description"] = "password=abcd1234efgh"  # gitleaks:allow
                data = json.dumps(manifest).encode("utf-8")
            entries.append((name, data, mode))
        write_zip(self.root / "credential.mcpb", entries)
        with self.assertRaises(mcpb.BundleError):
            mcpb.verify_bundle(self.root / "credential.mcpb", self.server)

    def test_non_executable_entry_is_rejected(self):
        source = self.build(output=self.root / "good.mcpb")
        entries = []
        for name, data, mode in read_entries(source["bundle"]):
            if name == "server/icloud-mcp":
                mode = 0o644
            entries.append((name, data, mode))
        write_zip(self.root / "not-executable.mcpb", entries)
        with self.assertRaises(mcpb.BundleError):
            mcpb.verify_bundle(self.root / "not-executable.mcpb", self.server)

    def test_manifest_command_is_stable(self):
        manifest = mcpb.manifest_for("darwin/arm64", f"v{VERSION}")
        self.assertEqual(manifest["compatibility"]["platforms"], ["darwin"])
        self.assertEqual(manifest["server"]["entry_point"], "server/icloud-mcp")
        self.assertEqual(manifest["version"], VERSION)
        self.assertNotIn("/Users/", json.dumps(manifest))


if __name__ == "__main__":
    unittest.main()
