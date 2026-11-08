#!/usr/bin/env python3
"""Check shipped host profiles through the real binary, without live tool calls."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import queue
import runpy
import subprocess
import sys
import threading


ROOT = Path(__file__).resolve().parents[2]
PROTOCOL = runpy.run_path(str(ROOT / "scripts/protocol_evidence.py"))
require = PROTOCOL["require"]
PROFILES = {
    "calendar-read-only": PROTOCOL["CALENDAR_READS"],
    "calendar-contacts-read-only": PROTOCOL["CALENDAR_READS"] | PROTOCOL["CONTACT_READS"],
    "mail-read-only": PROTOCOL["MAIL_READS"] | {"icloud_capabilities"},
    "calendar-scoped-write": PROTOCOL["CALENDAR_READS"] | {"create_event", "update_event"},
    "mail-exact-send": {"icloud_capabilities", "send_message"},
}
SECRETS = {
    "ICLOUD_EMAIL": "profile-calendar@example.invalid",
    "ICLOUD_PASSWORD": "synthetic-calendar-password",
    "ICLOUD_MAIL_ADDRESS": "profile-mail@example.invalid",
    "ICLOUD_MAIL_PASSWORD": "synthetic-mail-password",
}


def digest(data):
    return hashlib.sha256(data).hexdigest()


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, f"duplicate JSON key: {key}")
        result[key] = value
    return result


class ProfilePeer(PROTOCOL["Peer"]):
    def __init__(self, binary, output, name, env):
        self.name, self.output = name, output
        self.transcript = (output / f"{name}.jsonl").open("x", encoding="ascii")
        self.lock = threading.Lock()
        self.responses, self.logs = queue.Queue(), queue.Queue()
        self.errors, self.threads = [], []
        self.next_id, self.finished = 0, False
        command = [str(binary)]
        self.record("start", {"argv": command, "environment": env})
        self.process = subprocess.Popen(command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
                                        stderr=subprocess.PIPE, cwd=ROOT, env=env)
        for stream, channel in ((self.process.stdout, "stdout"), (self.process.stderr, "stderr")):
            thread = threading.Thread(target=self.read, args=(stream, channel), daemon=True)
            thread.start()
            self.threads.append(thread)


def check_profile(binary, output, name, expected):
    path = ROOT / "examples/profiles" / f"{name}.json"
    source = path.read_bytes()
    host = json.loads(source, object_pairs_hook=unique_object)
    require(set(host) == {"mcpServers"} and set(host["mcpServers"]) == {"icloud"}, "wrong host shape")
    entry = host["mcpServers"]["icloud"]
    require(set(entry) == {"command", "env"} and Path(entry["command"]).is_absolute(), "wrong stdio entry")
    require(isinstance(entry["env"], dict), "env must be an object")
    allowed = {line.split("=", 1)[0] for line in (ROOT / ".env.example").read_text().splitlines()
               if line.startswith("ICLOUD_")}
    home = output / f"{name}-home"
    home.mkdir(mode=0o700)
    env = {key: os.environ[key] for key in ("PATH", "SYSTEMROOT", "TMPDIR") if key in os.environ}
    env.update(HOME=str(home), GOMAXPROCS="4")
    fixtures = {}
    for key, value in entry["env"].items():
        require(key in allowed and isinstance(value, str), "unsupported variable or non-string value")
        if key in SECRETS:
            require(value.startswith("file:///"), f"inline or relative secret: {key}")
            secret = home / key
            fd = os.open(secret, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
            with os.fdopen(fd, "w", encoding="ascii") as stream:
                stream.write(SECRETS[key])
            fixtures[key] = {"path": str(secret), "mode": oct(secret.stat().st_mode & 0o777),
                             "sha256": digest(secret.read_bytes())}
            value = "file://" + str(secret)
        env[key] = value
    peer = ProfilePeer(binary, output, name, env)
    try:
        peer.initialize()
        capabilities = PROTOCOL["inventory"](peer, expected)
        readonly = name.endswith("read-only")
        require(capabilities["readOnly"] == readonly, "wrong read-only report")
        universe = set().union(*(PROFILES.values()), PROTOCOL["CONTACT_WRITES"],
                               PROTOCOL["MAIL_WRITES"], PROTOCOL["CALENDAR_WRITES"])
        denied = []
        for disabled in sorted(universe - expected):
            result = peer.call(disabled)
            require(result.get("error", {}).get("code") == -32602, f"disabled tool dispatched: {disabled}")
            denied.append(disabled)
        peer.finish()
    finally:
        peer.abort()
    transcript = (output / f"{name}.jsonl").read_text()
    for value in SECRETS.values():
        require(value not in transcript, "credential appeared in protocol or logs")
    return {"profile": name, "source_sha256": digest(source), "fixtures": fixtures,
            "capabilities": capabilities, "denied_tools": denied, "exit": peer.process.returncode,
            "status": "passed"}


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--output", required=True, type=Path, help="new absolute directory outside the source tree")
    args = parser.parse_args()
    output = args.output.resolve()
    require(args.output.is_absolute() and not output.is_relative_to(ROOT), "evidence must be external and absolute")
    output.mkdir(mode=0o700)
    env = {k: v for k, v in os.environ.items() if not k.startswith("ICLOUD_")}
    env.update(GOMAXPROCS="4", GOFLAGS="-p=4")
    binary = output / "icloud-mcp"
    command = ["go", "build", "-p", "4", "-o", str(binary), "./cmd/icloud-mcp"]
    report = {"argv": sys.argv, "platform": platform.platform(), "build_command": command,
              "head": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT).decode().strip(),
              "diff_sha256": digest(subprocess.check_output(["git", "diff", "--binary", "HEAD"], cwd=ROOT)),
              "source_files": {str(p.relative_to(ROOT)): digest(p.read_bytes()) for p in
                               [Path(__file__), *sorted((ROOT / "examples/profiles").glob("*.json"))]},
              "scenarios": [], "live_calls": "not_run", "desktop_installation": "not_run"}
    try:
        build = subprocess.run(command, cwd=ROOT, env=env, capture_output=True, timeout=180)
        (output / "build.stdout").write_bytes(build.stdout)
        (output / "build.stderr").write_bytes(build.stderr)
        report["build_exit"] = build.returncode
        require(build.returncode == 0, "binary build failed")
        report["binary_sha256"] = digest(binary.read_bytes())
        for name, expected in PROFILES.items():
            report["scenarios"].append(check_profile(binary, output, name, expected))
        report["status"] = "passed"
    except Exception as error:
        report["status"], report["failure"] = "failed", str(error)
        raise
    finally:
        (output / "result.json").write_text(json.dumps(report, indent=2) + "\n", encoding="ascii")
        print(json.dumps(report, indent=2))


if __name__ == "__main__":
    main()
