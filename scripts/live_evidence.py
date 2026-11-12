#!/usr/bin/env python3
"""Check live iCloud gates and record privacy-safe integration evidence.

The script has two modes:

* ``--check`` reads local environment variables and secret-file metadata only.
  It never starts Go, opens a network connection, or prints a secret value.
* The default mode runs the checked-in ``integration`` Go suite after explicit
  confirmation. It writes sanitized evidence to a new directory outside the
  repository.

The Apple endpoints are reached only by the Go integration suite when a caller
explicitly requests a live run. A fixture or synthetic pass is not evidence of
Apple compatibility.
"""

import argparse
import json
import os
from pathlib import Path
import platform
import re
import stat
import subprocess
import sys
from datetime import datetime, timezone


ROOT = Path(__file__).resolve().parents[1]
SCHEMA = "icloud-mcp-live-evidence-v1"
SECRET_LIMIT = 4096
BOOLEAN_GATES = (
    "ICLOUD_MCP_READ_ONLY",
    "ICLOUD_MCP_ENABLE_CONTACTS",
    "ICLOUD_MCP_ENABLE_MAIL",
    "ICLOUD_MCP_ENABLE_MAIL_WRITE",
    "ICLOUD_MCP_ENABLE_MAIL_SEND",
    "ICLOUD_MCP_INTEGRATION_WRITES",
)
CALENDAR_CREDENTIALS = ("ICLOUD_EMAIL", "ICLOUD_PASSWORD")


class GateError(ValueError):
    """A local environment gate is missing or invalid."""


def env_text(env, name):
    value = env.get(name)
    if value is None:
        return ""
    return value.strip()


def parse_boolean(env, name):
    value = env_text(env, name).lower()
    if value in ("", "0", "false"):
        return False
    if value in ("1", "true"):
        return True
    raise GateError(f"{name} must be unset, 0, false, 1, or true")


def check_secret_reference(env, name):
    """Return the configured reference or None, without returning its content."""
    raw = env_text(env, name)
    if not raw:
        return None
    if raw.startswith("file://"):
        path_text = raw[len("file://") :]
        if not path_text:
            raise GateError(f"{name} file:// reference has no path")
        path = Path(path_text)
        if not path.is_absolute():
            raise GateError(f"{name} file:// path must be absolute")
        try:
            info = path.stat()
        except OSError:
            raise GateError(f"{name} file:// target is not a readable regular file")
        if not stat.S_ISREG(info.st_mode):
            raise GateError(f"{name} file:// target is not a regular file")
        if info.st_size == 0:
            raise GateError(f"{name} file:// target is empty")
        if info.st_size > SECRET_LIMIT:
            raise GateError(f"{name} file:// target exceeds {SECRET_LIMIT} bytes")
        if stat.S_IMODE(info.st_mode) & 0o077:
            raise GateError(f"{name} file:// target mode must be 0600 or stricter")
    return raw


def secret_text(env, name):
    """Read a configured secret for local redaction. Never print the result."""
    raw = check_secret_reference(env, name)
    if raw is None:
        return None
    if raw.startswith("file://"):
        try:
            return Path(raw[len("file://") :]).read_text(
                encoding="utf-8", errors="replace"
            )[:SECRET_LIMIT].strip()
        except OSError:
            return None
    return raw


def recipient_policy(env):
    raw = env_text(env, "ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS")
    if not raw:
        return []
    return [part.strip() for part in raw.split(",") if part.strip()]


def mail_address_value(env):
    raw = env_text(env, "ICLOUD_MAIL_ADDRESS")
    if raw.startswith("file://"):
        try:
            return secret_text(env, "ICLOUD_MAIL_ADDRESS") or ""
        except GateError:
            return ""
    return raw


