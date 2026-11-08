#!/usr/bin/env python3
"""Native smoke test for an extracted icloud-mcp release binary.

The test uses no live account and no network. It checks:
    1. The binary reports the release version.
    2. An empty environment fails closed with a configuration error.
    3. A synthetic stdio session initializes, lists the read-only Calendar tools,
       and exits cleanly on SIGTERM.

Run it on the native target platform, for example:

    python3 scripts/native_smoke.py --binary native-smoke/icloud-mcp \
        --version v0.5.1 --tools 7
"""

import argparse
import json
import os
import queue
import signal
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path

PROTOCOL_VERSION = "2025-11-25"
MUTATION_TOOLS = {
    "create_event",
    "update_event",
    "delete_event",
    "create_contact",
    "update_contact",
    "delete_contact",
    "set_message_flags",
    "move_message",
    "trash_message",
    "send_message",
    "reply_message",
}


class SmokeError(Exception):
    pass


def read_stream(stream, lines):
    for line in stream:
        lines.put(line)
    lines.put(None)


def request(process, lines, payload, wanted, timeout):
    process.stdin.write(json.dumps(payload) + "\n")
    process.stdin.flush()
    deadline = time.monotonic() + timeout
    while time.monotonic() < deadline:
        try:
            line = lines.get(timeout=max(0.01, deadline - time.monotonic()))
        except queue.Empty:
            break
        if line is None:
            raise SmokeError(f"server closed stdout before response {wanted}")
        try:
            message = json.loads(line)
        except json.JSONDecodeError:
            continue
        if message.get("id") == wanted:
            return message
    raise SmokeError(f"timed out waiting for response {wanted}")


def check_version(binary, version):
    result = subprocess.run(
        [binary, "-version"], capture_output=True, text=True, timeout=20
    )
    if result.returncode != 0:
        raise SmokeError(f"-version exited {result.returncode}: {result.stderr.strip()}")
    if result.stdout.strip() != version:
        raise SmokeError(
            f"-version reported {result.stdout.strip()!r}, want {version!r}"
        )
    return result.stdout.strip()


def check_offline_failure(binary):
    env = {"PATH": os.environ.get("PATH", "")}
    result = subprocess.run(
        [binary],
        stdin=subprocess.DEVNULL,
        capture_output=True,
        text=True,
        env=env,
        timeout=20,
    )
    if result.returncode == 0:
        raise SmokeError("empty environment unexpectedly started the server")
    if "configuration error" not in result.stderr.lower():
        raise SmokeError(
            f"empty environment did not report a configuration error: {result.stderr.strip()}"
        )
    return result.returncode


def check_path_with_spaces(binary, version):
    with tempfile.TemporaryDirectory(prefix="icloud-mcp smoke ") as temp:
        spaced = Path(temp) / "icloud mcp" / "icloud-mcp"
        spaced.parent.mkdir()
        spaced.write_bytes(binary.read_bytes())
        spaced.chmod(0o755)
        check_version(spaced, version)
        check_offline_failure(spaced)
        return str(spaced)


