#!/usr/bin/env python3
"""Verify the exact release candidate bytes promoted to a GitHub Release.

The candidate job builds once, writes a digest manifest, and uploads the
directory. Native jobs and the publication job download that same artifact and
call this verifier. Any missing, extra, stale, or altered file fails closed.
"""

from __future__ import annotations

import argparse
import hashlib
import shutil
import sys
import tempfile
from pathlib import Path

TARGETS = ("linux/amd64", "linux/arm64", "darwin/arm64")


def digest(path: Path) -> str:
    value = hashlib.sha256()
    with path.open("rb") as stream:
        for block in iter(lambda: stream.read(1 << 20), b""):
            value.update(block)
    return value.hexdigest()


def expected_names(tag: str) -> set[str]:
    names = {f"icloud-mcp-{tag}-checksums.txt"}
    for target in TARGETS:
        os_name, arch = target.split("/")
        names.add(f"icloud-mcp-{tag}-{os_name}-{arch}.tar.gz")
        names.add(f"icloud-mcp-{tag}-{os_name}-{arch}.mcpb")
    return names


def collect(directory: Path, tag: str) -> dict[str, str]:
    if not directory.is_dir():
        raise SystemExit(f"candidate directory is missing: {directory}")
    found = {}
    for name in sorted(expected_names(tag)):
        path = directory / name
        if not path.is_file() or path.is_symlink():
            raise SystemExit(f"candidate file is missing or not regular: {name}")
        found[name] = digest(path)
    ignored = {"candidate-digests.txt"}
    for path in directory.iterdir():
        if path.name in ignored or path.name.startswith("native-digests-"):
            continue
        if path.name not in found:
            raise SystemExit(f"unexpected candidate file: {path.name}")
    checksums = parse_checksums((directory / f"icloud-mcp-{tag}-checksums.txt").read_text(encoding="ascii"))
    for name, value in found.items():
        if name.endswith("-checksums.txt"):
            continue
        if checksums.get(name) != value:
            raise SystemExit(f"checksums.txt disagrees for {name}")
    if set(checksums) != {name for name in found if not name.endswith("-checksums.txt")}:
        raise SystemExit("checksums.txt does not list exactly the candidate archives and bundles")
    return found


def parse_checksums(text: str) -> dict[str, str]:
    out = {}
    for line in text.splitlines():
        parts = line.split()
        if len(parts) != 2 or len(parts[0]) != 64:
            raise SystemExit(f"malformed checksum line: {line!r}")
        value, name = parts
        if name in out:
            raise SystemExit(f"duplicate checksum entry: {name}")
        int(value, 16)
        out[name] = value
    return out


def write_manifest(path: Path, digests: dict[str, str]) -> None:
    path.write_text("".join(f"{value}  {name}\n" for name, value in sorted(digests.items())), encoding="ascii")


def read_manifest(path: Path) -> dict[str, str]:
    if not path.is_file():
        raise SystemExit(f"digest manifest is missing: {path}")
    return parse_checksums(path.read_text(encoding="ascii"))


def verify(directory: Path, tag: str, manifest: Path) -> None:
    expected = read_manifest(manifest)
    actual = collect(directory, tag)
    if expected != actual:
        missing = sorted(set(expected) - set(actual))
        extra = sorted(set(actual) - set(expected))
        changed = sorted(name for name in set(expected) & set(actual) if expected[name] != actual[name])
        raise SystemExit(
            f"candidate digest mismatch: missing={missing} extra={extra} changed={changed}")


def selftest() -> None:
    tag = "v9.9.9"
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary)
        good = root / "good"
        good.mkdir()
        lines = []
        for name in sorted(expected_names(tag)):
            if name.endswith("-checksums.txt"):
                continue
            path = good / name
            path.write_bytes(f"synthetic {name}\n".encode("ascii"))
            lines.append(f"{digest(path)}  {name}\n")
        (good / f"icloud-mcp-{tag}-checksums.txt").write_text("".join(lines), encoding="ascii")
        digests = collect(good, tag)
        manifest = root / "candidate-digests.txt"
        write_manifest(manifest, digests)
        verify(good, tag, manifest)

        altered = root / "altered"
        shutil.copytree(good, altered)
        target = altered / f"icloud-mcp-{tag}-linux-amd64.tar.gz"
        target.write_bytes(target.read_bytes() + b"tamper")
        try:
            verify(altered, tag, manifest)
        except SystemExit:
            pass
        else:
            raise SystemExit("altered candidate passed verification")

        missing = root / "missing"
        shutil.copytree(good, missing)
        (missing / f"icloud-mcp-{tag}-darwin-arm64.mcpb").unlink()
        try:
            verify(missing, tag, manifest)
        except SystemExit:
            pass
        else:
            raise SystemExit("missing candidate passed verification")


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    sub = parser.add_subparsers(dest="command", required=True)
    collect_parser = sub.add_parser("collect")
    collect_parser.add_argument("--dir", type=Path, required=True)
    collect_parser.add_argument("--tag", required=True)
    collect_parser.add_argument("--manifest", type=Path, required=True)
    collect_parser.add_argument("--github-output", type=Path)
    verify_parser = sub.add_parser("verify")
    verify_parser.add_argument("--dir", type=Path, required=True)
    verify_parser.add_argument("--tag", required=True)
    verify_parser.add_argument("--manifest", type=Path, required=True)
    sub.add_parser("selftest")
    args = parser.parse_args()

    if args.command == "collect":
        digests = collect(args.dir, args.tag)
        write_manifest(args.manifest, digests)
        if args.github_output:
            with args.github_output.open("a", encoding="ascii") as output:
                for name, value in sorted(digests.items()):
                    key = name.replace(".", "_").replace("-", "_").upper()
                    output.write(f"{key}={value}\n")
        for name, value in sorted(digests.items()):
            print(f"{value}  {name}")
    elif args.command == "verify":
        verify(args.dir, args.tag, args.manifest)
        print("release candidates verified")
    else:
        selftest()
        print("release candidate verifier self-test passed")
    return 0


if __name__ == "__main__":
    sys.exit(main())
