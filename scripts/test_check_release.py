#!/usr/bin/env python3
"""Tests for scripts/check_release.py. Run from the repository root:

    python3 -m unittest scripts/test_check_release.py

The registry is a local loopback HTTP server. The tests never use the public
registry or the network.
"""

import hashlib
import json
import sys
import tempfile
import threading
import unittest
import zipfile
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import check_release  # noqa: E402

NAME = "io.github.ThomasCrouzet/icloud-mcp"
DIGEST = "a" * 64
VERSION = "0.5.0"


def bundle_name(version, os_name, arch):
    return f"icloud-mcp-v{version}-{os_name}-{arch}.mcpb"


def package_entry(version=VERSION, os_name="linux", arch="amd64", digest=DIGEST,
                  identifier=None):
    if identifier is None:
        identifier = (
            "https://github.com/ThomasCrouzet/icloud-mcp/releases/download/"
            f"v{version}/{bundle_name(version, os_name, arch)}"
        )
    return {
        "registryType": "mcpb",
        "identifier": identifier,
        "version": version,
        "fileSha256": digest,
        "transport": {"type": "stdio"},
    }


def write_release(root, version=VERSION, changelog="", packages=None, remotes=None):
    server = {
        "$schema": "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json",
        "name": NAME,
        "title": "icloud-mcp",
        "description": "stdio MCP for iCloud Calendar (CalDAV), with optional Contacts and Mail.",
        "repository": {"url": "https://github.com/ThomasCrouzet/icloud-mcp", "source": "github"},
        "version": version,
    }
    if packages is not None:
        server["packages"] = packages
    if remotes is not None:
        server["remotes"] = remotes
    (root / "server.json").write_text(json.dumps(server), encoding="utf-8")
    body = changelog or "### Added\n- A verified change.\n"
    (root / "CHANGELOG.md").write_text(
        f"# Changelog\n\n## [{version}] - 2026-11-05\n{body}\n", encoding="utf-8"
    )


def write_bundle(root, filename, version=VERSION, os_name="linux", arch="amd64"):
    platform = {"linux": "linux", "darwin": "darwin", "windows": "win32"}[os_name]
    entry = "server/icloud-mcp.exe" if os_name == "windows" else "server/icloud-mcp"
    manifest = {
        "manifest_version": "0.4",
        "name": "icloud-mcp",
        "version": version,
        "description": "test",
        "author": {"name": "test"},
        "compatibility": {"platforms": [platform]},
        "server": {"type": "binary", "entry_point": entry},
    }
    path = root / filename
    with zipfile.ZipFile(path, "w", compression=zipfile.ZIP_DEFLATED) as archive:
        archive.writestr("manifest.json", json.dumps(manifest))
    return path


def digest_of(path):
    return hashlib.sha256(path.read_bytes()).hexdigest()


class RegistryHandler(BaseHTTPRequestHandler):
    version = VERSION
    packages = None
    error = None

    def do_GET(self):
        if self.error:
            self.send_error(self.error)
            return
        server = {"name": NAME, "version": self.version}
        if self.packages is not None:
            server["packages"] = self.packages
        payload = {
            "servers": [
                {
                    "server": server,
                    "_meta": {
                        "io.modelcontextprotocol.registry/official": {
                            "isLatest": True,
                            "publishedAt": "2026-11-05T00:00:00Z",
                        }
                    },
                }
            ]
        }
        data = json.dumps(payload).encode("utf-8")
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_args):
        pass


class ReleaseCheckTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        write_release(self.root)
        self.server = ThreadingHTTPServer(("127.0.0.1", 0), RegistryHandler)
        self.thread = threading.Thread(target=self.server.serve_forever, daemon=True)
        self.thread.start()
        self.registry = f"http://127.0.0.1:{self.server.server_port}/v0/servers"
        RegistryHandler.version = VERSION
        RegistryHandler.packages = None
        RegistryHandler.error = None

    def tearDown(self):
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
        self.temp.cleanup()

    def test_pending_release_passes_local_metadata(self):
        # The next version need not exist in the registry or as a Git tag.
        check_release.validate_metadata(self.root)
        check_release.validate_metadata(self.root, tag=f"v{VERSION}")

    def test_tag_mismatch_fails(self):
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_metadata(self.root, tag="v0.4.3")

    def test_complete_publication_passes(self):
        self.assertEqual(check_release.validate_registry(self.root, self.registry), VERSION)

    def test_registry_mismatch_fails(self):
        RegistryHandler.version = "0.4.3"
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_registry(self.root, self.registry)

    def test_registry_unavailable_fails_closed(self):
        self.server.shutdown()
        self.server.server_close()
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_registry(self.root, self.registry)
        self.server = None

    def test_missing_or_empty_changelog_fails(self):
        write_release(self.root, changelog="\n")
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_metadata(self.root)
        write_release(self.root, version="0.5.0", changelog="text")
        text = (self.root / "CHANGELOG.md").read_text(encoding="utf-8")
        (self.root / "CHANGELOG.md").write_text(text.replace("0.5.0", "0.6.0"), encoding="utf-8")
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_metadata(self.root)

    def test_invalid_server_metadata_fails(self):
        data = json.loads((self.root / "server.json").read_text(encoding="utf-8"))
        data["version"] = "next"
        (self.root / "server.json").write_text(json.dumps(data), encoding="utf-8")
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_metadata(self.root)

    def test_packages_metadata_passes(self):
        write_release(self.root, packages=[package_entry()])
        server, _ = check_release.validate_metadata(self.root)
        self.assertEqual(len(server["packages"]), 1)

    def test_invalid_packages_fail(self):
        cases = [
            [package_entry(identifier="http://example.invalid/a.mcpb")],
            [package_entry(identifier="https://example.invalid/a.zip")],
            [package_entry(identifier="https://example.invalid/no-extension")],
            [dict(package_entry(), registryType="npm")],
            [dict(package_entry(), version="0.5.1")],
            [dict(package_entry(), fileSha256="A" * 64)],
            [dict(package_entry(), fileSha256="a" * 63)],
            [dict(package_entry(), transport={"type": "streamable-http"})],
            [package_entry(), package_entry()],
        ]
        for index, packages in enumerate(cases):
            write_release(self.root, packages=packages)
            with self.subTest(index=index):
                with self.assertRaises(check_release.MetadataError):
                    check_release.validate_metadata(self.root)
        write_release(self.root, packages=[], )
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_metadata(self.root)

    def test_packages_cannot_mix_with_remotes(self):
        write_release(
            self.root,
            packages=[package_entry()],
            remotes=[{"type": "streamable-http", "url": "https://example.invalid/mcp"}],
        )
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_metadata(self.root)

    def test_packages_command_passes(self):
        dist = self.root / "dist"
        dist.mkdir()
        entries = []
        for os_name, arch in (("linux", "amd64"), ("linux", "arm64"), ("darwin", "arm64")):
            path = write_bundle(dist, bundle_name(VERSION, os_name, arch), os_name=os_name, arch=arch)
            entries.append(package_entry(os_name=os_name, arch=arch, digest=digest_of(path)))
        write_release(self.root, packages=entries)
        bundles = check_release.validate_packages_dir(self.root, "dist")
        self.assertEqual(len(bundles), 3)
        self.assertEqual(
            {bundle["platform"] for bundle in bundles},
            {"linux/amd64", "linux/arm64", "darwin/arm64"},
        )
        alternate = self.root / "alternate-server.json"
        alternate.write_text(
            (self.root / "server.json").read_text(encoding="utf-8"), encoding="utf-8"
        )
        self.assertEqual(
            len(check_release.validate_packages_dir(
                self.root, "dist", server_path=alternate
            )),
            3,
        )

    def test_packages_command_fails_closed(self):
        dist = self.root / "dist"
        dist.mkdir()
        path = write_bundle(dist, bundle_name(VERSION, "linux", "amd64"))
        good = package_entry(digest=digest_of(path))
        write_release(self.root, packages=[good])
        check_release.validate_packages_dir(self.root, "dist")

        write_release(self.root, packages=[dict(good, fileSha256="b" * 64)])
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_packages_dir(self.root, "dist")

        write_release(self.root, packages=[package_entry(os_name="darwin", arch="arm64")])
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_packages_dir(self.root, "dist")

        write_release(self.root, packages=[dict(good, identifier=good["identifier"].replace(".mcpb", ".zip"))])
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_metadata(self.root)

    def test_packages_command_rejects_wrong_bundle_manifest(self):
        dist = self.root / "dist"
        dist.mkdir()
        path = write_bundle(dist, bundle_name(VERSION, "linux", "amd64"), version="0.5.1")
        write_release(self.root, packages=[package_entry(digest=digest_of(path))])
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_packages_dir(self.root, "dist")

        path = write_bundle(dist, bundle_name(VERSION, "linux", "amd64"), os_name="darwin")
        write_release(self.root, packages=[package_entry(digest=digest_of(path))])
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_packages_dir(self.root, "dist")

    def test_registry_packages_match(self):
        packages = [package_entry()]
        write_release(self.root, packages=packages)
        RegistryHandler.packages = packages
        self.assertEqual(check_release.validate_registry(self.root, self.registry), VERSION)

    def test_registry_packages_mismatch_fails(self):
        packages = [package_entry()]
        write_release(self.root, packages=packages)

        RegistryHandler.packages = [dict(packages[0], fileSha256="b" * 64)]
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_registry(self.root, self.registry)

        RegistryHandler.packages = None
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_registry(self.root, self.registry)

        RegistryHandler.packages = []
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_registry(self.root, self.registry)

        RegistryHandler.packages = packages + [
            package_entry(os_name="linux", arch="arm64", digest="c" * 64)
        ]
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_registry(self.root, self.registry)

        RegistryHandler.packages = [
            package_entry(identifier=packages[0]["identifier"].replace("linux-amd64", "linux-arm64"))
        ]
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_registry(self.root, self.registry)


if __name__ == "__main__":
    unittest.main()
