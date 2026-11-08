#!/usr/bin/env python3
"""Regression tests for the offline benchmark recorder.

Complete predicate: for every selected or required benchmark name, the number
of samples equals the requested positive integer count, and every sample has
ns/op, B/op, allocs/op, and every required custom metric. An empty result, a
truncated count, an extra count, or a missing field on any sample is rejected.
A legitimate partial selection is valid when each selected name is complete,
but it stays incomplete at the CLI level.
"""

import json
import math
import subprocess
import sys
import tempfile
import unittest
from pathlib import Path

import benchmark_baseline as recorder

SCRIPT = Path(__file__).with_name("benchmark_baseline.py")
CASE = "BenchmarkRecurrenceExpansion/DSTSpringForward"
CASE_METRICS = recorder.REQUIRED_BENCHMARKS[CASE]


def sample(name, missing_metric=None, missing_memory=None):
    metrics = {metric: 1 for metric in recorder.REQUIRED_BENCHMARKS.get(name, ())}
    if name == CASE:
        metrics = {metric: 1 for metric in CASE_METRICS}
    if missing_metric:
        metrics.pop(missing_metric, None)
    return {
        "name": name,
        "iterations": 1,
        "ns_per_op": 1000,
        "bytes_per_op": None if missing_memory == "B/op" else 1,
        "allocs_per_op": None if missing_memory == "allocs/op" else 1,
        "custom_metrics": metrics,
        "raw_line": "synthetic",
    }


def healthy_records(samples=3):
    records = []
    for name in recorder.REQUIRED_BENCHMARKS:
        for _ in range(samples):
            records.append(sample(name))
    return records


def healthy_scenario_events():
    events = []
    for package, test in recorder.REQUIRED_SCENARIOS.items():
        events.append({"action": "run", "package": package, "test": test})
        events.append({"action": "pass", "package": package, "test": test})
    return events


def write_manifest(directory, records, count=3, scenario_events=None):
    path = Path(directory) / "manifest.json"
    if scenario_events is None:
        scenario_events = healthy_scenario_events()
    path.write_text(
        json.dumps(
            {
                "schema": "icloud-mcp-offline-benchmark-v1",
                "benchmarks": records,
                "benchmark_settings": {"count": count},
                "scenario_events": scenario_events,
            }
        )
    )
    return path


def run_cli(*args):
    return subprocess.run(
        [sys.executable, str(SCRIPT), *args],
        capture_output=True,
        text=True,
        timeout=60,
        check=False,
    )


class ParseBenchmarkLineTests(unittest.TestCase):
    def test_scientific_custom_metric_is_exact(self):
        line = (
            "BenchmarkMetricControl-4 1 1234 ns/op "
            "4.000e+09 sparse-uid-base 204800 B/op 20 allocs/op"
        )
        records = recorder.parse_benchmarks(line)
        self.assertEqual(len(records), 1)
        self.assertEqual(records[0]["name"], "BenchmarkMetricControl")
        self.assertEqual(records[0]["custom_metrics"]["sparse-uid-base"], 4_000_000_000)
        self.assertEqual(records[0]["bytes_per_op"], 204800)
        self.assertEqual(records[0]["allocs_per_op"], 20)

    def test_signed_and_scientific_custom_metrics_are_exact(self):
        line = (
            "BenchmarkMetricControl-4 1 1.5e+03 ns/op "
            "-2.500e-02 signed-metric +3.000e+02 positive-metric "
            "1024 B/op 7 allocs/op"
        )
        record = recorder.parse_benchmarks(line)[0]
        self.assertEqual(record["ns_per_op"], 1500)
        self.assertEqual(record["custom_metrics"]["signed-metric"], -0.025)
        self.assertEqual(record["custom_metrics"]["positive-metric"], 300)

    def test_malformed_or_partial_number_is_rejected(self):
        for value in ("4.000e+09junk", "4foo", "1.2.3", "e+09", "4e", ""):
            with self.subTest(value=value):
                line = (
                    "BenchmarkMetricControl-4 1 1234 ns/op "
                    f"{value} sparse-uid-base 204800 B/op 20 allocs/op"
                )
                with self.assertRaises(recorder.BenchmarkParseError):
                    recorder.parse_benchmarks(line)

    def test_nonfinite_number_is_rejected(self):
        for value in ("NaN", "Inf", "+Inf", "-Inf", "Infinity"):
            with self.subTest(value=value):
                line = (
                    "BenchmarkMetricControl-4 1 1234 ns/op "
                    f"{value} sparse-uid-base 204800 B/op 20 allocs/op"
                )
                with self.assertRaises(recorder.BenchmarkParseError):
                    recorder.parse_benchmarks(line)

    def test_missing_memory_metrics_are_rejected(self):
        for line in (
            "BenchmarkMetricControl-4 1 1234 ns/op 204800 B/op",
            "BenchmarkMetricControl-4 1 1234 ns/op 20 allocs/op",
            "BenchmarkMetricControl-4 1 1234 ns/op",
        ):
            with self.subTest(line=line):
                with self.assertRaises(recorder.BenchmarkParseError):
                    recorder.parse_benchmarks(line)

    def test_metrics_must_be_finite_after_conversion(self):
        record = recorder.parse_benchmarks(
            "BenchmarkMetricControl-4 1 1234 ns/op 1e308 one-metric 1e308 another-metric 1 B/op 1 allocs/op"
        )[0]
        self.assertTrue(math.isfinite(record["custom_metrics"]["one-metric"]))
        self.assertTrue(math.isfinite(record["custom_metrics"]["another-metric"]))