def evaluate_gates(env, destructive=False):
    """Return (problems, state). State contains booleans and presence only."""
    problems = []
    flags = {}
    for name in BOOLEAN_GATES:
        try:
            flags[name] = parse_boolean(env, name)
        except GateError as error:
            problems.append(str(error))
            flags[name] = False

    read_only = flags["ICLOUD_MCP_READ_ONLY"]
    contacts = flags["ICLOUD_MCP_ENABLE_CONTACTS"]
    mail = flags["ICLOUD_MCP_ENABLE_MAIL"]
    mail_write = flags["ICLOUD_MCP_ENABLE_MAIL_WRITE"]
    mail_send = flags["ICLOUD_MCP_ENABLE_MAIL_SEND"]
    writes = flags["ICLOUD_MCP_INTEGRATION_WRITES"]

    calendar_present = True
    for name in CALENDAR_CREDENTIALS:
        try:
            if check_secret_reference(env, name) is None:
                calendar_present = False
                problems.append(f"{name} is required for Calendar integration")
        except GateError as error:
            calendar_present = False
            problems.append(str(error))

    mail_present = True
    mail_password_name = None
    if mail:
        try:
            if check_secret_reference(env, "ICLOUD_MAIL_ADDRESS") is None:
                mail_present = False
                problems.append("ICLOUD_MAIL_ADDRESS is required when Mail is enabled")
        except GateError as error:
            mail_present = False
            problems.append(str(error))
        if env_text(env, "ICLOUD_MAIL_PASSWORD"):
            mail_password_name = "ICLOUD_MAIL_PASSWORD"
        elif env_text(env, "ICLOUD_PASSWORD"):
            mail_password_name = "ICLOUD_PASSWORD"
        else:
            mail_present = False
            problems.append(
                "ICLOUD_MAIL_PASSWORD or ICLOUD_PASSWORD is required when Mail is enabled"
            )
        if mail_password_name:
            try:
                check_secret_reference(env, mail_password_name)
            except GateError as error:
                mail_present = False
                problems.append(str(error))

    if (mail_write or mail_send) and not mail:
        problems.append(
            "ICLOUD_MCP_ENABLE_MAIL=true is required for Mail mutation or send"
        )
    if (mail_write or mail_send) and read_only:
        problems.append(
            "ICLOUD_MCP_READ_ONLY=false is required for Mail mutation or send"
        )
    if writes and read_only:
        problems.append(
            "ICLOUD_MCP_READ_ONLY=false is required for destructive integration tests"
        )

    calendar_selector = env_text(env, "ICLOUD_MCP_INTEGRATION_CALENDAR")
    calendar_selector_set = bool(calendar_selector)
    if writes:
        if not calendar_selector_set:
            problems.append(
                "ICLOUD_MCP_INTEGRATION_CALENDAR is required for destructive "
                "integration tests; select a disposable calendar path"
            )
        elif not calendar_selector.startswith("/") or "://" in calendar_selector:
            problems.append(
                "ICLOUD_MCP_INTEGRATION_CALENDAR must be an absolute calendar "
                "path such as /123456789/calendars/disposable/"
            )

    policy = recipient_policy(env)
    wildcard = "*" in policy
    if wildcard:
        problems.append(
            "ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS must list exact addresses; "
            "the literal * is refused"
        )
    if mail_send:
        if not policy:
            problems.append(
                "ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS is required when Mail send is enabled"
            )
        elif wildcard:
            problems.append(
                "ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS must not be a wildcard for a live run"
            )

    self_recipient = env_text(env, "ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT")
    self_recipient_set = bool(self_recipient)
    if self_recipient_set:
        if not mail:
            problems.append(
                "ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT requires ICLOUD_MCP_ENABLE_MAIL=true"
            )
        else:
            address = mail_address_value(env)
            if not address:
                problems.append(
                    "ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT requires a valid "
                    "ICLOUD_MAIL_ADDRESS"
                )
            elif self_recipient != address:
                problems.append(
                    "ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT must exactly match "
                    "the configured Mail address"
                )
            elif policy and self_recipient not in policy:
                problems.append(
                    "ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS must allow the exact "
                    "self recipient"
                )

    if destructive and not writes:
        problems.append(
            "ICLOUD_MCP_INTEGRATION_WRITES=true is required for --destructive"
        )

    state = {
        "read_only": read_only,
        "contacts": contacts,
        "mail": mail,
        "mail_write": mail_write,
        "mail_send": mail_send,
        "integration_writes": writes,
        "calendar_selector_set": calendar_selector_set,
        "smtp_policy_exact": bool(policy) and not wildcard,
        "self_recipient_set": self_recipient_set,
        "credentials": {
            "calendar": calendar_present,
            "mail": mail_present if mail else False,
            "mail_password_source": mail_password_name or "",
        },
    }
    return problems, state


