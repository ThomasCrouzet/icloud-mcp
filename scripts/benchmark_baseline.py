#!/usr/bin/env python3
"""Record a deterministic offline protocol performance baseline.

The runner uses only the Go toolchain and the Python standard library. It
clears inherited ICLOUD_* variables, executes synthetic fixtures, and writes
raw measurements outside the repository. It does not contact Apple services.
"""

from __future__ import annotations

import argparse
import hashlib
import json
import math
import os
import platform
import re
import resource
import statistics
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path

PACKAGES = [
    "./internal/icloud",
    "./internal/contacts",
    "./internal/mail",
    "./internal/mail/imapadapter",
    "./internal/mcptools",
]
MODULE_PREFIX = "github.com/ThomasCrouzet/icloud-mcp"
REQUIRED_SCENARIOS = {
    f"{MODULE_PREFIX}/internal/icloud": "TestCalendarBenchmarkScenarios",
    f"{MODULE_PREFIX}/internal/contacts": "TestContactsBenchmarkScenarios",
    f"{MODULE_PREFIX}/internal/mail": "TestMailBenchmarkScenarios",
    f"{MODULE_PREFIX}/internal/mail/imapadapter": "TestIMAPWireBenchmarkScenario",
    f"{MODULE_PREFIX}/internal/mcptools": "TestMCPBenchmarkScenarios",
}
SCENARIO_TESTS = "|".join(REQUIRED_SCENARIOS.values())
BENCH_LINE = re.compile(
    r"^(Benchmark\S+?)(?:-\d+)?\s+(\d+)\s+(\S+)\s+ns/op(?:\s+(.*))?$"
)
NUMBER = re.compile(r"^[+-]?(?:\d+(?:\.\d*)?|\.\d+)(?:[eE][+-]?\d+)?$")
METRIC_UNIT = re.compile(r"^[A-Za-z][A-Za-z0-9/_%\-]*$")

# Every scenario in the documented baseline must report these custom metrics.
# The strict default run fails when any name or field is absent.
REQUIRED_BENCHMARKS = {
    "BenchmarkRecurrenceExpansion/DSTSpringForward": ("fixture-occurrences", "occurrence-cap", "truncated"),
    "BenchmarkRecurrenceExpansion/DSTFallBack": ("fixture-occurrences", "occurrence-cap", "truncated"),
    "BenchmarkRecurrenceExpansion/SeriesCap2000": ("fixture-occurrences", "occurrence-cap", "truncated"),
    "BenchmarkRecurrenceExpansion/Count2000": ("fixture-occurrences", "occurrence-cap", "truncated"),
    "BenchmarkCalendarImportedUID/ColdDiscovery": ("fallback-report", "fixture-events", "rpc/op"),
    "BenchmarkCalendarImportedUID/WarmDiscovery": ("fallback-report", "fixture-events", "rpc/op", "location-cache"),
    "BenchmarkCalendarImportedUID/WarmLocationCache": ("fallback-report", "fixture-events", "rpc/op", "location-cache"),
    "BenchmarkContactsParseSearch2000/ColdDiscovery": ("fixture-contacts", "rpc/op"),
    "BenchmarkContactsParseSearch2000/WarmDiscovery": ("fixture-contacts", "rpc/op"),
    "BenchmarkMailSearchSparseUID": ("fixture-candidates", "result-limit", "service-call/op", "sparse-uid-base"),
    "BenchmarkMailGetNearLimitBody/NearLimitBody": ("payload-bytes", "refused", "service-call/op"),
    "BenchmarkMailGetNearLimitBody/OverLimitBodyRefusal": ("payload-bytes", "refused", "service-call/op"),
    "BenchmarkIMAPWireMetadata": ("wire-rpc/op", "metadata-messages", "header-bytes", "sparse-uid-base"),
    "BenchmarkIMAPWireBodyNearLimit": ("wire-rpc/op", "body-bytes", "mime-header-bytes", "sparse-uid-base", "inbound-budget-bytes"),
    "BenchmarkAvailabilityBudget/HighCardinality": ("range-hours", "worker-limit"),
    "BenchmarkAvailabilityBudget/Overflow": ("range-hours", "worker-limit"),
    "BenchmarkAvailabilityBudget/ManyEmpty": ("range-hours", "worker-limit"),
    "BenchmarkAvailabilityBudget/DenseRecurrence": ("range-hours", "worker-limit"),
    "BenchmarkAvailabilityBudget/LargeBuffers": ("range-hours", "worker-limit"),
    "BenchmarkSearchEventsSerialization": ("materialized-events", "result-cap", "rpc/op", "worker-limit"),
    "BenchmarkSearchEventsRefusals/MaterializationOverflow": ("materialized-events", "rpc/op"),
    "BenchmarkSearchEventsRefusals/ValidationRefusal": ("rpc/op",),
    "BenchmarkSearchEventsRefusals/RateLimited": ("refused", "rpc/op"),
    "BenchmarkSearchEventsRefusals/Cancellation": ("canceled", "rpc/op"),
    "BenchmarkSearchEventsFanoutLatency": ("injected-latency-ms", "max-concurrency", "rpc/op"),
    "BenchmarkMCPToolSchema": ("schema-properties",),
}


