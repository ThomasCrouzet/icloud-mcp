#!/usr/bin/env python3
"""Check public documentation against the implementation and itself.

Checks:
    Environment keys: .env.example, internal/config source, README, and
    docs/agent-hosts.md agree on the exact key set and the stated count.
    Capability tokens: every accepted ICLOUD_MCP_PERMISSIONS token and every
    resource-scope action is documented in README and docs/agent-hosts.md.
    Relative links: every local Markdown link target exists.
    Release metadata: server.json and CHANGELOG.md share one version.

Tool counts and tool names are checked from Go by
internal/mcptools/docs_consistency_test.go.

Run from the repository root:

    python3 scripts/check_docs.py

Exit status is zero only when every check passes. The command uses no network.
"""

import json
import re
import sys
from pathlib import Path

ENV_KEY = re.compile(r"^([A-Z][A-Z0-9_]*)=(.*)$")
GO_ENV_KEY = re.compile(r'"(ICLOUD_[A-Z0-9_]+)"')
TABLE_KEY = re.compile(r"^\|\s*`(ICLOUD_[A-Z0-9_]+)`\s*\|", re.MULTILINE)
PERMISSION_CASE = re.compile(r'case "([a-z][a-z.]+)":')
SCOPE_ACTION = re.compile(r'Resource\w+\s*=\s*"([a-z][a-z.]+)"')
MARKDOWN_LINK = re.compile(r"!?\[[^\]]*\]\(([^)]+)\)")
COUNT_PATTERNS = (
    (Path("README.md"), re.compile(r"exactly \*\*(\d+) product environment variables\*\*")),
    (Path("docs/agent-hosts.md"), re.compile(r"Exactly \*\*(\d+)\*\* product environment variables")),
    (Path("docs/agent-hosts.md"), re.compile(r"for all (\d+) product variables")),
    (Path("docs/architecture.md"), re.compile(r"reads and validates the (\d+) environment variables")),
)


class DocsError(Exception):
    pass


def read_text(root, relative):
    path = root / relative
    try:
        return path.read_text(encoding="utf-8")
    except OSError as exc:
        raise DocsError(f"cannot read {relative}: {exc}") from exc


def env_source_keys(root):
    example = set()
    for line in read_text(root, ".env.example").splitlines():
        match = ENV_KEY.match(line)
        if match:
            example.add(match.group(1))
    go_keys = set()
    for path in sorted((root / "internal" / "config").glob("*.go")):
        if path.name.endswith("_test.go"):
            continue
        go_keys.update(GO_ENV_KEY.findall(path.read_text(encoding="utf-8")))
    if example != go_keys:
        missing = sorted(go_keys - example)
        extra = sorted(example - go_keys)
        raise DocsError(
            ".env.example and internal/config disagree on environment keys "
            f"(missing from .env.example: {missing}; extra: {extra})"
        )
    if not example:
        raise DocsError("no environment keys found")
    return example


def check_env_inventory(root):
    keys = env_source_keys(root)
    for relative in ("README.md", "docs/agent-hosts.md"):
        text = read_text(root, relative)
        documented = set(TABLE_KEY.findall(text))
        missing = sorted(keys - documented)
        extra = sorted(documented - keys)
        if missing or extra:
            raise DocsError(
                f"{relative} environment table disagrees with source "
                f"(missing: {missing}; extra: {extra})"
            )
    for relative, pattern in COUNT_PATTERNS:
        text = read_text(root, relative)
        matches = pattern.findall(text)
        if not matches:
            raise DocsError(f"{relative} does not state the environment variable count")
        for value in matches:
            if int(value) != len(keys):
                raise DocsError(
                    f"{relative} states {value} environment variables, source has {len(keys)}"
                )
    return len(keys)


def permission_tokens(root):
    text = read_text(root, "internal/config/config.go")
    start = text.find("func ParsePermissions(")
    end = text.find("func (p Permissions) Configured()", start)
    if start < 0 or end < 0:
        raise DocsError("cannot locate ParsePermissions in internal/config/config.go")
    return set(PERMISSION_CASE.findall(text[start:end]))


def scope_actions(root):
    text = read_text(root, "internal/config/scopes.go")
    actions = set(SCOPE_ACTION.findall(text))
    if not actions:
        raise DocsError("cannot locate resource scope actions in internal/config/scopes.go")
    return actions


def check_capability_docs(root):
    permissions = permission_tokens(root)
    actions = scope_actions(root)
    for relative in ("README.md", "docs/agent-hosts.md"):
        text = read_text(root, relative)
        missing_permissions = sorted(
            token for token in permissions if f"`{token}`" not in text
        )
        missing_actions = sorted(action for action in actions if f"`{action}`" not in text)
        if missing_permissions or missing_actions:
            raise DocsError(
                f"{relative} omits capability tokens "
                f"(permissions: {missing_permissions}; scopes: {missing_actions})"
            )
    return len(permissions), len(actions)


def check_relative_links(root):
    checked = 0
    for path in sorted(root.rglob("*.md")):
        relative = path.relative_to(root)
        if any(part in {".git", "bin", "dist"} for part in relative.parts):
            continue
        text = path.read_text(encoding="utf-8")
        in_fence = False
        for line in text.splitlines():
            if line.lstrip().startswith("```"):
                in_fence = not in_fence
                continue
            if in_fence:
                continue
            for target in MARKDOWN_LINK.findall(line):
                target = target.strip()
                if target.startswith(("<", "http://", "https://", "mailto:", "#")):
                    continue
                target = target.split("#", 1)[0].split("?", 1)[0]
                if not target:
                    continue
                candidate = (path.parent / target).resolve()
                try:
                    candidate.relative_to(root.resolve())
                except ValueError:
                    raise DocsError(f"{relative} links outside the repository: {target}")
                checked += 1
                if not candidate.exists():
                    raise DocsError(f"{relative} links to a missing target: {target}")
    if checked == 0:
        raise DocsError("no relative Markdown links found")
    return checked


def check_release_version(root):
    try:
        server = json.loads(read_text(root, "server.json"))
    except json.JSONDecodeError as exc:
        raise DocsError(f"cannot parse server.json: {exc}") from exc
    version = server.get("version")
    if not isinstance(version, str) or not version:
        raise DocsError("server.json version is missing")
    changelog = read_text(root, "CHANGELOG.md")
    if not re.search(rf"^## \[{re.escape(version)}\] - \d{{4}}-\d{{2}}-\d{{2}}$", changelog, re.MULTILINE):
        raise DocsError(f"CHANGELOG.md has no dated section for {version}")
    return version


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    root = Path(argv[0]).resolve() if argv else Path.cwd().resolve()
    try:
        keys = check_env_inventory(root)
        permissions, actions = check_capability_docs(root)
        links = check_relative_links(root)
        version = check_release_version(root)
    except DocsError as exc:
        print(f"documentation error: {exc}", file=sys.stderr)
        return 1
    print(
        "documentation checks passed: "
        f"{keys} environment keys, {permissions} capability tokens, "
        f"{actions} scope actions, {links} relative links, version {version}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
