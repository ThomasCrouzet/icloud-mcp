#!/usr/bin/env python3
"""Run synthetic MCP scenarios and keep repeatable evidence outside the tree."""

import argparse
import hashlib
import json
import os
from pathlib import Path
import platform
import queue
import signal
import subprocess
import sys
import threading
import time


ROOT = Path(__file__).resolve().parents[1]
FRAME_LIMIT = 1 << 20
PRODUCT_ENV = {
    "ICLOUD_EMAIL": "fixture-user@example.com",
    "ICLOUD_PASSWORD": "fixture-password-only",
    "ICLOUD_MCP_DEFAULT_TZ": "Europe/Paris",
    "ICLOUD_MAIL_ADDRESS": "fixture-mail@example.com",
    "ICLOUD_MAIL_PASSWORD": "fixture-mail-password-only",
    "ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS": "fixture-recipient@example.com",
}
CALENDAR_READS = {
    "list_calendars", "search_events", "get_event", "find_free_slots",
    "validate_event", "calendar_capabilities", "icloud_capabilities",
}
CALENDAR_WRITES = {"create_event", "update_event", "delete_event"}
CONTACT_READS = {"list_address_books", "search_contacts", "get_contact"}
CONTACT_WRITES = {"create_contact", "update_contact", "delete_contact"}
MAIL_READS = {"list_mailboxes", "search_messages", "get_message"}
MAIL_WRITES = {"set_message_flags", "move_message", "trash_message", "send_message"}


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def digest(data):
    return hashlib.sha256(data).hexdigest()


def git(*args):
    return subprocess.check_output(["git", *args], cwd=ROOT)


def environment():
    # Do not inherit product secrets, credentials, proxies, or fixture switches.
    allowed = {"PATH", "HOME", "TMPDIR", "TMP", "TEMP", "SYSTEMROOT", "GOTOOLCHAIN"}
    env = {key: value for key, value in os.environ.items() if key in allowed}
    env.update({"GOMAXPROCS": "2", "GOFLAGS": "-p=1", "TZ": "UTC"})
    return env