def secret_literals(env):
    """Return local values that must not appear in a stored artifact."""
    values = []
    for name in (
        "ICLOUD_EMAIL",
        "ICLOUD_PASSWORD",
        "ICLOUD_MAIL_ADDRESS",
        "ICLOUD_MAIL_FROM",
        "ICLOUD_MAIL_PASSWORD",
        "ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT",
        "ICLOUD_MCP_INTEGRATION_CALENDAR",
    ):
        raw = env_text(env, name)
        if raw:
            values.append(raw)
        secret = secret_text(env, name)
        if secret:
            values.append(secret)
    values.extend(recipient_policy(env))
    unique = []
    for value in values:
        if len(value) >= 3 and value not in unique:
            unique.append(value)
    return sorted(unique, key=len, reverse=True)


AUTHORIZATION_LINE = re.compile(
    r"(?i)(\bAuthorization\s*:\s*)(Basic|Bearer|SASL)\s+[^\s,;]+"
)
BASIC_AUTH = re.compile(r"(?i)\b(Basic|Bearer)\s+[A-Za-z0-9+/=._~-]{8,}")
SASL_AUTH = re.compile(r"(?i)\bSASL\s+(?:AUTH\s+)?[A-Za-z0-9+/=._~-]{8,}")
ASSIGNED_SECRET = re.compile(
    r"(?i)\b(password|passwd|secret|token|api[_-]?key)\s*[:=]\s*[^\s,;\"']+"
)
MESSAGE_ID = re.compile(r"<[^<>\s]+@[^<>\s]+>")
HEADER_ID = re.compile(
    r"(?im)^(\s*(?:Message-ID|In-Reply-To|References)\s*:\s*).*$"
)
EMAIL_ADDRESS = re.compile(r"[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}")


def redact_text(text, secrets):
    """Remove configured secrets and common protocol credentials from text."""
    result = text or ""
    for secret in secrets:
        if secret:
            result = result.replace(secret, "[REDACTED]")
    result = AUTHORIZATION_LINE.sub(r"\1\2 [REDACTED]", result)
    result = BASIC_AUTH.sub(r"\1 [REDACTED]", result)
    result = SASL_AUTH.sub("SASL [REDACTED]", result)
    result = ASSIGNED_SECRET.sub(r"\1=[REDACTED]", result)
    result = HEADER_ID.sub(r"\1[REDACTED]", result)
    result = MESSAGE_ID.sub("<redacted-message-id>", result)
    result = EMAIL_ADDRESS.sub("[redacted-address]", result)
    return result


RESULT_LINE = re.compile(r"^\s*--- (PASS|FAIL|SKIP): (\S+) \(([^)]*)\)")


def parse_results(output):
    results = []
    for line in (output or "").splitlines():
        match = RESULT_LINE.match(line)
        if match:
            results.append(
                {
                    "name": match.group(2),
                    "status": match.group(1).lower(),
                    "duration": match.group(3),
                }
            )
    return results


def summarize_results(results, exit_code, timed_out):
    counts = {"pass": 0, "fail": 0, "skip": 0}
    for result in results:
        status = result.get("status")
        if status in counts:
            counts[status] += 1
    passed = exit_code == 0 and not timed_out and counts["fail"] == 0
    return {
        "exit_code": exit_code,
        "timed_out": timed_out,
        "passed": passed,
        "counts": counts,
        "recorded_assertions": len(results) > 0,
    }


def validate_output_directory(path, repo=ROOT):
    if path is None:
        raise GateError("--output is required for a live run")
    target = Path(path)
    if not target.is_absolute():
        raise GateError("--output must be an absolute path")
    resolved = target.resolve()
    repo_resolved = Path(repo).resolve()
    if resolved == repo_resolved or repo_resolved in resolved.parents:
        raise GateError("--output must be outside the repository")
    if resolved.exists():
        if not resolved.is_dir():
            raise GateError("--output exists and is not a directory")
        try:
            if any(resolved.iterdir()):
                raise GateError("--output must be new or empty; previous evidence is preserved")
        except OSError:
            raise GateError("--output is not readable")
    return resolved


def run_git(repo, *args):
    try:
        completed = subprocess.run(
            ["git", *args],
            cwd=repo,
            capture_output=True,
            text=True,
            check=False,
        )
    except OSError:
        return "unknown"
    value = (completed.stdout or "").strip()
    return value or "unknown"


def run_go_version(go_binary):
    try:
        completed = subprocess.run(
            [go_binary, "version"],
            capture_output=True,
            text=True,
            check=False,
        )
    except OSError:
        return "unknown"
    value = (completed.stdout or "").strip()
    return value or "unknown"


