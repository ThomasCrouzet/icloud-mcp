#!/usr/bin/env python3
"""Tests for scripts/check_platforms.py. Run from the repository root:

    python3 -m unittest scripts/test_check_platforms.py

Fixtures are temporary and local. The tests use no network.
"""

import io
import sys
import tempfile
import unittest
from contextlib import redirect_stderr
from pathlib import Path

sys.path.insert(0, str(Path(__file__).resolve().parent))
import check_platforms  # noqa: E402

MAKEFILE = "TARGETS  := linux/amd64 linux/arm64 darwin/arm64\n"
README = """# fixture

| Target | Published archive | Native release smoke | Status |
|--------|-------------------|----------------------|--------|
| Linux amd64 | Yes | Ubuntu runner | Supported |
| Linux arm64 | Yes | Ubuntu arm64 runner | Supported |
| macOS arm64 | Yes | macOS runner | Supported |
| Windows amd64 | No | None | Compile-only, explicit scope decision |
"""
PLATFORMS = """# Platforms

| Platform | Compiled in CI | Published archive | Archive verified | Native executed | Support claim |
|----------|----------------|-------------------|------------------|-----------------|---------------|
| `linux/amd64` | Yes | Yes | Yes | Yes, ubuntu | Supported |
| `linux/arm64` | Yes | Yes | Yes | Yes, ubuntu arm | Supported |
| `darwin/arm64` | Yes | Yes | Yes | Yes, macos | Supported |
| `windows/amd64` | Yes, compile smoke | No | No | No | Not supported, compile-only |
"""
CI = """jobs:
  native-release-smoke:
    strategy:
      matrix:
        include:
          - runner: ubuntu-latest
            asset_os: linux
            asset_arch: amd64
          - runner: ubuntu-24.04-arm
            asset_os: linux
            asset_arch: arm64
          - runner: macos-15
            asset_os: darwin
            asset_arch: arm64
          - target: windows/amd64
"""


def make_repo(root):
    (root / "docs").mkdir()
    (root / ".github" / "workflows").mkdir(parents=True)
    (root / "Makefile").write_text(MAKEFILE, encoding="utf-8")
    (root / "README.md").write_text(README, encoding="utf-8")
    (root / "docs" / "platform-support.md").write_text(PLATFORMS, encoding="utf-8")
    (root / ".github" / "workflows" / "ci.yml").write_text(CI, encoding="utf-8")


class CheckPlatformsTest(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.root = Path(self.temp.name)
        make_repo(self.root)

    def tearDown(self):
        self.temp.cleanup()

    def assertFails(self, fragment):
        stderr = io.StringIO()
        with redirect_stderr(stderr):
            status = check_platforms.main([str(self.root)])
        self.assertEqual(status, 1)
        self.assertIn(fragment, stderr.getvalue())

    def test_fixture_passes(self):
        self.assertEqual(check_platforms.main([str(self.root)]), 0)

    def test_makefile_target_missing_from_ci_fails(self):
        text = CI.replace("            asset_os: linux\n            asset_arch: arm64\n", "")
        (self.root / ".github" / "workflows" / "ci.yml").write_text(text, encoding="utf-8")
        self.assertFails("CI native matrix disagrees")

    def test_readme_missing_published_target_fails(self):
        text = README.replace("| Linux arm64 | Yes | Ubuntu arm64 runner | Supported |\n", "")
        (self.root / "README.md").write_text(text, encoding="utf-8")
        self.assertFails("README status table omits published targets")

    def test_readme_claims_windows_archive_fails(self):
        text = README.replace(
            "| Windows amd64 | No | None |", "| Windows amd64 | Yes | None |"
        )
        (self.root / "README.md").write_text(text, encoding="utf-8")
        self.assertFails("Windows archive")

    def test_platform_page_missing_native_claim_fails(self):
        text = PLATFORMS.replace(
            "| `linux/arm64` | Yes | Yes | Yes | Yes, ubuntu arm | Supported |",
            "| `linux/arm64` | Yes | Yes | Yes | No | Supported |",
        )
        (self.root / "docs" / "platform-support.md").write_text(text, encoding="utf-8")
        self.assertFails("does not claim native executed")

    def test_platform_page_missing_windows_decision_fails(self):
        text = PLATFORMS.replace("Not supported, compile-only", "Supported")
        (self.root / "docs" / "platform-support.md").write_text(text, encoding="utf-8")
        self.assertFails("does not record the Windows decision")

    def test_ci_without_windows_compile_fails(self):
        text = CI.replace("          - target: windows/amd64\n", "")
        (self.root / ".github" / "workflows" / "ci.yml").write_text(text, encoding="utf-8")
        self.assertFails("windows/amd64")


if __name__ == "__main__":
    unittest.main()