class Peer:
    def __init__(self, binary, output, name, mode="normal", **gates):
        self.name = name
        self.output = output
        self.transcript = (output / f"{name}.jsonl").open("w", encoding="ascii")
        self.lock = threading.Lock()
        self.responses = queue.Queue()
        self.logs = queue.Queue()
        self.errors = []
        self.next_id = 0
        self.finished = False
        env = environment()
        env.update(PRODUCT_ENV)
        env.update({
            "ICLOUD_MCP_READ_ONLY": "true",
            "ICLOUD_MCP_ENABLE_CONTACTS": "false",
            "ICLOUD_MCP_ENABLE_MAIL": "false",
            "ICLOUD_MCP_ENABLE_MAIL_WRITE": "false",
            "ICLOUD_MCP_ENABLE_MAIL_SEND": "false",
            "ICLOUD_MCP_PROTOCOL_FIXTURE": mode,
        })
        env.update(gates)
        command = [str(binary), "-test.run=^TestExecutableProtocolFixture$",
                   "-test.coverprofile=" + str(output / f"{name}.coverage.out")]
        self.record("start", {"argv": command, "environment": env})
        self.process = subprocess.Popen(
            command, stdin=subprocess.PIPE, stdout=subprocess.PIPE,
            stderr=subprocess.PIPE, cwd=ROOT, env=env,
        )
        self.threads = []
        for stream, channel in ((self.process.stdout, "stdout"), (self.process.stderr, "stderr")):
            thread = threading.Thread(target=self.read, args=(stream, channel), daemon=True)
            thread.start()
            self.threads.append(thread)

    def record(self, channel, value):
        with self.lock:
            self.transcript.write(json.dumps({"channel": channel, "value": value}, sort_keys=True) + "\n")
            self.transcript.flush()

    def read(self, stream, channel):
        try:
            while line := stream.readline(FRAME_LIMIT + 2):
                text = line.decode("utf-8")
                self.record(channel, text)
                for secret in PRODUCT_ENV.values():
                    if secret.startswith("fixture-") and secret in text:
                        self.errors.append(f"{channel} exposed a synthetic credential")
                if channel == "stdout":
                    require(len(line) <= 256 << 10, "oversized stdout frame")
                    message = json.loads(text)
                    require(message.get("jsonrpc") == "2.0", "non-protocol stdout")
                    self.responses.put(message)
                else:
                    self.logs.put(text)
        except Exception as error:
            self.errors.append(str(error))
        finally:
            stream.close()
            if channel == "stdout":
                self.responses.put(None)

    def send(self, message, fragments=False):
        data = json.dumps(message, separators=(",", ":")).encode() + b"\n"
        self.record("stdin", message)
        if fragments:
            split = len(data) // 2
            self.process.stdin.write(data[:split])
            self.process.stdin.flush()
            self.process.stdin.write(data[split:])
        else:
            self.process.stdin.write(data)
        self.process.stdin.flush()

    def request(self, method, params=None, fragments=False):
        self.next_id += 1
        message = {"jsonrpc": "2.0", "id": self.next_id, "method": method}
        if params is not None:
            message["params"] = params
        self.send(message, fragments)
        return self.next_id

    def response(self, request_id):
        message = self.responses.get(timeout=10)
        require(message is not None, f"{self.name}: process closed before response; {self.errors}")
        require(message.get("id") == request_id, f"unexpected response: {message}")
        return message

    def call(self, name, arguments=None):
        return self.response(self.request("tools/call", {"name": name, "arguments": arguments or {}}))

    def initialize(self):
        response = self.response(self.request("initialize", {
            "protocolVersion": "2025-11-25", "capabilities": {},
            "clientInfo": {"name": "protocol-fixture", "version": "1"},
        }, fragments=True))
        require(response["result"]["serverInfo"]["name"] == "icloud-mcp", "wrong server identity")
        self.send({"jsonrpc": "2.0", "method": "notifications/initialized"})

    def wait_log(self, marker):
        deadline = time.monotonic() + 10
        while True:
            line = self.logs.get(timeout=max(0.01, deadline - time.monotonic()))
            if marker in line:
                return
            require(time.monotonic() < deadline, f"missing fixture marker: {marker}")

    def finish(self, expected=0, terminate=False):
        if self.finished:
            return
        try:
            if terminate:
                self.record("signal", "SIGTERM")
                self.process.send_signal(signal.SIGTERM)
            elif not self.process.stdin.closed:
                self.record("stdin", {"eof": True})
                self.process.stdin.close()
            code = self.process.wait(timeout=10)
            for thread in self.threads:
                thread.join(timeout=2)
            self.record("exit", code)
            require(not self.errors, "; ".join(self.errors))
            require(code == expected, f"{self.name}: exit {code}, expected {expected}")
        finally:
            if self.process.poll() is None:
                self.process.kill()
                self.process.wait(timeout=5)
            if not self.process.stdin.closed:
                self.process.stdin.close()
            for thread in self.threads:
                thread.join(timeout=2)
            self.finished = True
            self.transcript.close()

    def abort(self):
        if self.finished:
            return
        self.record("abort", "scenario did not complete")
        if self.process.poll() is None:
            self.process.kill()
        self.process.wait(timeout=5)
        self.process.stdin.close()
        for thread in self.threads:
            thread.join(timeout=2)
        self.record("exit", self.process.returncode)
        self.finished = True
        self.transcript.close()


def tool_payload(response, error=False):
    require("error" not in response, f"unexpected JSON-RPC error: {response}")
    result = response["result"]
    require(bool(result.get("isError")) == error, f"unexpected tool status: {response}")
    require(len(result["content"]) == 1, "expected one text result")
    return json.loads(result["content"][0]["text"])


def inventory(peer, expected):
    response = peer.response(peer.request("tools/list"))
    names = {tool["name"] for tool in response["result"]["tools"]}
    require(names == expected, f"wrong tool inventory: {names ^ expected}")
    capabilities = tool_payload(peer.call("icloud_capabilities"))
    require(set(capabilities["tools"]) == expected, "capability manifest differs")
    require(capabilities["toolCount"] == len(expected), "wrong capability count")
    return capabilities