class BenchmarkParseError(ValueError):
    """A benchmark line or metric is malformed or incomplete."""


def parse_number(token: str, context: str) -> float:
    if not NUMBER.fullmatch(token):
        raise BenchmarkParseError(f"{context}: invalid numeric token {token!r}")
    value = float(token)
    if not math.isfinite(value):
        raise BenchmarkParseError(f"{context}: non-finite numeric token {token!r}")
    return value


def utc_now() -> str:
    return datetime.now(timezone.utc).replace(microsecond=0).isoformat()


def sha256_bytes(value: bytes) -> str:
    return hashlib.sha256(value).hexdigest()


def sha256_file(path: Path) -> str:
    return sha256_bytes(path.read_bytes())


def git_output(repo: Path, *args: str) -> str:
    return subprocess.run(
        ["git", *args], cwd=repo, check=True, capture_output=True, text=True
    ).stdout.strip()


def proc_meminfo() -> dict[str, int]:
    result: dict[str, int] = {}
    try:
        for line in Path("/proc/meminfo").read_text().splitlines():
            key, _, rest = line.partition(":")
            value = rest.strip().split()
            if value and value[0].isdigit():
                result[key] = int(value[0])
    except OSError:
        pass
    return result


def cpu_model() -> str:
    try:
        for line in Path("/proc/cpuinfo").read_text().splitlines():
            if line.startswith("model name"):
                return line.split(":", 1)[1].strip()
    except OSError:
        pass
    return platform.processor() or "unknown"


def environment_snapshot(repo: Path) -> dict:
    mem = proc_meminfo()
    load = os.getloadavg() if hasattr(os, "getloadavg") else (0.0, 0.0, 0.0)
    return {
        "time": utc_now(),
        "platform": platform.platform(),
        "machine": platform.machine(),
        "cpu_model": cpu_model(),
        "logical_cpu_count": os.cpu_count(),
        "go_version": subprocess.run(
            ["go", "version"], cwd=repo, check=True, capture_output=True, text=True
        ).stdout.strip(),
        "goos": subprocess.run(
            ["go", "env", "GOOS"], cwd=repo, check=True, capture_output=True, text=True
        ).stdout.strip(),
        "goarch": subprocess.run(
            ["go", "env", "GOARCH"], cwd=repo, check=True, capture_output=True, text=True
        ).stdout.strip(),
        "load_average": [round(value, 3) for value in load],
        "mem_total_kib": mem.get("MemTotal"),
        "mem_available_kib": mem.get("MemAvailable"),
        "swap_total_kib": mem.get("SwapTotal"),
        "swap_free_kib": mem.get("SwapFree"),
        "child_peak_rss_kib": resource.getrusage(resource.RUSAGE_CHILDREN).ru_maxrss,
    }


def source_identity(repo: Path) -> dict:
    status = git_output(repo, "status", "--porcelain")
    diff = subprocess.run(
        ["git", "diff", "HEAD", "--no-ext-diff", "--binary"],
        cwd=repo,
        check=True,
        capture_output=True,
    ).stdout
    untracked = git_output(repo, "ls-files", "--others", "--exclude-standard")
    return {
        "head": git_output(repo, "rev-parse", "HEAD"),
        "tree": git_output(repo, "rev-parse", "HEAD^{tree}"),
        "branch": git_output(repo, "rev-parse", "--abbrev-ref", "HEAD"),
        "worktree_status": status.splitlines(),
        "tracked_diff_sha256": sha256_bytes(diff) if diff else None,
        "untracked_files": untracked.splitlines(),
        "clean": not status and not untracked,
    }


