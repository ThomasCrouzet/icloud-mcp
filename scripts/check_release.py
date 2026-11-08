#!/usr/bin/env python3
"""Validate release metadata or the published MCP Registry state.

Commands:
    metadata   Validate server.json and the matching CHANGELOG.md section.
    packages   Verify built .mcpb files against server.json package hashes.
    registry   Validate the published registry version without changing files.

Run from the repository root. Exit status is zero only on success. Metadata
validation does not use the network. Registry validation fails closed on a
missing record, a mismatch, or an unreadable response.
"""

import argparse
import hashlib
import json
import os
import re
import sys
import urllib.error
import urllib.parse
import urllib.request
import zipfile
from pathlib import Path, PurePosixPath

DEFAULT_REGISTRY = "https://registry.modelcontextprotocol.io/v0/servers"
DEFAULT_NAME = "io.github.ThomasCrouzet/icloud-mcp"
VERSION_PATTERN = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+(?:[-.][0-9A-Za-z.-]+)?$")
NAME_PATTERN = re.compile(r"^[A-Za-z0-9.-]+/[A-Za-z0-9._-]+$")
SHA256_PATTERN = re.compile(r"^[a-f0-9]{64}$")
PLATFORM_PATTERN = re.compile(r"-(linux|darwin|windows)-(amd64|arm64)\.mcpb$")


class MetadataError(Exception):
    pass


def validate_packages(server):
    packages = server.get("packages")
    if packages is None:
        return []
    if not isinstance(packages, list) or not packages:
        raise MetadataError("server.json packages must be a non-empty array")
    if server.get("remotes"):
        raise MetadataError("server.json must not declare remotes with packages")

    seen = set()
    for package in packages:
        if not isinstance(package, dict):
            raise MetadataError("server.json packages entries must be objects")
        if package.get("registryType") != "mcpb":
            raise MetadataError("server.json package registryType must be mcpb")
        identifier = package.get("identifier")
        if not isinstance(identifier, str) or not identifier.startswith("https://"):
            raise MetadataError("server.json package identifier must be an https URL")
        parsed = urllib.parse.urlparse(identifier)
        if parsed.scheme != "https" or not parsed.netloc:
            raise MetadataError("server.json package identifier must be a valid URL")
        if not parsed.path.endswith(".mcpb"):
            raise MetadataError("server.json package identifier must end in .mcpb")
        if "mcp" not in identifier.lower():
            raise MetadataError("server.json package identifier must contain mcp")
        if identifier in seen:
            raise MetadataError("server.json package identifiers must be unique")
        seen.add(identifier)

        if package.get("version") != server["version"]:
            raise MetadataError(
                "server.json package version must equal the server version"
            )
        digest = package.get("fileSha256")
        if not isinstance(digest, str) or not SHA256_PATTERN.match(digest):
            raise MetadataError(
                "server.json package fileSha256 must be 64 lowercase hex characters"
            )
        transport = package.get("transport")
        if not isinstance(transport, dict) or transport.get("type") != "stdio":
            raise MetadataError("server.json package transport type must be stdio")
    return packages


def read_server(root, server_path=None):
    path = Path(server_path) if server_path else root / "server.json"
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
    validate_packages(data)
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


def validate_metadata(root, tag="", name=DEFAULT_NAME, server_path=None):
    server = read_server(root, server_path)
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
            versions.append((version, meta, server))
    return versions


def latest_registry_version(versions, name):
    if not versions:
        raise MetadataError(f"registry has no entry for {name}")
    for version, meta, _server in versions:
        if meta.get("isLatest"):
            return version, meta, _server
    latest = max(
        versions, key=lambda item: str(item[1].get("publishedAt", ""))
    )
    return latest[0], latest[1], latest[2]


def validate_registry(
    root, registry=DEFAULT_REGISTRY, name=DEFAULT_NAME, server_path=None
):
    server, _ = validate_metadata(root, name=name, server_path=server_path)
    version, _meta, published = latest_registry_version(
        registry_versions(name, registry), name
    )
    if version != server["version"]:
        raise MetadataError(
            f"registry version {version} does not match server.json version {server['version']}"
        )
    local_packages = server.get("packages") or []
    if local_packages:
        published_packages = published.get("packages")
        if not isinstance(published_packages, list) or not published_packages:
            raise MetadataError("registry latest version has no packages")
        expected = sorted(
            (package["identifier"], package["fileSha256"]) for package in local_packages
        )
        actual = sorted(
            (package.get("identifier"), package.get("fileSha256"))
            for package in published_packages
            if isinstance(package, dict)
        )
        if len(actual) != len(published_packages):
            raise MetadataError("registry latest version has an invalid package entry")
        if expected != actual:
            raise MetadataError(
                "registry packages do not match server.json identifiers and hashes"
            )
    return server["version"]


