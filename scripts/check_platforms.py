#!/usr/bin/env python3
"""Check platform support claims against the build and CI configuration.

The Makefile TARGETS list, the README status table, the platform support page,
and the CI native matrix must agree. Windows stays compile-only unless the
documentation and the CI matrix both change. The command uses no network.

Run from the repository root:

    python3 scripts/check_platforms.py
"""

import re
import sys
from pathlib import Path

TARGETS_LINE = re.compile(r"^TARGETS\s*:=\s*(.+)$", re.MULTILINE)
CI_PAIR = re.compile(r"asset_os:\s*([a-z0-9]+)\s*\n\s*asset_arch:\s*([a-z0-9]+)")
README_LABEL = {
    "Linux amd64": "linux/amd64",
    "Linux arm64": "linux/arm64",
    "macOS arm64": "darwin/arm64",
    "Windows amd64": "windows/amd64",
}


class PlatformError(Exception):
    pass


def read(root, relative):
    try:
        return (root / relative).read_text(encoding="utf-8")
    except OSError as exc:
        raise PlatformError(f"cannot read {relative}: {exc}") from exc


def makefile_targets(root):
    text = read(root, "Makefile")
    match = TARGETS_LINE.search(text)
    if not match:
        raise PlatformError("Makefile has no TARGETS line")
    targets = {target.strip() for target in match.group(1).split() if target.strip()}
    if not targets:
        raise PlatformError("Makefile TARGETS is empty")
    return targets


def table_rows(text):
    rows = []
    for line in text.splitlines():
        line = line.strip()
        if not line.startswith("|"):
            continue
        cells = [cell.strip() for cell in line.strip("|").split("|")]
        if not cells or cells[0] in {"Platform", "Target"}:
            continue
        if all(set(cell) <= {"-", ":"} for cell in cells):
            continue
        rows.append(cells)
    return rows


def check_readme(root, targets):
    rows = table_rows(read(root, "README.md"))
    found = {}
    for cells in rows:
        if len(cells) < 4 or cells[0] not in README_LABEL:
            continue
        found[README_LABEL[cells[0]]] = cells
    missing = sorted(targets - set(found))
    if missing:
        raise PlatformError(f"README status table omits published targets: {missing}")
    for target in sorted(targets):
        cells = found[target]
        if cells[1].lower() != "yes":
            raise PlatformError(f"README says {target} has no published archive")
        if "none" in cells[2].lower() or "compile and archive only" in cells[2].lower():
            raise PlatformError(f"README says {target} has no native release smoke")
        if "supported" not in cells[3].lower():
            raise PlatformError(f"README does not claim {target} as supported")
    windows = found.get("windows/amd64")
    if not windows:
        raise PlatformError("README status table omits windows/amd64")
    if windows[1].lower() != "no":
        raise PlatformError("README claims a Windows archive without native support")
    if "none" not in windows[2].lower():
        raise PlatformError("README claims a Windows native smoke without evidence")
    return found


def check_platform_page(root, targets):
    rows = {}
    for cells in table_rows(read(root, "docs/platform-support.md")):
        if len(cells) < 6:
            continue
        name = cells[0].strip("`")
        if "/" in name and name not in {"Platform"}:
            rows[name] = cells
    missing = sorted(targets - set(rows))
    if missing:
        raise PlatformError(f"docs/platform-support.md omits targets: {missing}")
    for target in sorted(targets):
        cells = rows[target]
        for index, label in ((1, "compiled"), (2, "published"), (3, "archive verified"), (4, "native executed")):
            if not cells[index].lower().startswith("yes"):
                raise PlatformError(f"platform page does not claim {label} for {target}")
        if "supported" not in cells[5].lower():
            raise PlatformError(f"platform page does not claim support for {target}")
    windows = rows.get("windows/amd64")
    if not windows:
        raise PlatformError("platform page omits windows/amd64")
    if not windows[2].lower().startswith("no") or not windows[4].lower().startswith("no"):
        raise PlatformError("platform page claims Windows publication or native evidence")
    if "not supported" not in windows[5].lower():
        raise PlatformError("platform page does not record the Windows decision")
    return rows


def check_ci(root, targets):
    text = read(root, ".github/workflows/ci.yml")
    matrix = {f"{os_name}/{arch}" for os_name, arch in CI_PAIR.findall(text)}
    if matrix != targets:
        missing = sorted(targets - matrix)
        extra = sorted(matrix - targets)
        raise PlatformError(
            "CI native matrix disagrees with Makefile TARGETS "
            f"(missing: {missing}; extra: {extra})"
        )
    if "windows/amd64" not in text:
        raise PlatformError("CI no longer compiles windows/amd64 for the smoke check")
    return matrix


def main(argv=None):
    argv = sys.argv[1:] if argv is None else argv
    root = Path(argv[0]).resolve() if argv else Path.cwd().resolve()
    try:
        targets = makefile_targets(root)
        check_readme(root, targets)
        check_platform_page(root, targets)
        matrix = check_ci(root, targets)
    except PlatformError as exc:
        print(f"platform check error: {exc}", file=sys.stderr)
        return 1
    print(
        "platform checks passed: "
        f"published={','.join(sorted(targets))} "
        f"native={','.join(sorted(matrix))} windows=compile-only"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