def parse_benchmarks(log_text: str) -> list[dict]:
    records: list[dict] = []
    for line_number, line in enumerate(log_text.splitlines(), start=1):
        stripped = line.strip()
        if not stripped.startswith("Benchmark"):
            continue
        if " ns/op" not in stripped:
            # Sub-benchmark header lines name a scenario but carry no measurement.
            continue
        match = BENCH_LINE.match(stripped)
        if not match:
            raise BenchmarkParseError(f"line {line_number}: malformed benchmark line {stripped!r}")
        name, iterations, ns_token, rest = match.groups()
        ns_per_op = parse_number(ns_token, f"line {line_number} ns/op")
        if rest is None:
            raise BenchmarkParseError(f"line {line_number}: missing B/op and allocs/op")
        parts = rest.split()
        if len(parts) % 2 != 0:
            raise BenchmarkParseError(f"line {line_number}: incomplete metric pair {rest!r}")
        metrics: dict[str, float] = {}
        for index in range(0, len(parts), 2):
            value_token, unit = parts[index], parts[index + 1]
            if not METRIC_UNIT.fullmatch(unit):
                raise BenchmarkParseError(f"line {line_number}: invalid metric unit {unit!r}")
            if unit in metrics:
                raise BenchmarkParseError(f"line {line_number}: duplicate metric {unit!r}")
            metrics[unit] = parse_number(value_token, f"line {line_number} metric {unit}")
        if "B/op" not in metrics or "allocs/op" not in metrics:
            raise BenchmarkParseError(f"line {line_number}: missing B/op or allocs/op")
        records.append(
            {
                "name": name,
                "iterations": int(iterations),
                "ns_per_op": ns_per_op,
                "bytes_per_op": metrics.pop("B/op"),
                "allocs_per_op": metrics.pop("allocs/op"),
                "custom_metrics": metrics,
                "raw_line": stripped,
            }
        )
    return records


def validate_baseline(
    records: list[dict], required: dict[str, tuple[str, ...]], expected_count: int
) -> list[str]:
    if not isinstance(expected_count, int) or isinstance(expected_count, bool) or expected_count < 1:
        return [f"requested count must be a positive integer, got {expected_count!r}"]
    errors: list[str] = []
    if not records:
        errors.append("no benchmark results were parsed")
    by_name: dict[str, list[dict]] = {}
    for record in records:
        by_name.setdefault(record.get("name", "unknown"), []).append(record)
    for name, samples in by_name.items():
        if len(samples) != expected_count:
            errors.append(f"sample count {len(samples)} for {name}, want {expected_count}")
        for index, record in enumerate(samples, start=1):
            if record.get("bytes_per_op") is None:
                errors.append(f"{name} sample {index} of {expected_count}: missing B/op")
            if record.get("allocs_per_op") is None:
                errors.append(f"{name} sample {index} of {expected_count}: missing allocs/op")
    for name, required_metrics in required.items():
        samples = by_name.get(name)
        if samples is None:
            errors.append(f"missing benchmark {name}")
            continue
        for index, record in enumerate(samples, start=1):
            for metric in required_metrics:
                if metric not in record.get("custom_metrics", {}):
                    errors.append(f"missing metric {metric} for {name} sample {index} of {expected_count}")
    return errors


def parse_scenario_events(log_text: str) -> list[dict]:
    events: list[dict] = []
    for line_number, line in enumerate(log_text.splitlines(), start=1):
        stripped = line.strip()
        if not stripped:
            continue
        try:
            event = json.loads(stripped)
        except json.JSONDecodeError as error:
            raise BenchmarkParseError(f"scenario event line {line_number}: invalid JSON: {error}") from error
        if not isinstance(event, dict):
            raise BenchmarkParseError(f"scenario event line {line_number}: not a JSON object")
        action = event.get("Action")
        test = event.get("Test")
        package = event.get("Package")
        if action in {"run", "pass", "fail", "skip"} and test:
            events.append({"action": action, "package": package, "test": test})
    return events