def sha256_file(path):
    digest = hashlib.sha256()
    with path.open("rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def package_platform(identifier):
    filename = PurePosixPath(urllib.parse.urlparse(identifier).path).name
    match = PLATFORM_PATTERN.search(filename)
    if match is None:
        raise MetadataError(
            f"package identifier does not name a supported platform: {filename}"
        )
    return filename, match.group(1), match.group(2)


def bundle_manifest(path):
    try:
        with zipfile.ZipFile(path) as archive:
            data = archive.read("manifest.json")
    except (OSError, KeyError, zipfile.BadZipFile) as exc:
        raise MetadataError(f"cannot read bundle manifest {path}: {exc}") from exc
    try:
        manifest = json.loads(data.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise MetadataError(f"bundle manifest is not JSON: {path}: {exc}") from exc
    if not isinstance(manifest, dict):
        raise MetadataError(f"bundle manifest must be an object: {path}")
    return manifest


def validate_packages_dir(root, dist, name=DEFAULT_NAME, server_path=None):
    server, _ = validate_metadata(root, name=name, server_path=server_path)
    packages = server.get("packages")
    if not packages:
        raise MetadataError("server.json has no packages to verify")
    dist_dir = Path(dist)
    if not dist_dir.is_absolute():
        dist_dir = root / dist_dir

    seen = set()
    bundles = []
    platform_names = {"linux": "linux", "darwin": "darwin", "windows": "win32"}
    for package in packages:
        identifier = package["identifier"]
        filename, os_name, arch = package_platform(identifier)
        platform = (os_name, arch)
        if platform in seen:
            raise MetadataError(f"server.json claims platform twice: {os_name}/{arch}")
        seen.add(platform)
        path = dist_dir / filename
        if not path.is_file():
            raise MetadataError(f"missing bundle for claimed platform: {path}")
        actual_sha = sha256_file(path)
        if actual_sha != package["fileSha256"]:
            raise MetadataError(
                f"bundle hash does not match server.json: {path}"
            )
        manifest = bundle_manifest(path)
        if manifest.get("version") != server["version"]:
            raise MetadataError(f"bundle version does not match server.json: {path}")
        compatibility = manifest.get("compatibility")
        platforms = (
            compatibility.get("platforms") if isinstance(compatibility, dict) else None
        )
        if platforms != [platform_names[os_name]]:
            raise MetadataError(f"bundle platform does not match its URL: {path}")
        entry = manifest.get("server", {}).get("entry_point")
        expected_entry = (
            "server/icloud-mcp.exe" if os_name == "windows" else "server/icloud-mcp"
        )
        if entry != expected_entry:
            raise MetadataError(f"bundle entry point is wrong: {path}")
        bundles.append(
            {
                "path": str(path),
                "platform": f"{os_name}/{arch}",
                "sha256": actual_sha,
                "version": manifest["version"],
            }
        )
    return bundles


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
    version = validate_registry(
        args.root, args.registry, args.name, getattr(args, "server", "")
    )
    print(f"release metadata: registry, server.json, and CHANGELOG.md agree on {version}")
    return 0


def command_packages(args):
    bundles = validate_packages_dir(
        args.root, args.dir, args.name, getattr(args, "server", None)
    )
    print(
        "release packages: "
        + ", ".join(
            f"{bundle['platform']} {bundle['sha256']}" for bundle in bundles
        )
        + " match server.json"
    )
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
    registry.add_argument("--server", default="", help="alternate server.json path")
    registry.set_defaults(func=command_registry)

    packages = subcommands.add_parser(
        "packages", help="verify built MCPB bundles against server.json"
    )
    packages.add_argument("--dir", default="dist", help="directory with .mcpb files")
    packages.add_argument("--server", default="", help="alternate server.json path")
    packages.set_defaults(func=command_packages)
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