def run_stdio(binary, output, results):
    all_reads = CALENDAR_READS | CONTACT_READS | MAIL_READS
    all_writes = CALENDAR_WRITES | CONTACT_WRITES | MAIL_WRITES
    scenarios = [
        ("read-only", "normal", {"ICLOUD_MCP_ENABLE_CONTACTS": "true", "ICLOUD_MCP_ENABLE_MAIL": "true",
         "ICLOUD_MCP_ENABLE_MAIL_WRITE": "true", "ICLOUD_MCP_ENABLE_MAIL_SEND": "true"}, all_reads),
        ("default", "normal", {"ICLOUD_MCP_READ_ONLY": "false"}, CALENDAR_READS | CALENDAR_WRITES),
        ("all-tools", "normal", {"ICLOUD_MCP_READ_ONLY": "false", "ICLOUD_MCP_ENABLE_CONTACTS": "true",
         "ICLOUD_MCP_ENABLE_MAIL": "true", "ICLOUD_MCP_ENABLE_MAIL_WRITE": "true",
         "ICLOUD_MCP_ENABLE_MAIL_SEND": "true"}, all_reads | all_writes),
        ("mail-read-only", "normal", {"ICLOUD_MCP_READ_ONLY": "false", "ICLOUD_MCP_ENABLE_MAIL": "true"},
         CALENDAR_READS | CALENDAR_WRITES | MAIL_READS),
        ("domain-protocol-failures", "protocol-failure", {"ICLOUD_MCP_ENABLE_CONTACTS": "true",
         "ICLOUD_MCP_ENABLE_MAIL": "true"}, all_reads),
        ("cancellation", "cancel", {}, CALENDAR_READS),
        ("signal-shutdown", "normal", {}, CALENDAR_READS),
        ("eof-inflight", "cancel", {}, CALENDAR_READS),
        ("signal-inflight", "cancel", {}, CALENDAR_READS),
        ("oversized-frame", "normal", {}, CALENDAR_READS),
    ]
    for name, mode, gates, expected in scenarios:
        peer = Peer(binary, output, name, mode, **gates)
        try:
            peer.initialize()
            original = inventory(peer, expected)
            for disabled in sorted(all_writes - expected):
                denied = peer.call(disabled)
                require(denied.get("error", {}).get("code") == -32602, f"disabled write accepted: {disabled}")
            if name in {"read-only", "domain-protocol-failures"}:
                before = tool_payload(peer.call("list_calendars"))
                require(before["calendars"][0]["name"] == "Fixture Calendar", "missing Calendar fixture")
                for tool in ("list_address_books", "list_mailboxes"):
                    for _ in range(2):
                        failure = tool_payload(peer.call(tool), error=True)
                        code = "protocol_error" if mode == "protocol-failure" else "authentication"
                        require(failure["code"] == code, f"wrong failure classification: {failure}")
                        require(tool_payload(peer.call("list_calendars")) == before, "Calendar changed after optional failure")
                require(inventory(peer, expected) == original, "failures changed capabilities")
            if name == "cancellation":
                request_id = peer.request("tools/call", {"name": "list_calendars", "arguments": {}})
                peer.wait_log("fixture Calendar waiting")
                peer.send({"jsonrpc": "2.0", "method": "notifications/cancelled", "params": {"requestId": request_id}})
                peer.wait_log("fixture Calendar canceled")
                canceled = tool_payload(peer.response(request_id), error=True)
                require(canceled["code"] == "timeout", f"wrong cancellation result: {canceled}")
                require(tool_payload(peer.call("list_calendars"))["calendars"], "Calendar unusable after cancellation")
            if name in {"eof-inflight", "signal-inflight"}:
                peer.request("tools/call", {"name": "list_calendars", "arguments": {}})
                peer.wait_log("fixture Calendar waiting")
            if name == "oversized-frame":
                request_id = peer.next_id + 1
                frame = json.dumps({"jsonrpc": "2.0", "id": request_id, "method": "ping"}, separators=(",", ":")).encode()
                exact = frame + b" " * (FRAME_LIMIT - len(frame))
                peer.record("stdin-generated", {"base": frame.decode(), "pad_to": FRAME_LIMIT, "sha256": digest(exact)})
                peer.process.stdin.write(exact + b"\n")
                peer.process.stdin.flush()
                require("result" in peer.response(request_id), "exact-size frame rejected")
                oversized = exact + b" "
                peer.record("stdin-generated", {"base": frame.decode(), "pad_to": FRAME_LIMIT + 1, "sha256": digest(oversized)})
                peer.process.stdin.write(oversized + b"\n")
                peer.process.stdin.flush()
                peer.finish(expected=1)
                require(peer.responses.get(timeout=1) is None, "oversized frame reached the protocol")
            else:
                peer.finish(terminate=name in {"signal-shutdown", "signal-inflight"})
                if name in {"eof-inflight", "signal-inflight"}:
                    peer.wait_log("fixture Calendar canceled")
            results.append({"scenario": name, "status": "passed"})
        except Exception:
            results.append({"scenario": name, "status": "failed"})
            raise
        finally:
            peer.abort()


