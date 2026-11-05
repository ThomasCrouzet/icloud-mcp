#!/usr/bin/env python3
"""Run the versioned network and production-policy AST guards.

The guard sources live in scripts/guards/*.go.txt. This runner copies them to a
temporary directory, runs the negative network fixture, then scans the repository.
Exit status is zero only when accepted code passes and the negative fixture fails.
"""

import argparse
import shutil
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parent
GUARDS = ROOT / "guards"


def run_guard(source, root, label):
    with tempfile.TemporaryDirectory(prefix="icloud-mcp-guard-") as temp:
        tool = Path(temp) / "guard.go"
        shutil.copyfile(source, tool)
        result = subprocess.run(
            ["go", "run", str(tool), "."],
            cwd=root,
            capture_output=True,
            text=True,
        )
    if result.stdout:
        sys.stdout.write(result.stdout)
    if result.stderr:
        sys.stderr.write(result.stderr)
    if result.returncode != 0:
        print(f"{label}: guard rejected {root}", file=sys.stderr)
    return result.returncode


def negative_fixture():
    with tempfile.TemporaryDirectory(prefix="icloud-mcp-guard-fixture-") as temp:
        base = Path(temp)
        root = base / "fixture"
        root.mkdir()
        shutil.copyfile(GUARDS / "network_guard_fixture.go.txt", root / "dynamic.go")
        tool = base / "network_guard.go"
        shutil.copyfile(GUARDS / "network_guard.go.txt", tool)
        result = subprocess.run(
            ["go", "run", str(tool), str(root)],
            capture_output=True,
            text=True,
        )
    output = result.stdout + result.stderr
    if result.returncode == 0:
        print("network guard accepted unresolved direct destinations", file=sys.stderr)
        return 1
    required = [
        "unresolved dynamic direct dial target",
        "unresolved dynamic direct HTTP target",
    ]
    missing = [message for message in required if message not in output]
    if missing:
        sys.stderr.write(output)
        print("network guard negative fixture missed: " + ", ".join(missing), file=sys.stderr)
        return 1
    print("network guard rejected both expected invalid destinations")
    return 0


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("root", nargs="?", default=".", help="repository root")
    args = parser.parse_args(argv)
    root = Path(args.root).resolve()
    status = negative_fixture()
    if status != 0:
        return status
    status = run_guard(GUARDS / "network_guard.go.txt", root, "network guard")
    if status != 0:
        return status
    return run_guard(
        GUARDS / "production_policy_guard.go.txt", root, "production policy guard"
    )


if __name__ == "__main__":
    sys.exit(main())
