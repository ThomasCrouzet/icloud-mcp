#!/usr/bin/env python3
"""Build and verify deterministic MCPB bundles for icloud-mcp.

Commands:
    build      Create one platform bundle from a release archive or directory.
    verify     Validate a bundle, then run the bundled binary with -version.
    manifest   Print the generated manifest v0.4 for inspection.

The bundle contains only the native executable, LICENSE,
THIRD_PARTY_NOTICES.md, and manifest.json. It contains no JavaScript runtime
and no credentials. Run this script from the repository root.
"""

import argparse
import hashlib
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile
import zipfile
from pathlib import Path, PurePosixPath

MANIFEST_VERSION = "0.4"
PROJECT = "icloud-mcp"
AUTHOR = "Thomas Crouzet"
REPOSITORY_URL = "https://github.com/ThomasCrouzet/icloud-mcp"
SUPPORTED_TARGETS = ("linux/amd64", "linux/arm64", "darwin/arm64")
WINDOWS_TARGET = "windows/amd64"
TEXT_FILES = ("LICENSE", "THIRD_PARTY_NOTICES.md")
ZIP_TIMESTAMP = (1980, 1, 1, 0, 0, 0)
MAX_MANIFEST_BYTES = 64 * 1024
MAX_BUNDLE_MEMBER_BYTES = 64 * 1024 * 1024
VERSION_PATTERN = re.compile(r"^[0-9]+\.[0-9]+\.[0-9]+(?:[-.][0-9A-Za-z.-]+)?$")
PLATFORM_NAMES = {"linux": "linux", "darwin": "darwin", "windows": "win32"}

CREDENTIAL_PATTERNS = (
    re.compile(r"-----BEGIN [A-Z ]*PRIVATE KEY-----"),
    re.compile(r"\b(?:ghp|gho|ghu|ghs|ghr)_[A-Za-z0-9]{20,}"),
    re.compile(r"\bgithub_pat_[A-Za-z0-9_]{20,}"),
    re.compile(r"\bAKIA[0-9A-Z]{16}\b"),
    re.compile(r"\bxox[baprs]-[A-Za-z0-9-]{10,}"),
    re.compile(r"\bsk-[A-Za-z0-9]{20,}"),
    re.compile(r"\bAIza[0-9A-Za-z_-]{35}\b"),
    re.compile(
        r"(?i)\b(password|passwd|secret|token|api[_-]?key)\b\s*[:=]\s*"
        r"[\"']?[A-Za-z0-9_./+=-]{8,}"
    ),
    re.compile(r"\b[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}-[a-z0-9]{4}\b"),
)


class BundleError(Exception):
    pass


def normalize_version(value):
    text = str(value).strip()
    if text[:1] in ("v", "V"):
        text = text[1:]
    if not VERSION_PATTERN.match(text):
        raise BundleError(f"version must be a release version, got {value!r}")
    return text


def release_name(version):
    return f"v{normalize_version(version)}"


def parse_target(target, allow_untested_windows=False):
    value = str(target).strip()
    if value == WINDOWS_TARGET:
        if not allow_untested_windows:
            raise BundleError(
                "windows/amd64 bundles are not tested; pass "
                "--allow-untested-windows only for an explicit experiment"
            )
        return "windows", "amd64"
    if value not in SUPPORTED_TARGETS:
        joined = ", ".join(SUPPORTED_TARGETS)
        raise BundleError(f"unsupported target {value!r}; use one of {joined}")
    os_name, arch = value.split("/", 1)
    return os_name, arch


def binary_name(os_name):
    return f"{PROJECT}.exe" if os_name == "windows" else PROJECT


def bundle_filename(version, os_name, arch):
    return f"{PROJECT}-{release_name(version)}-{os_name}-{arch}.mcpb"


