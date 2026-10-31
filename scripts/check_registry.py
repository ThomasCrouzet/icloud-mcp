#!/usr/bin/env python3
"""Fail closed when the latest tag, server.json, changelog, and registry differ.

The registry is the public MCP Registry at registry.modelcontextprotocol.io.
Run this command from the repository root:

    python3 scripts/check_registry.py

Exit status is zero only when all four locations report the same version.
Network, parsing, and missing-data errors are failures, not warnings.
"""

import argparse
import json
import re
import subprocess
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

DEFAULT_REGISTRY = "https://registry.modelcontextprotocol.io/v0/servers"
DEFAULT_NAME = "io.github.ThomasCrouzet/icloud-mcp"


def fail(message):
    print("release metadata: " + message, file=sys.stderr)
    return 1


def latest_tag():
    try:
        result = subprocess.run(
            ["git", "describe", "--tags", "--abbrev=0", "HEAD"],
            check=True,
            capture_output=True,
            text=True,
        )
    except (subprocess.CalledProcessError, FileNotFoundError) as exc:
        raise SystemExit(f"release metadata: cannot read the latest Git tag: {exc}")
    return result.stdout.strip()


def server_version():
    try:
        data = json.loads(Path("server.json").read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise SystemExit(f"release metadata: cannot read server.json: {exc}")
    version = data.get("version")
    if not isinstance(version, str) or not version:
        raise SystemExit("release metadata: server.json has no version")
    return version


def changelog_version(version):
    try:
        text = Path("CHANGELOG.md").read_text(encoding="utf-8")
    except OSError as exc:
        raise SystemExit(f"release metadata: cannot read CHANGELOG.md: {exc}")
    pattern = re.compile(
        rf"^## \[{re.escape(version)}\] - \d{{4}}-\d{{2}}-\d{{2}}\n(.*?)(?=^## |\Z)",
        re.MULTILINE | re.DOTALL,
    )
    section = pattern.search(text)
    if section is None:
        return False
    return bool(section.group(1).strip())


def registry_versions(name, registry):
    query = urllib.parse.urlencode({"search": name})
    request = urllib.request.Request(
        f"{registry}?{query}",
        headers={"Accept": "application/json", "User-Agent": "icloud-mcp-release-check"},
    )
    try:
        with urllib.request.urlopen(request, timeout=20) as response:
            data = json.load(response)
    except (urllib.error.URLError, TimeoutError, json.JSONDecodeError) as exc:
        raise SystemExit(f"release metadata: cannot read the registry: {exc}")

    versions = []
    for entry in data.get("servers", []):
        server = entry.get("server", {})
        if server.get("name") != name:
            continue
        version = server.get("version")
        meta = entry.get("_meta", {}).get(
            "io.modelcontextprotocol.registry/official", {}
        )
        if isinstance(version, str) and version:
            versions.append((version, meta))
    return versions


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--registry", default=DEFAULT_REGISTRY)
    parser.add_argument("--name", default=DEFAULT_NAME)
    args = parser.parse_args()

    tag = latest_tag()
    tag_version = tag[1:] if tag.startswith("v") else tag
    declared = server_version()
    versions = registry_versions(args.name, args.registry)

    failures = 0
    if not changelog_version(declared):
        failures += fail(f"CHANGELOG.md has no non-empty section for {declared}")

    if tag_version != declared:
        failures += fail(
            f"latest tag {tag} does not match server.json version {declared}"
        )

    if not versions:
        failures += fail(f"registry has no entry for {args.name}")
    else:
        latest = None
        for version, meta in versions:
            if meta.get("isLatest"):
                latest = version
                break
        if latest is None:
            latest = max(
                versions, key=lambda item: str(item[1].get("publishedAt", ""))
            )[0]
        if latest != declared:
            failures += fail(
                f"registry version {latest} does not match server.json version {declared}"
            )

    if failures:
        print(
            f"release metadata: {failures} mismatch(es); "
            "tag, server.json, changelog, and registry must agree",
            file=sys.stderr,
        )
        return 1

    print(
        f"release metadata: tag {tag}, server.json {declared}, "
        f"changelog {declared}, registry {declared} agree"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