class BaselineValidationTests(unittest.TestCase):
    def test_healthy_three_samples_per_name_are_complete(self):
        errors = recorder.validate_baseline(healthy_records(), recorder.REQUIRED_BENCHMARKS, 3)
        self.assertEqual(errors, [])

    def test_empty_results_are_not_a_baseline(self):
        errors = recorder.validate_baseline([], recorder.REQUIRED_BENCHMARKS, 3)
        self.assertTrue(errors)
        self.assertTrue(any("no benchmark" in error.lower() for error in errors))

    def test_missing_required_benchmark_is_rejected(self):
        errors = recorder.validate_baseline([sample(CASE)] * 3, recorder.REQUIRED_BENCHMARKS, 3)
        self.assertTrue(any("missing benchmark" in error.lower() for error in errors))

    def test_truncated_sample_count_is_rejected_even_when_first_sample_is_complete(self):
        records = healthy_records()
        records = [record for record in records if record["name"] != CASE]
        records.extend(sample(CASE) for _ in range(1))
        errors = recorder.validate_baseline(records, recorder.REQUIRED_BENCHMARKS, 3)
        self.assertTrue(any("sample count 1" in error and CASE in error for error in errors))

    def test_extra_sample_count_is_rejected(self):
        records = healthy_records(samples=4)
        errors = recorder.validate_baseline(records, recorder.REQUIRED_BENCHMARKS, 3)
        self.assertTrue(any("sample count 4" in error and CASE in error for error in errors))

    def test_missing_custom_metric_on_second_sample_is_rejected(self):
        records = healthy_records()
        seen = 0
        for record in records:
            if record["name"] == CASE:
                seen += 1
                if seen == 2:
                    record["custom_metrics"].pop("fixture-occurrences", None)
                    break
        errors = recorder.validate_baseline(records, recorder.REQUIRED_BENCHMARKS, 3)
        self.assertTrue(any("missing metric fixture-occurrences" in error and CASE in error for error in errors))

    def test_missing_memory_metric_on_second_sample_is_rejected(self):
        records = healthy_records()
        seen = 0
        for record in records:
            if record["name"] == CASE:
                seen += 1
                if seen == 2:
                    record["bytes_per_op"] = None
                    break
        errors = recorder.validate_baseline(records, recorder.REQUIRED_BENCHMARKS, 3)
        self.assertTrue(any("missing B/op" in error and CASE in error for error in errors))

    def test_legitimate_partial_selection_is_valid(self):
        errors = recorder.validate_baseline([sample(CASE) for _ in range(3)], {}, 3)
        self.assertEqual(errors, [])

    def test_invalid_requested_count_is_rejected(self):
        for count in (0, -1, 1.5, True, "3"):
            with self.subTest(count=count):
                errors = recorder.validate_baseline(healthy_records(), recorder.REQUIRED_BENCHMARKS, count)
                self.assertTrue(any("requested count" in error.lower() for error in errors))