def manifest_for(target, version, allow_untested_windows=False):
    os_name, arch = parse_target(target, allow_untested_windows)
    normalized = normalize_version(version)
    entry = f"server/{binary_name(os_name)}"
    platform = PLATFORM_NAMES[os_name]

    user_config = {
        "icloud_email": {
            "type": "string",
            "title": "Apple Account email",
            "description": (
                "Apple Account email for Calendar and Contacts. Mail-only "
                "profiles use the Mail address instead. A file:// value is allowed."
            ),
            "required": False,
            "sensitive": False,
        },
        "icloud_password": {
            "type": "string",
            "title": "Calendar and Contacts app-specific password",
            "description": (
                "App-specific password for Calendar and Contacts. Do not use the "
                "main Apple Account password. A file:// value is allowed."
            ),
            "required": False,
            "sensitive": True,
        },
        "read_only": {
            "type": "string",
            "title": "Global read-only",
            "description": (
                "true removes every mutation and send tool. Keep true for the "
                "first run. The Calendar read-only profile is the install default."
            ),
            "required": False,
            "default": "true",
            "sensitive": False,
        },
        "permissions": {
            "type": "string",
            "title": "Capability list",
            "description": (
                "Optional comma-separated capabilities, for example "
                "calendar.read,calendar.write. Empty uses the documented switches."
            ),
            "required": False,
            "sensitive": False,
        },
        "resource_scopes": {
            "type": "string",
            "title": "Resource scopes",
            "description": (
                "Optional semicolon-separated action=value scopes, for example "
                "calendar.read=/cal/allowed/."
            ),
            "required": False,
            "sensitive": False,
        },
        "default_timezone": {
            "type": "string",
            "title": "Default timezone",
            "description": "IANA timezone for Calendar values without an offset.",
            "required": False,
            "default": "UTC",
            "sensitive": False,
        },
        "enable_contacts": {
            "type": "string",
            "title": "Enable Contacts",
            "description": (
                "true enables Contacts. Writes follow read_only and permissions."
            ),
            "required": False,
            "default": "false",
            "sensitive": False,
        },
        "enable_mail": {
            "type": "string",
            "title": "Enable Mail reads",
            "description": "true enables Mail reads. Requires the Mail address.",
            "required": False,
            "default": "false",
            "sensitive": False,
        },
        "mail_address": {
            "type": "string",
            "title": "iCloud Mail address",
            "description": "Full iCloud Mail address. Used when Mail is enabled.",
            "required": False,
            "sensitive": False,
        },
        "mail_password": {
            "type": "string",
            "title": "Mail app-specific password",
            "description": (
                "App-specific password for Mail. If empty, the server uses the "
                "Calendar and Contacts password. A file:// value is allowed."
            ),
            "required": False,
            "sensitive": True,
        },
        "enable_mail_write": {
            "type": "string",
            "title": "Enable Mail mutation",
            "description": "true enables Mail flag, move, and trash tools.",
            "required": False,
            "default": "false",
            "sensitive": False,
        },
        "enable_mail_send": {
            "type": "string",
            "title": "Enable Mail send",
            "description": (
                "true enables SMTP send tools. Requires exact recipient policy."
            ),
            "required": False,
            "default": "false",
            "sensitive": False,
        },
        "smtp_allowed_recipients": {
            "type": "string",
            "title": "Allowed recipients",
            "description": (
                "Exact plain recipient addresses, comma-separated. A literal * "
                "is unsafe and is not recommended."
            ),
            "required": False,
            "sensitive": False,
        },
    }

    env = {
        "ICLOUD_EMAIL": "${user_config.icloud_email}",
        "ICLOUD_PASSWORD": "${user_config.icloud_password}",
        "ICLOUD_MCP_READ_ONLY": "${user_config.read_only}",
        "ICLOUD_MCP_PERMISSIONS": "${user_config.permissions}",
        "ICLOUD_MCP_RESOURCE_SCOPES": "${user_config.resource_scopes}",
        "ICLOUD_MCP_DEFAULT_TZ": "${user_config.default_timezone}",
        "ICLOUD_MCP_ENABLE_CONTACTS": "${user_config.enable_contacts}",
        "ICLOUD_MCP_ENABLE_MAIL": "${user_config.enable_mail}",
        "ICLOUD_MAIL_ADDRESS": "${user_config.mail_address}",
        "ICLOUD_MAIL_PASSWORD": "${user_config.mail_password}",
        "ICLOUD_MCP_ENABLE_MAIL_WRITE": "${user_config.enable_mail_write}",
        "ICLOUD_MCP_ENABLE_MAIL_SEND": "${user_config.enable_mail_send}",
        "ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS": (
            "${user_config.smtp_allowed_recipients}"
        ),
    }

    return {
        "manifest_version": MANIFEST_VERSION,
        "name": PROJECT,
        "display_name": "icloud-mcp",
        "version": normalized,
        "description": (
            "stdio MCP for iCloud Calendar (CalDAV), with optional Contacts and Mail."
        ),
        "long_description": (
            "Use one native Go process for iCloud Calendar, optional Contacts, "
            "and optional Mail. The Calendar read-only profile is the install "
            "default. Documented profiles are environment choices: Calendar "
            "read-only, Calendar plus Contacts read-only, Calendar scoped write, "
            "Mail read-only, and Mail exact send."
        ),
        "author": {"name": AUTHOR},
        "repository": {"type": "git", "url": REPOSITORY_URL},
        "homepage": f"{REPOSITORY_URL}#quick-start",
        "documentation": f"{REPOSITORY_URL}/blob/main/docs/agent-hosts.md",
        "support": f"{REPOSITORY_URL}/issues",
        "license": "MIT",
        "keywords": ["icloud", "calendar", "contacts", "mail", "caldav"],
        "tools_generated": True,
        "compatibility": {"platforms": [platform]},
        "server": {
            "type": "binary",
            "entry_point": entry,
            "mcp_config": {"command": f"${{__dirname}}/{entry}", "env": env},
        },
        "user_config": user_config,
    }