def merge_coverage(output, inputs):
    blocks = {}
    for path in inputs:
        lines = path.read_text(encoding="ascii").splitlines()
        require(lines[0] == "mode: atomic", f"unexpected coverage mode in {path}")
        for line in lines[1:]:
            block, statements, count = line.split()
            key = (block, int(statements))
            blocks[key] = blocks.get(key, 0) + int(count)
    output.write_text("mode: atomic\n" + "".join(
        f"{block} {statements} {count}\n" for (block, statements), count in sorted(blocks.items())
    ), encoding="ascii")


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    mode = parser.add_mutually_exclusive_group(required=True)
    mode.add_argument("--output", type=Path)
    mode.add_argument("--merge-coverage", type=Path, nargs="+")
    args = parser.parse_args()
    if args.merge_coverage:
        require(len(args.merge_coverage) >= 2, "supply the output and at least one input profile")
        merge_coverage(args.merge_coverage[0], args.merge_coverage[1:])
        return 0
    output = args.output.expanduser().resolve()
    require(not output.is_relative_to(ROOT), "evidence must stay outside the repository")
    output.mkdir(parents=True, exist_ok=False)
    env = environment()
    patch = git("diff", "--binary", "HEAD")
    (output / "working.patch").write_bytes(patch)
    files = set(git("ls-files", "-z", "--cached", "--others", "--exclude-standard").decode().split("\0"))
    sources = {}
    for name in sorted(files - {""}):
        path = ROOT / name
        if path.is_file():
            sources[name] = digest(path.read_bytes())
    fixtures = {name: value for name, value in sources.items() if "protocol" in name or "/testdata/" in name}
    for name in fixtures:
        target = output / "fixtures" / name
        target.parent.mkdir(parents=True, exist_ok=True)
        target.write_bytes((ROOT / name).read_bytes())
    manifest = {
        "command": [sys.executable, str(Path(__file__).resolve()), "--output", str(output)],
        "source_revision": git("rev-parse", "HEAD").decode().strip(),
        "git_status": git("status", "--porcelain=v1").decode(),
        "working_diff_sha256": digest(patch), "source_files": sources,
        "source_identity": digest(json.dumps(sources, sort_keys=True).encode()),
        "removed_documents": {
            name: digest(git("show", "HEAD:" + name))
            for name in git("diff", "--name-only", "--diff-filter=D", "HEAD", "--", "*.md").decode().splitlines()
        },
        "fixtures": fixtures, "environment": env, "platform": platform.platform(),
        "python": sys.version, "go": subprocess.check_output(["go", "version"], env=env).decode().strip(),
        "started_at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "commands": [], "results": [], "status": "failed",
    }

    def run(command, log):
        with (output / log).open("wb") as stream:
            result = subprocess.run(command, cwd=ROOT, env=env, stdout=stream, stderr=subprocess.STDOUT, timeout=300)
        manifest["commands"].append({"argv": command, "log": log, "exit_code": result.returncode})
        require(result.returncode == 0, f"command failed; see {output / log}")

    try:
        binary = output / "protocol-fixture"
        run(["go", "test", "-c", "-race", "-covermode=atomic", "-coverpkg=./cmd/icloud-mcp",
             "-o", str(binary), "./cmd/icloud-mcp"], "build.log")
        run_stdio(binary, output, manifest["results"])
        merge_coverage(output / "executable-coverage.out", sorted(output.glob("*.coverage.out")))
        run(["go", "test", "./internal/mcptools", "-run", "^TestProtocol", "-count=1", "-v", "-json", "-race"], "protocol-tests.jsonl")
        test_events = [json.loads(line) for line in (output / "protocol-tests.jsonl").read_text().splitlines()]
        passed = {event["Test"] for event in test_events if event.get("Action") == "pass" and "Test" in event}
        skipped = {event["Test"] for event in test_events if event.get("Action") == "skip" and "Test" in event}
        required = {
            "TestProtocolIdempotencySuccessAndConflict", "TestProtocolIdempotencyAmbiguousDispatch",
            "TestProtocolIdempotencyCancelledWaiter", "TestProtocolIdempotencyPendingDoesNotExpire",
            "TestProtocolRecurrenceCombinedDST", "TestProtocolRecurrenceOccurrenceLimit",
            "TestProtocolRecurrenceRejectedBudgets", "TestProtocolRecurrenceAggregateWorkBudget",
        }
        manifest["protocol_tests"] = {"passed": sorted(passed), "skipped": sorted(skipped)}
        require(required <= passed and not skipped, "required protocol scenarios are missing or skipped")
        manifest["status"] = "passed"
    except Exception as error:
        manifest["failure"] = str(error)
    finally:
        manifest["finished_at"] = time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
        manifest["artifacts"] = {
            str(path.relative_to(output)): digest(path.read_bytes())
            for path in output.rglob("*") if path.is_file()
        }
        (output / "manifest.json").write_text(json.dumps(manifest, indent=2, sort_keys=True) + "\n", encoding="ascii")
    print(f"Protocol evidence: {manifest['status']}: {output}")
    if manifest["status"] != "passed":
        print(manifest["failure"], file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    sys.exit(main())