def run_suite(repo, go_binary, timeout):
    command = [
        go_binary,
        "test",
        "-tags=integration",
        "-count=1",
        "-v",
        f"-timeout={timeout}s",
        ".",
    ]
    child_env = dict(os.environ)
    child_env.setdefault("GOMAXPROCS", "2")
    try:
        completed = subprocess.run(
            command,
            cwd=repo,
            env=child_env,
            capture_output=True,
            text=True,
            timeout=timeout + 60,
            check=False,
        )
        output = (completed.stdout or "") + (completed.stderr or "")
        return command, output, completed.returncode, False
    except subprocess.TimeoutExpired as error:
        stdout = error.stdout or ""
        stderr = error.stderr or ""
        if isinstance(stdout, bytes):
            stdout = stdout.decode("utf-8", errors="replace")
        if isinstance(stderr, bytes):
            stderr = stderr.decode("utf-8", errors="replace")
        return command, stdout + stderr, 124, True
    except OSError as error:
        return command, f"failed to start the integration suite: {error}", 127, False


def build_evidence(repo, go_binary, command, raw_output, exit_code, timed_out, state):
    results = parse_results(raw_output)
    summary = summarize_results(results, exit_code, timed_out)
    revision = run_git(repo, "rev-parse", "HEAD")
    describe = run_git(repo, "describe", "--tags", "--always", "--dirty")
    return {
        "schema": SCHEMA,
        "generated_at": datetime.now(timezone.utc).isoformat(timespec="seconds"),
        "repository": {
            "revision": revision,
            "describe": describe,
            "version": describe,
            "binary": "go test temporary test binary; no release archive",
        },
        "environment": {
            "os": platform.system(),
            "os_release": platform.release(),
            "arch": platform.machine(),
            "go_version": run_go_version(go_binary),
            "python_version": platform.python_version(),
        },
        "command": command,
        "gates": state,
        "results": {**summary, "tests": results},
        "limitations": [
            "One account and one date do not prove compatibility for every account.",
            "A fixture or synthetic pass is not Apple compatibility proof.",
            "SMTP accepted does not prove final delivery.",
            "Skipped tests are not live evidence.",
        ],
    }


def markdown_evidence(evidence):
    results = evidence["results"]
    lines = [
        "# Live iCloud evidence record",
        "",
        f"- Recorded at: {evidence['generated_at']}",
        f"- Revision: {evidence['repository']['revision']}",
        f"- Version: {evidence['repository']['version']}",
        f"- Describe: {evidence['repository']['describe']}",
        f"- OS: {evidence['environment']['os']} {evidence['environment']['os_release']}",
        f"- Architecture: {evidence['environment']['arch']}",
        f"- Go version: {evidence['environment']['go_version']}",
        f"- Python version: {evidence['environment']['python_version']}",
        f"- Result: {'PASS' if results['passed'] else 'FAIL'}",
        f"- Exit code: {results['exit_code']}",
        f"- Timed out: {str(results['timed_out']).lower()}",
        "",
        "## Command",
        "",
        "```text",
        " ".join(evidence["command"]),
        "```",
        "",
        "## Gate flags",
        "",
        "| Gate | Value |",
        "|------|-------|",
    ]
    gates = evidence["gates"]
    for name in (
        "read_only",
        "contacts",
        "mail",
        "mail_write",
        "mail_send",
        "integration_writes",
        "calendar_selector_set",
        "smtp_policy_exact",
        "self_recipient_set",
    ):
        lines.append(f"| `{name}` | `{str(gates.get(name)).lower()}` |")
    credentials = gates.get("credentials", {})
    lines.append(
        f"| `calendar_credentials_present` | "
        f"`{str(credentials.get('calendar', False)).lower()}` |"
    )
    lines.append(
        f"| `mail_credentials_present` | "
        f"`{str(credentials.get('mail', False)).lower()}` |"
    )
    lines.extend(
        [
            "",
            "## Scenario results",
            "",
            "| Test | Status | Duration |",
            "|------|--------|----------|",
        ]
    )
    for result in results["tests"]:
        lines.append(
            f"| `{result['name']}` | {result['status']} | {result['duration']} |"
        )
    if not results["tests"]:
        lines.append("| none | none | none |")
    lines.extend(["", "## Limitations", ""])
    for item in evidence["limitations"]:
        lines.append(f"- {item}")
    lines.extend(
        [
            "",
            "Keep this record and any raw transcript outside the repository.",
            "Remove addresses, resource paths, tokens, and message identifiers before sharing.",
            "",
        ]
    )
    return "\n".join(lines)


