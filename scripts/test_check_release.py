#!/usr/bin/env python3
"""Tests for scripts/check_release.py. Run from the repository root:

    python3 -m unittest scripts/test_check_release.py

The registry is a local loopback HTTP server. The tests never use the public
registry or the network.
"""

import json
import sys
import tempfile
import threading
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import check_release  # noqa: E402

NAME = "io.github.ThomasCrouzet/icloud-mcp"


def write_release(root, version="0.5.0", changelog=""):
    server = {
        "$schema": "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json",
        "name": NAME,
        "title": "icloud-mcp",
        "description": "stdio MCP for iCloud Calendar (CalDAV), with optional Contacts and Mail.",
        "repository": {"url": "https://github.com/ThomasCrouzet/icloud-mcp", "source": "github"},
        "version": version,
    }
    (root / "server.json").write_text(json.dumps(server), encoding="utf-8")
    body = changelog or "### Added\n- A verified change.\n"
    (root / "CHANGELOG.md").write_text(
        f"# Changelog\n\n## [{version}] - 2026-11-05\n{body}\n", encoding="utf-8"
    )


class RegistryHandler(BaseHTTPRequestHandler):
    version = "0.5.0"
    error = None

    def do_GET(self):
        if self.error:
            self.send_error(self.error)
            return
        payload = {
            "servers": [
                {
                    "server": {"name": NAME, "version": self.version},
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
        RegistryHandler.version = "0.5.0"
        RegistryHandler.error = None

    def tearDown(self):
        if self.server is not None:
            self.server.shutdown()
            self.server.server_close()
        self.temp.cleanup()

    def test_pending_release_passes_local_metadata(self):
        # The next version need not exist in the registry or as a Git tag.
        check_release.validate_metadata(self.root)
        check_release.validate_metadata(self.root, tag="v0.5.0")

    def test_tag_mismatch_fails(self):
        with self.assertRaises(check_release.MetadataError):
            check_release.validate_metadata(self.root, tag="v0.4.3")

    def test_complete_publication_passes(self):
        self.assertEqual(check_release.validate_registry(self.root, self.registry), "0.5.0")

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


if __name__ == "__main__":
    unittest.main()