def manifest_bytes(manifest):
    return (
        json.dumps(manifest, indent=2, sort_keys=True, ensure_ascii=True) + "\n"
    ).encode("utf-8")


def sha256_file(path):
    digest = hashlib.sha256()
    with open(path, "rb") as handle:
        for chunk in iter(lambda: handle.read(1024 * 1024), b""):
            digest.update(chunk)
    return digest.hexdigest()


def check_member_name(name):
    if not isinstance(name, str) or not name:
        raise BundleError("bundle entry name must not be empty")
    if "\\" in name:
        raise BundleError(f"bundle entry uses a backslash: {name}")
    if name.startswith("/"):
        raise BundleError(f"bundle entry is an absolute path: {name}")
    if re.match(r"^[A-Za-z]:", name):
        raise BundleError(f"bundle entry is an absolute path: {name}")
    path = PurePosixPath(name)
    if ".." in path.parts or "." in path.parts:
        raise BundleError(f"bundle entry is not normalized: {name}")
    if name.endswith("/"):
        raise BundleError(f"bundle must not contain directory entries: {name}")
    return name


def read_json_file(path):
    try:
        data = json.loads(Path(path).read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise BundleError(f"cannot read {path}: {exc}") from exc
    if not isinstance(data, dict):
        raise BundleError(f"{path} must contain an object")
    return data


def server_version(server_path):
    data = read_json_file(server_path)
    version = data.get("version")
    if not isinstance(version, str):
        raise BundleError(f"{server_path} has no string version")
    return normalize_version(version)


def extract_archive(archive, destination):
    try:
        handle = tarfile.open(archive, "r:gz")
    except (OSError, tarfile.TarError) as exc:
        raise BundleError(f"cannot read release archive {archive}: {exc}") from exc
    with handle:
        for member in handle:
            name = member.name[2:] if member.name.startswith("./") else member.name
            check_member_name(name)
            if not member.isfile():
                raise BundleError(
                    f"release archive member must be a regular file: {name}"
                )
            if member.size > MAX_BUNDLE_MEMBER_BYTES:
                raise BundleError(f"release archive member is too large: {name}")
            target = destination / name
            target.parent.mkdir(parents=True, exist_ok=True)
            source = handle.extractfile(member)
            if source is None:
                raise BundleError(f"cannot read release archive member: {member.name}")
            with source, open(target, "wb") as output:
                shutil.copyfileobj(source, output)
            mode = 0o755 if member.mode & 0o111 else 0o644
            os.chmod(target, mode)


def locate_source_files(source):
    source = Path(source)
    temp = None
    root = None
    try:
        if source.is_dir():
            root = source
        elif source.is_file() and (
            source.name.endswith(".tar.gz") or source.suffix == ".tgz"
        ):
            temp = tempfile.TemporaryDirectory(prefix="mcpb-source-")
            root = Path(temp.name)
            extract_archive(source, root)
        else:
            raise BundleError(
                "source must be a directory or a .tar.gz release archive"
            )

        roots = [root]
        nested = root / "package"
        if nested.is_dir():
            roots.append(nested)

        def find(name):
            for candidate_root in roots:
                candidate = candidate_root / name
                if candidate.is_file():
                    return candidate
            return None

        binary = find(PROJECT)
        if binary is None:
            raise BundleError(f"source has no {PROJECT} executable")
        if binary.stat().st_size == 0:
            raise BundleError(f"source {PROJECT} executable is empty")
        texts = []
        for name in TEXT_FILES:
            path = find(name)
            if path is None:
                fallback = Path(__file__).resolve().parent.parent / name
                if fallback.is_file():
                    path = fallback
            if path is None:
                raise BundleError(f"source has no {name}")
            texts.append((name, path.read_bytes()))
        return binary.read_bytes(), texts
    finally:
        if temp is not None:
            temp.cleanup()


def write_bundle(output, manifest, binary, texts):
    output = Path(output)
    entries = [
        ("manifest.json", manifest_bytes(manifest), 0o644),
        (manifest["server"]["entry_point"], binary, 0o755),
    ]
    entries.extend((name, data, 0o644) for name, data in texts)
    entries.sort(key=lambda item: item[0])

    output.parent.mkdir(parents=True, exist_ok=True)
    temporary = output.with_name(output.name + ".tmp")
    if temporary.exists():
        temporary.unlink()
    try:
        with zipfile.ZipFile(
            temporary, "w", compression=zipfile.ZIP_DEFLATED, compresslevel=9
        ) as archive:
            for name, data, mode in entries:
                info = zipfile.ZipInfo(name, date_time=ZIP_TIMESTAMP)
                info.compress_type = zipfile.ZIP_DEFLATED
                info.create_system = 3
                info.external_attr = (stat.S_IFREG | mode) << 16
                info.extra = b""
                info.comment = b""
                archive.writestr(info, data)
        os.replace(temporary, output)
    except Exception:
        if temporary.exists():
            temporary.unlink()
        raise


def build_bundle(
    source,
    target,
    version,
    server,
    output=None,
    allow_untested_windows=False,
    force=False,
):
    os_name, arch = parse_target(target, allow_untested_windows)
    normalized = normalize_version(version)
    declared = server_version(server)
    if normalized != declared:
        raise BundleError(
            f"bundle version {normalized} does not match server.json {declared}"
        )
    binary, texts = locate_source_files(source)
    manifest = manifest_for(target, normalized, allow_untested_windows)
    if output is None:
        output = Path("dist") / bundle_filename(normalized, os_name, arch)
    output = Path(output)
    if output.exists() and not force:
        raise BundleError(f"output already exists: {output}")
    write_bundle(output, manifest, binary, texts)
    return {
        "status": "ok",
        "bundle": str(output),
        "sha256": sha256_file(output),
        "size": output.stat().st_size,
        "version": normalized,
        "platform": f"{os_name}/{arch}",
    }


def find_absolute_path(value):
    if not isinstance(value, str):
        return None
    if value.startswith("https://") or value.startswith("http://"):
        return None
    if value.startswith("/"):
        return value
    if re.match(r"^[A-Za-z]:[\\/]", value):
        return value
    match = re.search(r"(?:^|[^A-Za-z0-9])/(?:home|Users|tmp|private|var)/", value)
    if match:
        return value
    return None


def walk_json(value):
    yield value
    if isinstance(value, dict):
        for item in value.values():
            yield from walk_json(item)
    elif isinstance(value, list):
        for item in value:
            yield from walk_json(item)


def validate_manifest(manifest, expected_version):
    if not isinstance(manifest, dict):
        raise BundleError("manifest.json must contain an object")
    if manifest.get("manifest_version") != MANIFEST_VERSION:
        raise BundleError("manifest_version must be 0.4")
    if manifest.get("name") != PROJECT:
        raise BundleError(f"manifest name must be {PROJECT}")
    version = normalize_version(manifest.get("version", ""))
    if version != expected_version:
        raise BundleError(
            f"manifest version {version} does not match server.json {expected_version}"
        )
    if not isinstance(manifest.get("description"), str) or not manifest[
        "description"
    ].strip():
        raise BundleError("manifest description must not be empty")
    author = manifest.get("author")
    if not isinstance(author, dict) or not isinstance(author.get("name"), str):
        raise BundleError("manifest author.name must be a string")
    repository = manifest.get("repository")
    if not isinstance(repository, dict) or not str(
        repository.get("url", "")
    ).startswith("https://"):
        raise BundleError("manifest repository.url must be an https URL")

    compatibility = manifest.get("compatibility")
    platforms = compatibility.get("platforms") if isinstance(compatibility, dict) else None
    if not isinstance(platforms, list) or len(platforms) != 1:
        raise BundleError("manifest must name exactly one platform")
    platform = platforms[0]
    if platform not in ("linux", "darwin", "win32"):
        raise BundleError(f"unsupported manifest platform: {platform}")

    server = manifest.get("server")
    if not isinstance(server, dict) or server.get("type") != "binary":
        raise BundleError("server.type must be binary")
    entry = server.get("entry_point")
    if not isinstance(entry, str):
        raise BundleError("server.entry_point must be a string")
    check_member_name(entry)
    expected_entry = f"server/{binary_name('windows' if platform == 'win32' else platform)}"
    if entry != expected_entry:
        raise BundleError(f"server.entry_point must be {expected_entry}")
    if platform == "win32" and not entry.endswith(".exe"):
        raise BundleError("Windows entry_point must end in .exe")
    if platform != "win32" and entry.endswith(".exe"):
        raise BundleError("non-Windows entry_point must not end in .exe")

    config = server.get("mcp_config")
    if not isinstance(config, dict):
        raise BundleError("server.mcp_config must be an object")
    command = config.get("command")
    if command != f"${{__dirname}}/{entry}":
        raise BundleError("server.mcp_config.command must use ${__dirname}")
    env = config.get("env")
    if not isinstance(env, dict) or not env:
        raise BundleError("server.mcp_config.env must map user_config values")
    if env.get("ICLOUD_MCP_READ_ONLY") != "${user_config.read_only}":
        raise BundleError("read-only environment must use ${user_config.read_only}")
    if env.get("ICLOUD_PASSWORD") != "${user_config.icloud_password}":
        raise BundleError("password environment must use ${user_config.icloud_password}")
    for key, option in env.items():
        if not isinstance(key, str) or not isinstance(option, str):
            raise BundleError("server.mcp_config.env must contain strings")
        if not re.match(r"^\$\{user_config\.[a-z0-9_]+\}$", option):
            raise BundleError(
                f"server.mcp_config.env.{key} must reference user_config"
            )

    user_config = manifest.get("user_config")
    if not isinstance(user_config, dict):
        raise BundleError("manifest user_config must be an object")
    for key in ("icloud_email", "icloud_password", "read_only", "enable_mail"):
        if key not in user_config:
            raise BundleError(f"manifest user_config must include {key}")
    read_only = user_config["read_only"]
    if not isinstance(read_only, dict) or read_only.get("default") != "true":
        raise BundleError("user_config.read_only must default to true")
    for key in ("icloud_password", "mail_password"):
        option = user_config.get(key)
        if not isinstance(option, dict) or option.get("sensitive") is not True:
            raise BundleError(f"user_config.{key} must be sensitive")
    for key, option in user_config.items():
        if not isinstance(option, dict) or option.get("type") not in (
            "string",
            "number",
            "boolean",
            "directory",
            "file",
        ):
            raise BundleError(f"user_config.{key} must have a supported type")
        if not isinstance(option.get("title"), str) or not option["title"].strip():
            raise BundleError(f"user_config.{key} must have a title")
        if not isinstance(option.get("description"), str) or not option[
            "description"
        ].strip():
            raise BundleError(f"user_config.{key} must have a description")

    for value in walk_json(manifest):
        if isinstance(value, str):
            absolute = find_absolute_path(value)
            if absolute is not None:
                raise BundleError(f"manifest contains an absolute path: {absolute}")

    text = manifest_bytes(manifest).decode("utf-8")
    for pattern in CREDENTIAL_PATTERNS:
        match = pattern.search(text)
        if match:
            raise BundleError(
                f"manifest contains a credential-like value: {match.group(0)[:24]}"
            )
    return platform, entry


def detect_binary_platform(data):
    if data.startswith(b"\x7fELF") and len(data) >= 20:
        machine = int.from_bytes(data[18:20], "little")
        return {0x3E: "amd64", 0xB7: "arm64"}.get(machine)
    if data.startswith(b"\xcf\xfa\xed\xfe") and len(data) >= 8:
        cpu = int.from_bytes(data[4:8], "little")
        return {0x01000007: "amd64", 0x0100000C: "arm64"}.get(cpu)
    if data.startswith(b"MZ") and len(data) >= 0x40:
        offset = int.from_bytes(data[0x3C:0x40], "little")
        if len(data) >= offset + 6 and data[offset : offset + 4] == b"PE\x00\x00":
            machine = int.from_bytes(data[offset + 4 : offset + 6], "little")
            return {0x8664: "amd64", 0xAA64: "arm64"}.get(machine)
    return None


def read_text_member(archive, name, limit=MAX_MANIFEST_BYTES):
    info = archive.getinfo(name)
    if info.file_size > limit:
        raise BundleError(f"bundle member is too large: {name}")
    data = archive.read(name)
    try:
        return data.decode("utf-8")
    except UnicodeDecodeError as exc:
        raise BundleError(f"bundle member is not UTF-8: {name}") from exc


def verify_bundle(bundle, server, execute=True):
    bundle = Path(bundle)
    if not bundle.is_file():
        raise BundleError(f"bundle not found: {bundle}")
    expected_version = server_version(server)
    try:
        archive = zipfile.ZipFile(bundle)
    except (OSError, zipfile.BadZipFile) as exc:
        raise BundleError(f"cannot read bundle: {exc}") from exc
    with archive:
        infos = archive.infolist()
        names = [info.filename for info in infos]
        if len(names) != len(set(names)):
            raise BundleError("bundle contains duplicate entries")
        for info in infos:
            check_member_name(info.filename)
            if info.file_size > MAX_BUNDLE_MEMBER_BYTES:
                raise BundleError(f"bundle member is too large: {info.filename}")
        if "manifest.json" not in names:
            raise BundleError("bundle has no manifest.json at the zip root")
        manifest = json.loads(read_text_member(archive, "manifest.json"))
        platform, entry = validate_manifest(manifest, expected_version)
        expected = {"manifest.json", entry, *TEXT_FILES}
        if set(names) != expected:
            extra = sorted(set(names) - expected)
            missing = sorted(expected - set(names))
            raise BundleError(
                f"bundle entries differ from the contract; extra={extra} missing={missing}"
            )

        binary_info = archive.getinfo(entry)
        mode = (binary_info.external_attr >> 16) & 0o777
        if mode & 0o111 == 0:
            raise BundleError(f"bundle entry is not executable: {entry}")
        binary = archive.read(entry)
        arch = detect_binary_platform(binary)
        os_name = {"linux": "linux", "darwin": "darwin", "win32": "windows"}[platform]
        if arch is not None:
            if platform == "win32" and arch != "amd64":
                raise BundleError("Windows bundle must contain an amd64 executable")
            if platform == "darwin" and arch != "arm64":
                raise BundleError("macOS bundle must contain an arm64 executable")
            if platform == "linux" and arch not in ("amd64", "arm64"):
                raise BundleError("Linux bundle has an unsupported executable")

        for name in TEXT_FILES:
            text = read_text_member(archive, name)
            if not text.strip():
                raise BundleError(f"bundle entry is empty: {name}")
            for pattern in CREDENTIAL_PATTERNS:
                if pattern.search(text):
                    raise BundleError(
                        f"bundle entry contains a credential-like value: {name}"
                    )

        binary_version = "not-run"
        if execute:
            with tempfile.TemporaryDirectory(prefix="mcpb-verify-") as temp:
                root = Path(temp)
                for name in (entry, "manifest.json"):
                    target = root / name
                    target.parent.mkdir(parents=True, exist_ok=True)
                    target.write_bytes(archive.read(name))
                executable = root / entry
                executable.chmod(0o755)
                environment = {
                    key: value
                    for key, value in os.environ.items()
                    if not key.startswith("ICLOUD_")
                }
                try:
                    result = subprocess.run(
                        [str(executable), "-version"],
                        cwd=str(root),
                        env=environment,
                        capture_output=True,
                        text=True,
                        timeout=15,
                        check=False,
                    )
                except (OSError, subprocess.TimeoutExpired) as exc:
                    raise BundleError(f"cannot run the bundled binary: {exc}") from exc
                if result.returncode != 0:
                    raise BundleError(
                        f"bundled binary -version exited {result.returncode}: "
                        f"{result.stderr.strip()[:200]}"
                    )
                reported = result.stdout.strip()
                if reported[:1] in ("v", "V"):
                    reported = reported[1:]
                if reported != expected_version:
                    raise BundleError(
                        f"bundled binary reports {result.stdout.strip()!r}, "
                        f"expected {expected_version}"
                    )
                binary_version = result.stdout.strip()

    return {
        "status": "ok",
        "bundle": str(bundle),
        "sha256": sha256_file(bundle),
        "size": bundle.stat().st_size,
        "version": expected_version,
        "platform": f"{os_name}/{arch or 'unknown'}",
        "binaryVersion": binary_version,
        "entries": sorted(names),
    }


def command_build(args):
    summary = build_bundle(
        args.source,
        args.target,
        args.version or server_version(args.server),
        args.server,
        args.output or None,
        args.allow_untested_windows,
        args.force,
    )
    print(json.dumps(summary, sort_keys=True))
    return 0


def command_verify(args):
    summary = verify_bundle(args.bundle, args.server, execute=not args.no_exec)
    print(json.dumps(summary, sort_keys=True))
    return 0


def command_manifest(args):
    manifest = manifest_for(
        args.target,
        args.version or server_version(args.server),
        args.allow_untested_windows,
    )
    print(json.dumps(manifest, indent=2, sort_keys=True))
    return 0


def build_parser():
    parser = argparse.ArgumentParser(description=__doc__)
    subcommands = parser.add_subparsers(dest="command", required=True)

    common = argparse.ArgumentParser(add_help=False)
    common.add_argument(
        "--server", type=Path, default=Path("server.json"), help="server.json path"
    )
    common.add_argument(
        "--allow-untested-windows",
        action="store_true",
        help="permit a windows/amd64 experiment bundle",
    )

    build = subcommands.add_parser("build", parents=[common], help="build a bundle")
    build.add_argument("--source", required=True, help="release archive or directory")
    build.add_argument("--target", required=True, help="os/arch target")
    build.add_argument("--version", default="", help="release version, with or without v")
    build.add_argument("--output", default="", help="output .mcpb path")
    build.add_argument("--force", action="store_true", help="replace an existing output")
    build.set_defaults(func=command_build)

    verify = subcommands.add_parser("verify", parents=[common], help="verify a bundle")
    verify.add_argument("bundle", help=".mcpb file")
    verify.add_argument(
        "--no-exec",
        action="store_true",
        help="skip running the bundled binary; use for a foreign architecture",
    )
    verify.set_defaults(func=command_verify)

    manifest = subcommands.add_parser(
        "manifest", parents=[common], help="print the generated manifest"
    )
    manifest.add_argument("--target", required=True, help="os/arch target")
    manifest.add_argument("--version", default="", help="release version, with or without v")
    manifest.set_defaults(func=command_manifest)
    return parser


def main(argv=None):
    args = build_parser().parse_args(argv)
    try:
        return args.func(args)
    except (BundleError, OSError, json.JSONDecodeError) as exc:
        print(json.dumps({"status": "error", "error": str(exc)}), file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
