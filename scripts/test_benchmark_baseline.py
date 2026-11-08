#!/usr/bin/env python3
"""Regression tests for the offline benchmark recorder."""

import math
import unittest

import benchmark_baseline as recorder


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
    def test_empty_results_are_not_a_baseline(self):
        errors = recorder.validate_baseline([], recorder.REQUIRED_BENCHMARKS)
        self.assertTrue(errors)
        self.assertTrue(any("no benchmark" in error.lower() for error in errors))

    def test_missing_required_benchmark_is_rejected(self):
        record = {
            "name": "BenchmarkRecurrenceExpansion/DSTSpringForward",
            "bytes_per_op": 1,
            "allocs_per_op": 1,
            "custom_metrics": {
                "fixture-occurrences": 40,
                "occurrence-cap": 2000,
                "truncated": 0,
            },
        }
        errors = recorder.validate_baseline([record], recorder.REQUIRED_BENCHMARKS)
        self.assertTrue(any("missing benchmark" in error.lower() for error in errors))

    def test_missing_custom_metric_is_rejected(self):
        record = {
            "name": "BenchmarkRecurrenceExpansion/DSTSpringForward",
            "bytes_per_op": 1,
            "allocs_per_op": 1,
            "custom_metrics": {"fixture-occurrences": 40},
        }
        errors = recorder.validate_baseline([record], recorder.REQUIRED_BENCHMARKS)
        self.assertTrue(any("missing metric" in error.lower() for error in errors))

    def test_missing_memory_metric_is_rejected(self):
        record = {
            "name": "BenchmarkRecurrenceExpansion/DSTSpringForward",
            "bytes_per_op": None,
            "allocs_per_op": 1,
            "custom_metrics": {
                "fixture-occurrences": 40,
                "occurrence-cap": 2000,
                "truncated": 0,
            },
        }
        errors = recorder.validate_baseline([record], recorder.REQUIRED_BENCHMARKS)
        self.assertTrue(any("B/op" in error for error in errors))


if __name__ == "__main__":
    unittest.main()