def validate_scenarios(events: list[dict], required: dict[str, str]) -> list[str]:
    expected = {(package, test) for package, test in required.items()}
    observed: dict[tuple[str, str], list[str]] = {}
    for event in events:
        key = (event.get("package"), event.get("test"))
        if key in expected:
            observed.setdefault(key, []).append(event.get("action", ""))
    errors: list[str] = []
    for package, test in sorted(expected):
        actions = observed.get((package, test), [])
        if not actions:
            errors.append(f"missing scenario execution {test} in {package}")
            continue
        if "skip" in actions:
            errors.append(f"scenario {test} in {package} was skipped")
        if "fail" in actions:
            errors.append(f"scenario {test} in {package} failed")
        if "run" not in actions:
            errors.append(f"scenario {test} in {package} did not run")
        if "pass" not in actions:
            errors.append(f"scenario {test} in {package} did not pass")
    return errors


def summarise(records: list[dict]) -> dict:
    grouped: dict[str, list[dict]] = {}
    for record in records:
        grouped.setdefault(record["name"], []).append(record)
    summary = {}
    for name, samples in grouped.items():
        ns_values = [sample["ns_per_op"] for sample in samples]
        summary[name] = {
            "samples": len(samples),
            "ns_per_op_median": statistics.median(ns_values),
            "ns_per_op_min": min(ns_values),
            "ns_per_op_max": max(ns_values),
            "bytes_per_op": samples[-1]["bytes_per_op"],
            "allocs_per_op": samples[-1]["allocs_per_op"],
            "custom_metrics": samples[-1]["custom_metrics"],
        }
    return summary


def run_command(
    command: list[str], cwd: Path, env: dict[str, str], timeout: int, log_path: Path, err_path: Path
) -> dict:
    started = time.monotonic()
    try:
        completed = subprocess.run(
            command,
            cwd=cwd,
            env=env,
            capture_output=True,
            text=True,
            timeout=timeout,
            check=False,
        )
        stdout, stderr, exit_code, timed_out = completed.stdout, completed.stderr, completed.returncode, False
    except subprocess.TimeoutExpired as timeout_error:
        stdout = timeout_error.stdout or ""
        stderr = timeout_error.stderr or ""
        if isinstance(stdout, bytes):
            stdout = stdout.decode("utf-8", "replace")
        if isinstance(stderr, bytes):
            stderr = stderr.decode("utf-8", "replace")
        exit_code, timed_out = 124, True
    elapsed = time.monotonic() - started
    log_path.write_text(stdout)
    err_path.write_text(stderr)
    return {
        "command": command,
        "cwd": str(cwd),
        "exit_code": exit_code,
        "timed_out": timed_out,
        "elapsed_seconds": round(elapsed, 3),
        "stdout": log_path.name,
        "stderr": err_path.name,
        "stdout_sha256": sha256_file(log_path),
        "stderr_sha256": sha256_file(err_path),
    }


