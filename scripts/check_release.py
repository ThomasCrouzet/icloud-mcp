#!/usr/bin/env python3
"""Validate release metadata or the published MCP Registry state.

Commands:
    metadata   Validate server.json and the matching CHANGELOG.md section.
    registry   Validate the published registry version without changing files.

Run from the repository root. Exit status is zero only on success. Metadata
validation does not use the network. Registry validation fails closed on a
missing record, a mismatch, or an unreadable response.
"""

import argparse
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
from pathlib import Path

DEFAULT_REGISTRY = "https://registry.modelcontextprotocol.io/v0/servers"
DEFAULT_NAME = "io.github.ThomasCrouzet/icloud-mcp"
VERSION_PATTERN = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+(?:[-.][0-9A-Za-z.-]+)?$")
NAME_PATTERN = re.compile(r"^[A-Za-z0-9.-]+/[A-Za-z0-9._-]+$")


class MetadataError(Exception):
    pass


def read_server(root):
    path = root / "server.json"
    try:
        data = json.loads(path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise MetadataError(f"cannot read server.json: {exc}") from exc
    if not isinstance(data, dict):
        raise MetadataError("server.json must contain an object")

    name = data.get("name")
    if not isinstance(name, str) or not NAME_PATTERN.match(name):
        raise MetadataError("server.json name must use the namespace/name form")
    version = data.get("version")
    if not isinstance(version, str) or not VERSION_PATTERN.match(version):
        raise MetadataError("server.json version must be a semantic version")
    description = data.get("description")
    if not isinstance(description, str) or not description.strip():
        raise MetadataError("server.json description must not be empty")
    if len(description) > 100:
        raise MetadataError("server.json description must be 100 characters or fewer")
    title = data.get("title")
    if not isinstance(title, str) or not title.strip():
        raise MetadataError("server.json title must not be empty")
    schema = data.get("$schema")
    if not isinstance(schema, str) or not schema.startswith("https://"):
        raise MetadataError("server.json $schema must be an https URL")
    repository = data.get("repository")
    if not isinstance(repository, dict):
        raise MetadataError("server.json repository must be an object")
    url = repository.get("url")
    source = repository.get("source")
    if not isinstance(url, str) or not url.startswith("https://"):
        raise MetadataError("server.json repository.url must be an https URL")
    if not isinstance(source, str) or not source.strip():
        raise MetadataError("server.json repository.source must not be empty")
    return data


def changelog_section(root, version):
    path = root / "CHANGELOG.md"
    try:
        text = path.read_text(encoding="utf-8")
    except OSError as exc:
        raise MetadataError(f"cannot read CHANGELOG.md: {exc}") from exc
    pattern = re.compile(
        rf"^## \[{re.escape(version)}\] - \d{{4}}-\d{{2}}-\d{{2}}\n(.*?)(?=^## |\Z)",
        re.MULTILINE | re.DOTALL,
    )
    match = pattern.search(text)
    if match is None:
        raise MetadataError(f"CHANGELOG.md has no dated section for {version}")
    section = match.group(1).strip()
    if not section:
        raise MetadataError(f"CHANGELOG.md section for {version} is empty")
    return section


def expected_tag():
    if os.environ.get("GITHUB_REF_TYPE") == "tag":
        return os.environ.get("GITHUB_REF_NAME", "")
    return ""


def validate_metadata(root, tag="", name=DEFAULT_NAME):
    server = read_server(root)
    section = changelog_section(root, server["version"])
    if server["name"] != name:
        raise MetadataError(
            f"server.json name {server['name']} does not match expected {name}"
        )
    if tag and tag != f"v{server['version']}":
        raise MetadataError(
            f"release tag {tag} does not match server.json version {server['version']}"
        )
    return server, section


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
        raise MetadataError(f"cannot read the registry: {exc}") from exc

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


def latest_registry_version(versions, name):
    if not versions:
        raise MetadataError(f"registry has no entry for {name}")
    for version, meta in versions:
        if meta.get("isLatest"):
            return version
    return max(versions, key=lambda item: str(item[1].get("publishedAt", "")))[0]


def validate_registry(root, registry=DEFAULT_REGISTRY, name=DEFAULT_NAME):
    server, _ = validate_metadata(root, name=name)
    latest = latest_registry_version(registry_versions(name, registry), name)
    if latest != server["version"]:
        raise MetadataError(
            f"registry version {latest} does not match server.json version {server['version']}"
        )
    return server["version"]


def command_metadata(args):
    tag = args.tag or expected_tag()
    _, section = validate_metadata(args.root, tag, args.name)
    if args.notes_out:
        Path(args.notes_out).write_text(section + "\n", encoding="utf-8")
    print(
        f"release metadata: server.json and CHANGELOG.md agree on "
        f"{read_server(args.root)['version']}"
    )
    return 0


def command_registry(args):
    version = validate_registry(args.root, args.registry, args.name)
    print(f"release metadata: registry, server.json, and CHANGELOG.md agree on {version}")
    return 0


def build_parser():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--root", type=Path, default=Path("."))
    parser.add_argument("--name", default=DEFAULT_NAME)
    subcommands = parser.add_subparsers(dest="command", required=True)

    metadata = subcommands.add_parser("metadata", help="validate local release files")
    metadata.add_argument("--tag", default="", help="expected vX.Y.Z tag")
    metadata.add_argument("--notes-out", default="", help="write the changelog section")
    metadata.set_defaults(func=command_metadata)

    registry = subcommands.add_parser("registry", help="verify published registry state")
    registry.add_argument("--registry", default=DEFAULT_REGISTRY)
    registry.set_defaults(func=command_registry)
    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except MetadataError as exc:
        print(f"release metadata: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