def check_stdio(binary, version, tool_count, timeout):
    with tempfile.TemporaryDirectory(prefix="icloud-mcp-smoke-") as temp:
        root = Path(temp)
        email = root / "email"
        password = root / "password"
        email.write_text("smoke@example.invalid\n", encoding="utf-8")
        password.write_text("smoke-app-password\n", encoding="utf-8")
        for path in (email, password):
            path.chmod(0o600)

        env = {
            "PATH": os.environ.get("PATH", ""),
            "ICLOUD_EMAIL": f"file://{email}",
            "ICLOUD_PASSWORD": f"file://{password}",
            "ICLOUD_MCP_READ_ONLY": "true",
            "ICLOUD_MCP_PERMISSIONS": "calendar.read",
            "ICLOUD_MCP_ENABLE_CONTACTS": "false",
            "ICLOUD_MCP_ENABLE_MAIL": "false",
            "ICLOUD_MCP_DEFAULT_TZ": "UTC",
            "ICLOUD_MCP_LOG_LEVEL": "error",
        }
        process = subprocess.Popen(
            [binary],
            stdin=subprocess.PIPE,
            stdout=subprocess.PIPE,
            stderr=subprocess.PIPE,
            text=True,
            bufsize=1,
            env=env,
            start_new_session=os.name == "posix",
        )
        lines = queue.Queue()
        threads = [
            threading.Thread(target=read_stream, args=(process.stdout, lines), daemon=True),
            threading.Thread(target=read_stream, args=(process.stderr, queue.Queue()), daemon=True),
        ]
        for thread in threads:
            thread.start()
        try:
            initialized = request(
                process,
                lines,
                {
                    "jsonrpc": "2.0",
                    "id": 1,
                    "method": "initialize",
                    "params": {
                        "protocolVersion": PROTOCOL_VERSION,
                        "capabilities": {},
                        "clientInfo": {"name": "native-smoke", "version": "1"},
                    },
                },
                1,
                timeout,
            )
            info = initialized.get("result", {}).get("serverInfo", {})
            if info.get("name") != "icloud-mcp":
                raise SmokeError(f"serverInfo.name = {info.get('name')!r}")
            if info.get("version") != version:
                raise SmokeError(
                    f"serverInfo.version = {info.get('version')!r}, want {version!r}"
                )
            process.stdin.write(
                json.dumps(
                    {"jsonrpc": "2.0", "method": "notifications/initialized", "params": {}}
                )
                + "\n"
            )
            process.stdin.flush()
            listed = request(
                process,
                lines,
                {"jsonrpc": "2.0", "id": 2, "method": "tools/list", "params": {}},
                2,
                timeout,
            )
            tools = [tool.get("name") for tool in listed.get("result", {}).get("tools", [])]
            if len(tools) != tool_count:
                raise SmokeError(f"tools/list returned {len(tools)} tools, want {tool_count}")
            unexpected = sorted(MUTATION_TOOLS.intersection(tools))
            if unexpected:
                raise SmokeError(f"read-only session exposed mutation tools: {unexpected}")
            for required in ("icloud_capabilities", "list_calendars", "search_events"):
                if required not in tools:
                    raise SmokeError(f"tools/list omitted {required}")

            if os.name == "posix":
                process.send_signal(signal.SIGTERM)
            else:  # pragma: no cover - native Windows is not a supported claim
                process.terminate()
            try:
                code = process.wait(timeout=timeout)
            except subprocess.TimeoutExpired as exc:
                raise SmokeError("server did not exit after SIGTERM") from exc
            if code != 0:
                raise SmokeError(f"clean shutdown exited {code}")
            return len(tools), code
        finally:
            if process.poll() is None:
                if os.name == "posix":
                    os.killpg(process.pid, signal.SIGKILL)
                else:  # pragma: no cover
                    process.kill()
                process.wait(timeout=5)
            _ = threads


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True, type=Path)
    parser.add_argument("--version", required=True)
    parser.add_argument("--tools", type=int, default=7)
    parser.add_argument("--timeout", type=float, default=15.0)
    args = parser.parse_args(argv)

    binary = args.binary.resolve()
    if not binary.is_file():
        print(f"native smoke error: binary not found: {binary}", file=sys.stderr)
        return 1
    try:
        reported = check_version(binary, args.version)
        offline_code = check_offline_failure(binary)
        check_path_with_spaces(binary, args.version)
        count, shutdown_code = check_stdio(binary, args.version, args.tools, args.timeout)
    except (OSError, subprocess.SubprocessError, SmokeError) as exc:
        print(f"native smoke error: {exc}", file=sys.stderr)
        return 1
    print(
        "native smoke ok: "
        f"version={reported} offline_exit={offline_code} "
        f"tools={count} shutdown_exit={shutdown_code} spaced_path=ok "
        f"os={os.uname().sysname.lower()} arch={os.uname().machine}"
    )
    return 0


if __name__ == "__main__":
    sys.exit(main())