def check_manifest(path_value: str, requested_count: int | None, allow_partial: bool) -> int:
    path = Path(path_value).resolve()
    validation_errors: list[str] = []
    records: list[dict] = []
    events: list[dict] = []
    scenario_errors: list[str] = []
    manifest_count = None
    try:
        data = json.loads(path.read_text())
    except OSError as error:
        validation_errors.append(f"cannot read manifest {path}: {error}")
    except json.JSONDecodeError as error:
        validation_errors.append(f"manifest {path} is invalid JSON: {error}")
    else:
        candidates = data.get("benchmarks")
        if isinstance(candidates, list):
            records = candidates
        else:
            validation_errors.append("manifest benchmarks must be a list")
        settings = data.get("benchmark_settings")
        if isinstance(settings, dict):
            manifest_count = settings.get("count")
        count = requested_count if requested_count is not None else manifest_count
        required = {} if allow_partial else REQUIRED_BENCHMARKS
        validation_errors.extend(validate_baseline(records, required, count))
        scenario_events = data.get("scenario_events")
        if isinstance(scenario_events, list):
            events = scenario_events
            scenario_errors = validate_scenarios(events, REQUIRED_SCENARIOS)
        else:
            scenario_errors = ["missing scenario events"]
        validation_errors.extend(scenario_errors)
    validation_ok = not validation_errors
    complete = validation_ok and not allow_partial
    print(
        json.dumps(
            {
                "manifest": str(path),
                "requested_count": requested_count,
                "manifest_count": manifest_count,
                "benchmark_records": len(records),
                "benchmark_names": len({record.get("name") for record in records if isinstance(record, dict)}),
                "scenario_events": len(events),
                "scenario_validation_ok": not scenario_errors,
                "validation_ok": validation_ok,
                "complete": complete,
                "validation_errors": validation_errors,
            },
            indent=2,
        )
    )
    return 0 if validation_ok else 1


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--evidence-dir", help="Absolute output directory outside the repository")
    parser.add_argument("--check-manifest", default=None, help="Validate a retained manifest and exit without running Go")
    parser.add_argument("--mode", choices=("smoke", "full"), default="full")
    parser.add_argument("--bench", default="Benchmark", help="Go benchmark regular expression")
    parser.add_argument("--benchtime", default=None, help="Go -benchtime value; defaults to 1x for smoke and 500ms for full")
    parser.add_argument("--count", type=int, default=None, help="Go -count value; defaults to 1 for smoke and 3 for full")
    parser.add_argument("--timeout", type=int, default=900, help="Per-command timeout in seconds")
    parser.add_argument("--force", action="store_true", help="Write into a non-empty existing directory")
    parser.add_argument(
        "--allow-partial",
        action="store_true",
        help="Allow a targeted benchmark selection; still reject empty or incomplete records",
    )
    parser.add_argument("--repo", default=None, help="Repository root; defaults to the current directory")
    args = parser.parse_args()

    if args.count is not None and args.count < 1:
        parser.error("--count must be a positive integer")
    if args.check_manifest:
        return check_manifest(args.check_manifest, args.count, args.allow_partial)
    if not args.evidence_dir:
        parser.error("--evidence-dir is required unless --check-manifest is used")

    repo = Path(args.repo or os.getcwd()).resolve()
    if not (repo / "go.mod").exists():
        parser.error(f"repository root is missing go.mod: {repo}")

    evidence_dir = Path(args.evidence_dir).resolve()
    if evidence_dir == repo or repo in evidence_dir.parents:
        parser.error("evidence directory must be outside the repository")
    if evidence_dir.exists() and any(evidence_dir.iterdir()) and not args.force:
        parser.error("evidence directory is not empty; choose a new path or pass --force")
    evidence_dir.mkdir(parents=True, exist_ok=True)

    benchtime = args.benchtime or ("1x" if args.mode == "smoke" else "500ms")
    count = args.count if args.count is not None else (1 if args.mode == "smoke" else 3)

    credential_names = sorted(name for name in os.environ if name.startswith("ICLOUD_"))
    env = {key: value for key, value in os.environ.items() if not key.startswith("ICLOUD_")}
    env["GOMAXPROCS"] = "4"
    goflags = env.get("GOFLAGS", "").split()
    if "-p=4" not in goflags:
        goflags.append("-p=4")
    env["GOFLAGS"] = " ".join(goflags)

    environment_before = environment_snapshot(repo)
    identity = source_identity(repo)

    smoke_command = [
        "go",
        "test",
        *PACKAGES,
        "-run",
        f"^({SCENARIO_TESTS})$",
        "-count=1",
        "-timeout",
        f"{max(60, args.timeout // 2)}s",
        "-json",
    ]
    bench_command = [
        "go",
        "test",
        *PACKAGES,
        "-run",
        "^$",
        "-bench",
        args.bench,
        "-benchmem",
        f"-benchtime={benchtime}",
        f"-count={count}",
        "-timeout",
        f"{args.timeout}s",
        "-v",
    ]

    commands = []
    smoke = run_command(
        smoke_command, repo, env, args.timeout, evidence_dir / "scenario-smoke.json", evidence_dir / "scenario-smoke.err"
    )
    commands.append(smoke)
    scenario_events: list[dict] = []
    scenario_errors: list[str] = []
    try:
        scenario_events = parse_scenario_events((evidence_dir / "scenario-smoke.json").read_text())
    except BenchmarkParseError as error:
        scenario_errors.append(str(error))
    scenario_errors.extend(validate_scenarios(scenario_events, REQUIRED_SCENARIOS))
    if smoke["exit_code"] != 0:
        scenario_errors.insert(0, f"scenario command exited {smoke['exit_code']}")
    (evidence_dir / "scenario-events.json").write_text(json.dumps(scenario_events, indent=2) + "\n")
    benches: list[dict] = []
    parse_error = None
    if smoke["exit_code"] == 0 and not scenario_errors:
        bench = run_command(
            bench_command, repo, env, args.timeout, evidence_dir / "benchmark.log", evidence_dir / "benchmark.err"
        )
        commands.append(bench)
        if bench["exit_code"] == 0:
            bench_text = (evidence_dir / "benchmark.log").read_text()
            try:
                benches = parse_benchmarks(bench_text)
            except BenchmarkParseError as error:
                parse_error = str(error)
    required = {} if args.allow_partial else REQUIRED_BENCHMARKS
    validation_errors = validate_baseline(benches, required, count)
    if parse_error:
        validation_errors.insert(0, f"benchmark parser rejected the raw log: {parse_error}")
    validation_errors.extend(scenario_errors)
    validation_ok = not validation_errors
    complete = validation_ok and not args.allow_partial

    environment_after = environment_snapshot(repo)
    (evidence_dir / "environment-before.json").write_text(json.dumps(environment_before, indent=2) + "\n")
    (evidence_dir / "environment-after.json").write_text(json.dumps(environment_after, indent=2) + "\n")
    (evidence_dir / "benchmarks.json").write_text(json.dumps(benches, indent=2) + "\n")

    manifest = {
        "schema": "icloud-mcp-offline-benchmark-v1",
        "generated_at": utc_now(),
        "mode": args.mode,
        "source": identity,
        "environment_before": environment_before,
        "environment_after": environment_after,
        "credentials": {
            "inherited_icloud_variables_removed": credential_names,
            "note": "Only variable names are recorded; values are never read into the report.",
        },
        "commands": commands,
        "benchmark_settings": {
            "packages": PACKAGES,
            "bench": args.bench,
            "benchtime": benchtime,
            "count": count,
            "gomaxprocs": 4,
            "go_p": 4,
            "timeout_seconds": args.timeout,
        },
        "benchmarks": benches,
        "summary": summarise(benches),
        "scenario_events": scenario_events,
        "scenario_validation": {
            "ok": not scenario_errors,
            "errors": scenario_errors,
            "expected": [
                {"package": package, "test": test} for package, test in sorted(REQUIRED_SCENARIOS.items())
            ],
            "observed_events": len(scenario_events),
        },
        "validation": {
            "ok": validation_ok,
            "complete": complete,
            "errors": validation_errors,
            "required_benchmark_count": len(required),
            "requested_sample_count": count,
        },
        "limitations": [
            "The scenario smoke proves execution and business assertions, not statistical stability.",
            "ns/op, B/op, and allocs/op cover the measured operation; setup and fixture construction are excluded where the benchmark resets its timer.",
            "B/op and allocs/op are allocation totals, not retained live heap.",
            "Retained record budgets are code limits reported by scenario metrics, not measured memory.",
            "child_peak_rss_kib is the maximum resident set size of waited child processes; it is not a sampled Go heap and cannot establish another process state.",
            "Host load and frequency scaling can change wall-clock latency; compare only measurements taken on the same host and method.",
            "No benchmark defines a pass threshold or claims a speedup.",
        ],
    }
    manifest_path = evidence_dir / "manifest.json"
    manifest_path.write_text(json.dumps(manifest, indent=2) + "\n")

    failed = any(command["exit_code"] != 0 for command in commands) or not validation_ok
    print(json.dumps({
        "evidence_dir": str(evidence_dir),
        "mode": args.mode,
        "head": identity["head"],
        "clean": identity["clean"],
        "benchmark_count": len(benches),
        "validation_ok": validation_ok,
        "complete": complete,
        "validation_errors": validation_errors,
        "scenario_validation_ok": not scenario_errors,
        "scenario_errors": scenario_errors,
        "commands": [
            {"command": " ".join(command["command"]), "exit_code": command["exit_code"], "timed_out": command["timed_out"]}
            for command in commands
        ],
        "manifest": str(manifest_path),
    }, indent=2))
    return 1 if failed else 0


if __name__ == "__main__":
    sys.exit(main())
