#!/usr/bin/env python3
"""Tests for scripts/check_docs.py. Run from the repository root:

    python3 -m unittest scripts/test_check_docs.py

Every fixture is temporary and local. The tests use no network.
"""

import io
import json
import sys
import tempfile
import unittest
from contextlib import redirect_stderr
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import check_docs  # noqa: E402

ENV_EXAMPLE = "ICLOUD_EMAIL=\nICLOUD_MCP_READ_ONLY=true\n"
CONFIG_GO = '''package config

const (
	emailEnvVar    = "ICLOUD_EMAIL"
	readOnlyEnvVar = "ICLOUD_MCP_READ_ONLY"
)

func ParsePermissions(value string) (Permissions, error) {
	switch value {
	case "calendar.read":
	case "mail.archive":
	}
}

func (p Permissions) Configured() bool { return false }
'''
SCOPES_GO = '''package config

const (
	ResourceCalendarRead = "calendar.read"
	ResourceMailArchive  = "mail.archive"
)
'''
README = """# fixture

The binary reads exactly **2 product environment variables**.

| Variable | Purpose |
|---|---|
| `ICLOUD_EMAIL` | identity |
| `ICLOUD_MCP_READ_ONLY` | switch |

`ICLOUD_MCP_PERMISSIONS` accepts `calendar.read` and `mail.archive`.

`ICLOUD_MCP_RESOURCE_SCOPES` accepts `calendar.read` and `mail.archive`.

See [host guide](docs/agent-hosts.md).
"""
AGENT_HOSTS = """# Host guide

Exactly **2** product environment variables:

| Variable | Default |
|---|---|
| `ICLOUD_EMAIL` | none |
| `ICLOUD_MCP_READ_ONLY` | false |

It applies for all 2 product variables.

`ICLOUD_MCP_PERMISSIONS` accepts `calendar.read` and `mail.archive`.

`ICLOUD_MCP_RESOURCE_SCOPES` accepts `calendar.read` and `mail.archive`.
"""
ARCHITECTURE = "# Architecture\n\n`config.Load` reads and validates the 2 environment variables.\n"
CHANGELOG = "# Changelog\n\n## [0.5.0] - 2026-01-01\n\n### Added\n- Fixture.\n"
SERVER = {
    "$schema": "https://static.modelcontextprotocol.io/schemas/2025-12-11/server.schema.json",
    "name": "io.github.example/fixture",
    "title": "fixture",
    "description": "Fixture server.",
    "repository": {"url": "https://github.com/example/fixture", "source": "github"},
    "version": "0.5.0",
}


def make_repo(root):
    (root / ".env.example").write_text(ENV_EXAMPLE, encoding="utf-8")
    (root / "internal" / "config").mkdir(parents=True)
    (root / "internal" / "config" / "config.go").write_text(CONFIG_GO, encoding="utf-8")
    (root / "internal" / "config" / "scopes.go").write_text(SCOPES_GO, encoding="utf-8")
    (root / "docs").mkdir()
    (root / "README.md").write_text(README, encoding="utf-8")
    (root / "docs" / "agent-hosts.md").write_text(AGENT_HOSTS, encoding="utf-8")
    (root / "docs" / "architecture.md").write_text(ARCHITECTURE, encoding="utf-8")
    (root / "CHANGELOG.md").write_text(CHANGELOG, encoding="utf-8")
    (root / "server.json").write_text(json.dumps(SERVER), encoding="utf-8")


class CheckDocsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        make_repo(self.root)

    def tearDown(self):
        self.temp.cleanup()

    def assertFails(self, fragment):
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            status = check_docs.main([str(self.root)])
        self.assertEqual(status, 1)
        self.assertIn(fragment, stderr.getvalue())

    def test_fixture_passes(self):
        self.assertEqual(check_docs.main([str(self.root)]), 0)

    def test_env_source_disagreement_fails(self):
        (self.root / ".env.example").write_text("ICLOUD_EMAIL=\n", encoding="utf-8")
        self.assertFails("disagree on environment keys")

    def test_readme_table_missing_key_fails(self):
        text = README.replace("| `ICLOUD_MCP_READ_ONLY` | switch |\n", "")
        (self.root / "README.md").write_text(text, encoding="utf-8")
        self.assertFails("README.md environment table disagrees")

    def test_stated_count_mismatch_fails(self):
        text = README.replace("exactly **2 product", "exactly **3 product")
        (self.root / "README.md").write_text(text, encoding="utf-8")
        self.assertFails("states 3 environment variables")

    def test_missing_permission_token_fails(self):
        text = AGENT_HOSTS.replace("`mail.archive`", "mail archive")
        (self.root / "docs" / "agent-hosts.md").write_text(text, encoding="utf-8")
        self.assertFails("omits capability tokens")

    def test_missing_scope_action_fails(self):
        text = README.replace("accepts `calendar.read` and `mail.archive`", "accepts `calendar.read`")
        (self.root / "README.md").write_text(text, encoding="utf-8")
        self.assertFails("scopes: ['mail.archive']")

    def test_broken_relative_link_fails(self):
        (self.root / "README.md").write_text(
            README + "\nSee [missing](docs/missing.md).\n", encoding="utf-8"
        )
        self.assertFails("links to a missing target")

    def test_link_outside_repository_fails(self):
        (self.root / "README.md").write_text(
            README + "\nSee [outside](../outside.md).\n", encoding="utf-8"
        )
        self.assertFails("links outside the repository")

    def test_changelog_without_version_fails(self):
        (self.root / "CHANGELOG.md").write_text("# Changelog\n", encoding="utf-8")
        self.assertFails("no dated section for 0.5.0")


if __name__ == "__main__":
    unittest.main()