def write_artifacts(output, evidence, sanitized_output):
    (output / "live-test.log").write_text(sanitized_output, encoding="utf-8")
    (output / "live-evidence.json").write_text(
        json.dumps(evidence, indent=2, sort_keys=True, ensure_ascii=True) + "\n",
        encoding="utf-8",
    )
    (output / "live-evidence.md").write_text(
        markdown_evidence(evidence), encoding="utf-8"
    )


def check_command(env, destructive=False):
    problems, state = evaluate_gates(env, destructive=destructive)
    if problems:
        print("live evidence gate check: FAIL", file=sys.stderr)
        for problem in problems:
            print(f"- {problem}", file=sys.stderr)
        print("network: not used", file=sys.stderr)
        return 1
    print("live evidence gate check: OK")
    print(f"- Calendar credentials: {'present' if state['credentials']['calendar'] else 'missing'}")
    print(f"- Contacts gate: {'on' if state['contacts'] else 'off'}")
    print(f"- Mail gate: {'on' if state['mail'] else 'off'}")
    print(f"- Mail write gate: {'on' if state['mail_write'] else 'off'}")
    print(f"- Mail send gate: {'on' if state['mail_send'] else 'off'}")
    print(f"- Destructive gate: {'on' if state['integration_writes'] else 'off'}")
    print(f"- Destructive request: {'on' if destructive else 'off'}")
    print("network: not used")
    return 0


def run_command(args, env):
    if not args.confirm_live:
        print(
            "live run refused: pass --confirm-live after you review the gates",
            file=sys.stderr,
        )
        return 1
    try:
        output = validate_output_directory(args.output, args.repo)
    except GateError as error:
        print(f"live run refused: {error}", file=sys.stderr)
        return 1

    problems, state = evaluate_gates(env, destructive=args.destructive)
    if state["integration_writes"] and not args.destructive:
        problems.append(
            "ICLOUD_MCP_INTEGRATION_WRITES=true requires --destructive so "
            "destructive tests are explicit"
        )
    if problems:
        print("live evidence gate check: FAIL", file=sys.stderr)
        for problem in problems:
            print(f"- {problem}", file=sys.stderr)
        print("network: not used", file=sys.stderr)
        return 1

    output.mkdir(parents=True, exist_ok=True)
    command, raw_output, exit_code, timed_out = run_suite(
        args.repo, args.go, args.timeout
    )
    secrets = secret_literals(env)
    sanitized = redact_text(raw_output, secrets)
    evidence = build_evidence(
        args.repo,
        args.go,
        command,
        sanitized,
        exit_code,
        timed_out,
        state,
    )
    write_artifacts(output, evidence, sanitized)
    results = evidence["results"]
    verdict = "PASS" if results["passed"] else "FAIL"
    print(
        f"live integration: {verdict} "
        f"(pass={results['counts']['pass']} "
        f"fail={results['counts']['fail']} "
        f"skip={results['counts']['skip']})"
    )
    print(f"evidence: {output}")
    return 0 if results["passed"] else 1


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "--check",
        action="store_true",
        help="Validate local gates and credential presence without network access",
    )
    parser.add_argument(
        "--confirm-live",
        action="store_true",
        help="Confirm that the run may contact Apple with the configured account",
    )
    parser.add_argument(
        "--destructive",
        action="store_true",
        help="Include write and send tests; requires ICLOUD_MCP_INTEGRATION_WRITES=true",
    )
    parser.add_argument(
        "--output",
        default=None,
        help="New absolute directory outside the repository for sanitized evidence",
    )
    parser.add_argument(
        "--timeout",
        type=int,
        default=900,
        help="Bounded Go test timeout in seconds (default: 900)",
    )
    parser.add_argument(
        "--go",
        default="go",
        help="Go executable to use (default: go)",
    )
    parser.add_argument(
        "--repo",
        default=str(ROOT),
        help="Repository root; defaults to the checkout that contains this script",
    )
    args = parser.parse_args(argv)
    if args.timeout < 30:
        parser.error("--timeout must be at least 30 seconds")
    args.repo = Path(args.repo).resolve()
    if args.check:
        return check_command(os.environ, destructive=args.destructive)
    return run_command(args, os.environ)


if __name__ == "__main__":
    sys.exit(main())