class RecorderCLITests(unittest.TestCase):
    def test_healthy_manifest_through_cli(self):
        with tempfile.TemporaryDirectory() as directory:
            path = write_manifest(directory, healthy_records(), count=3)
            result = run_cli("--check-manifest", str(path), "--count", "3")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('"validation_ok": true', result.stdout)
        self.assertIn('"complete": true', result.stdout)

    def test_truncated_manifest_through_cli_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = write_manifest(directory, healthy_records(samples=1), count=3)
            result = run_cli("--check-manifest", str(path), "--count", "3")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("sample count 1", result.stdout)

    def test_extra_manifest_through_cli_is_rejected(self):
        with tempfile.TemporaryDirectory() as directory:
            path = write_manifest(directory, healthy_records(samples=4), count=3)
            result = run_cli("--check-manifest", str(path), "--count", "3")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("sample count 4", result.stdout)

    def test_second_sample_missing_metric_through_cli_is_rejected(self):
        records = healthy_records()
        seen = 0
        for record in records:
            if record["name"] == CASE:
                seen += 1
                if seen == 2:
                    record["custom_metrics"].pop("fixture-occurrences", None)
                    break
        with tempfile.TemporaryDirectory() as directory:
            path = write_manifest(directory, records, count=3)
            result = run_cli("--check-manifest", str(path), "--count", "3")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("missing metric fixture-occurrences", result.stdout)

    def test_legitimate_partial_manifest_through_cli_is_incomplete_but_valid(self):
        with tempfile.TemporaryDirectory() as directory:
            path = write_manifest(directory, [sample(CASE) for _ in range(3)], count=3)
            result = run_cli("--check-manifest", str(path), "--count", "3", "--allow-partial")
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)
        self.assertIn('"validation_ok": true', result.stdout)
        self.assertIn('"complete": false', result.stdout)

    def test_manifest_count_is_used_when_count_is_omitted(self):
        with tempfile.TemporaryDirectory() as directory:
            path = write_manifest(directory, healthy_records(), count=3)
            result = run_cli("--check-manifest", str(path))
        self.assertEqual(result.returncode, 0, result.stdout + result.stderr)

    def test_missing_scenario_event_through_cli_is_rejected(self):
        events = healthy_scenario_events()
        package, test = next(iter(recorder.REQUIRED_SCENARIOS.items()))
        events = [event for event in events if not (event["package"] == package and event["test"] == test)]
        with tempfile.TemporaryDirectory() as directory:
            path = write_manifest(directory, healthy_records(), scenario_events=events)
            result = run_cli("--check-manifest", str(path), "--count", "3")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("missing scenario execution", result.stdout)

    def test_skipped_scenario_through_cli_is_rejected(self):
        events = healthy_scenario_events()
        for event in events:
            if event["action"] == "pass":
                event["action"] = "skip"
                break
        with tempfile.TemporaryDirectory() as directory:
            path = write_manifest(directory, healthy_records(), scenario_events=events)
            result = run_cli("--check-manifest", str(path), "--count", "3")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("was skipped", result.stdout)

    def test_failed_scenario_through_cli_is_rejected(self):
        events = healthy_scenario_events()
        for event in events:
            if event["action"] == "pass":
                event["action"] = "fail"
                break
        with tempfile.TemporaryDirectory() as directory:
            path = write_manifest(directory, healthy_records(), scenario_events=events)
            result = run_cli("--check-manifest", str(path), "--count", "3")
        self.assertEqual(result.returncode, 1, result.stdout + result.stderr)
        self.assertIn("failed", result.stdout)


if __name__ == "__main__":
    unittest.main()
