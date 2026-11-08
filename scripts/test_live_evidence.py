#!/usr/bin/env python3
"""Regression tests for the live evidence harness.

Run from the repository root:

    python3 -m unittest scripts/test_live_evidence.py -v

The tests use local environment values, temporary directories, and synthetic
Go test output. They never start the Go integration suite or use the network.
"""

import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

sys.path.insert(0, str(Path(__file__).resolve().parent))
import live_evidence  # noqa: E402


SCRIPT = Path(__file__).with_name("live_evidence.py")


def clean_env(**overrides):
    env = {key: value for key, value in os.environ.items() if not key.startswith("ICLOUD_")}
    env.update(overrides)
    return env


def valid_calendar_env(**overrides):
    env = {
        "ICLOUD_EMAIL": "user@example.com",
        "ICLOUD_PASSWORD": "app-specific-password",
        "ICLOUD_MCP_READ_ONLY": "true",
    }
    env.update(overrides)
    return env


def valid_mail_env(**overrides):
    env = valid_calendar_env(
        ICLOUD_MCP_READ_ONLY="false",
        ICLOUD_MCP_ENABLE_MAIL="true",
        ICLOUD_MCP_ENABLE_MAIL_WRITE="true",
        ICLOUD_MCP_ENABLE_MAIL_SEND="true",
        ICLOUD_MAIL_ADDRESS="user@example.com",
        ICLOUD_MAIL_PASSWORD="mail-app-specific-password",
        ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS="user@example.com",
        ICLOUD_MCP_INTEGRATION_WRITES="true",
        ICLOUD_MCP_INTEGRATION_CALENDAR="/123456789/calendars/disposable/",
        ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT="user@example.com",
    )
    env.update(overrides)
    return env


def run_cli(*args, env=None):
    return subprocess.run(
        [sys.executable, str(SCRIPT), *args],
        capture_output=True,
        text=True,
        timeout=60,
        check=False,
        env=env,
    )


class GateValidationTests(unittest.TestCase):
    def test_empty_environment_reports_calendar_credentials(self):
        problems, state = live_evidence.evaluate_gates({})
        self.assertIn("ICLOUD_EMAIL is required for Calendar integration", problems)
        self.assertIn("ICLOUD_PASSWORD is required for Calendar integration", problems)
        self.assertEqual(len(problems), 2)
        self.assertFalse(state["credentials"]["calendar"])

    def test_valid_calendar_environment_has_no_problems(self):
        problems, state = live_evidence.evaluate_gates(valid_calendar_env())
        self.assertEqual(problems, [])
        self.assertTrue(state["credentials"]["calendar"])
        self.assertFalse(state["mail"])

    def test_file_credentials_require_regular_private_files(self):
        with tempfile.TemporaryDirectory() as temp:
            root = Path(temp)
            email = root / "email"
            password = root / "password"
            email.write_text("user@example.com", encoding="ascii")
            password.write_text("app-specific-password", encoding="ascii")
            os.chmod(email, 0o600)
            os.chmod(password, 0o600)
            env = clean_env(
                ICLOUD_EMAIL=f"file://{email}",
                ICLOUD_PASSWORD=f"file://{password}",
                ICLOUD_MCP_READ_ONLY="true",
            )
            problems, _state = live_evidence.evaluate_gates(env)
            self.assertEqual(problems, [])

            os.chmod(password, 0o644)
            problems, _state = live_evidence.evaluate_gates(env)
            self.assertTrue(
                any("mode must be 0600 or stricter" in problem for problem in problems)
            )

    def test_mail_requires_address_when_enabled(self):
        env = valid_calendar_env(ICLOUD_MCP_ENABLE_MAIL="true")
        problems, _state = live_evidence.evaluate_gates(env)
        self.assertIn(
            "ICLOUD_MAIL_ADDRESS is required when Mail is enabled", problems
        )

    def test_mail_child_gate_requires_mail(self):
        env = valid_calendar_env(
            ICLOUD_MCP_READ_ONLY="false",
            ICLOUD_MCP_ENABLE_MAIL_WRITE="true",
        )
        problems, _state = live_evidence.evaluate_gates(env)
        self.assertIn(
            "ICLOUD_MCP_ENABLE_MAIL=true is required for Mail mutation or send",
            problems,
        )

    def test_mail_mutation_requires_read_only_false(self):
        env = valid_calendar_env(
            ICLOUD_MCP_READ_ONLY="true",
            ICLOUD_MCP_ENABLE_MAIL="true",
            ICLOUD_MCP_ENABLE_MAIL_WRITE="true",
            ICLOUD_MAIL_ADDRESS="user@example.com",
        )
        problems, _state = live_evidence.evaluate_gates(env)
        self.assertIn(
            "ICLOUD_MCP_READ_ONLY=false is required for Mail mutation or send",
            problems,
        )

    def test_destructive_gate_requires_calendar_selector(self):
        env = valid_calendar_env(
            ICLOUD_MCP_READ_ONLY="false",
            ICLOUD_MCP_INTEGRATION_WRITES="true",
        )
        problems, _state = live_evidence.evaluate_gates(env)
        self.assertTrue(
            any("ICLOUD_MCP_INTEGRATION_CALENDAR is required" in item for item in problems)
        )

        env["ICLOUD_MCP_INTEGRATION_CALENDAR"] = "/123456789/calendars/disposable/"
        problems, state = live_evidence.evaluate_gates(env)
        self.assertEqual(problems, [])
        self.assertTrue(state["integration_writes"])
        self.assertTrue(state["calendar_selector_set"])

    def test_destructive_flag_requires_write_gate(self):
        problems, _state = live_evidence.evaluate_gates(
            valid_calendar_env(), destructive=True
        )
        self.assertIn(
            "ICLOUD_MCP_INTEGRATION_WRITES=true is required for --destructive",
            problems,
        )

    def test_wildcard_recipient_policy_is_refused(self):
        env = valid_mail_env(ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS="*")
        problems, state = live_evidence.evaluate_gates(env)
        self.assertTrue(any("literal * is refused" in item for item in problems))
        self.assertFalse(state["smtp_policy_exact"])

    def test_mixed_wildcard_recipient_policy_is_refused(self):
        env = valid_mail_env(
            ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS="user@example.com,*"
        )
        problems, _state = live_evidence.evaluate_gates(env)
        self.assertTrue(any("literal * is refused" in item for item in problems))

    def test_exact_self_recipient_configuration_is_valid(self):
        problems, state = live_evidence.evaluate_gates(valid_mail_env())
        self.assertEqual(problems, [])
        self.assertTrue(state["mail_send"])
        self.assertTrue(state["smtp_policy_exact"])
        self.assertTrue(state["self_recipient_set"])

    def test_self_recipient_must_match_mail_address(self):
        env = valid_mail_env(
            ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT="other@example.com"
        )
        problems, _state = live_evidence.evaluate_gates(env)
        self.assertIn(
            "ICLOUD_MCP_INTEGRATION_SELF_RECIPIENT must exactly match the "
            "configured Mail address",
            problems,
        )

    def test_invalid_boolean_is_reported(self):
        env = valid_calendar_env(ICLOUD_MCP_ENABLE_MAIL="yes")
        problems, _state = live_evidence.evaluate_gates(env)
        self.assertTrue(
            any("must be unset, 0, false, 1, or true" in item for item in problems)
        )


class OutputDirectoryTests(unittest.TestCase):
    def test_repository_output_is_refused(self):
        with tempfile.TemporaryDirectory() as temp:
            repo = Path(temp) / "repo"
            repo.mkdir()
            with self.assertRaises(live_evidence.GateError) as raised:
                live_evidence.validate_output_directory(repo / "evidence", repo)
            self.assertIn("outside the repository", str(raised.exception))

    def test_relative_output_is_refused(self):
        with self.assertRaises(live_evidence.GateError) as raised:
            live_evidence.validate_output_directory(Path("evidence"), live_evidence.ROOT)
        self.assertIn("absolute path", str(raised.exception))

    def test_external_output_is_accepted(self):
        with tempfile.TemporaryDirectory() as temp:
            output = Path(temp) / "evidence"
            resolved = live_evidence.validate_output_directory(output, live_evidence.ROOT)
            self.assertEqual(resolved, output.resolve())

    def test_existing_non_empty_output_is_refused(self):
        with tempfile.TemporaryDirectory() as temp:
            output = Path(temp) / "evidence"
            output.mkdir()
            (output / "old.json").write_text("{}", encoding="ascii")
            with self.assertRaises(live_evidence.GateError) as raised:
                live_evidence.validate_output_directory(output, live_evidence.ROOT)
            self.assertIn("new or empty", str(raised.exception))


class RedactionTests(unittest.TestCase):
    def test_configured_secrets_and_protocol_credentials_are_removed(self):
        env = valid_mail_env()
        env["ICLOUD_MCP_INTEGRATION_CALENDAR"] = "/123456789/calendars/private/"
        secrets = live_evidence.secret_literals(env)
        text = (
            "user@example.com app-specific-password mail-app-specific-password "
            "Authorization: Basic dXNlcjpwYXNz\n"
            "SASL AUTH dXNlcjpwYXNz\n"
            "token=deadbeef\n"
            "Message-ID: <abc123@example.com>\n"
            "unlisted@example.net\n"
            "calendar=/123456789/calendars/private/"
        )
        redacted = live_evidence.redact_text(text, secrets)
        for secret in (
            "user@example.com",
            "app-specific-password",
            "mail-app-specific-password",
            "dXNlcjpwYXNz",
            "deadbeef",
            "abc123@example.com",
            "unlisted@example.net",
            "/123456789/calendars/private/",
        ):
            self.assertNotIn(secret, redacted)
        self.assertIn("[REDACTED]", redacted)
        self.assertIn("Message-ID: [REDACTED]", redacted)
        self.assertIn("[redacted-address]", redacted)

    def test_authorization_values_are_removed_without_configured_secrets(self):
        redacted = live_evidence.redact_text(
            "Authorization: Bearer abcdefghijkl\nBasic YWxpY2U6c2VjcmV0",
            [],
        )
        self.assertNotIn("abcdefghijkl", redacted)
        self.assertNotIn("YWxpY2U6c2VjcmV0", redacted)
        self.assertIn("[REDACTED]", redacted)


class ResultSummaryTests(unittest.TestCase):
    OUTPUT = """=== RUN   TestCalendarRead
--- PASS: TestCalendarRead (0.01s)
=== RUN   TestMailRead
    --- SKIP: TestMailRead/no_credentials (0.00s)
--- SKIP: TestMailRead (0.00s)
=== RUN   TestContactsWrite
--- FAIL: TestContactsWrite (0.02s)
FAIL
FAIL	github.com/ThomasCrouzet/icloud-mcp	0.10s
"""

    def test_results_are_parsed_with_status_and_duration(self):
        results = live_evidence.parse_results(self.OUTPUT)
        self.assertEqual(
            results,
            [
                {"name": "TestCalendarRead", "status": "pass", "duration": "0.01s"},
                {
                    "name": "TestMailRead/no_credentials",
                    "status": "skip",
                    "duration": "0.00s",
                },
                {"name": "TestMailRead", "status": "skip", "duration": "0.00s"},
                {"name": "TestContactsWrite", "status": "fail", "duration": "0.02s"},
            ],
        )
        summary = live_evidence.summarize_results(results, 1, False)
        self.assertFalse(summary["passed"])
        self.assertEqual(summary["counts"], {"pass": 1, "fail": 1, "skip": 2})
        self.assertTrue(summary["recorded_assertions"])

    def test_zero_exit_without_failures_passes(self):
        results = [{"name": "TestRead", "status": "pass", "duration": "0.01s"}]
        summary = live_evidence.summarize_results(results, 0, False)
        self.assertTrue(summary["passed"])
        self.assertEqual(summary["counts"], {"pass": 1, "fail": 0, "skip": 0})

    def test_timeout_fails_even_with_zero_exit(self):
        summary = live_evidence.summarize_results([], 0, True)
        self.assertFalse(summary["passed"])
        self.assertFalse(summary["recorded_assertions"])


class CommandTests(unittest.TestCase):
    def test_check_fails_closed_without_credentials(self):
        completed = run_cli("--check", env=clean_env())
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("live evidence gate check: FAIL", completed.stderr)
        self.assertIn("ICLOUD_EMAIL is required", completed.stderr)
        self.assertIn("ICLOUD_PASSWORD is required", completed.stderr)
        self.assertIn("network: not used", completed.stderr)

    def test_check_succeeds_with_calendar_credentials(self):
        completed = run_cli("--check", env=clean_env(**valid_calendar_env()))
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("live evidence gate check: OK", completed.stdout)
        self.assertIn("network: not used", completed.stdout)
        self.assertNotIn("user@example.com", completed.stdout)
        self.assertNotIn("app-specific-password", completed.stdout)

    def test_check_refuses_wildcard_policy(self):
        completed = run_cli(
            "--check",
            env=clean_env(
                **valid_mail_env(ICLOUD_MCP_SMTP_ALLOWED_RECIPIENTS="*")
            ),
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("literal * is refused", completed.stderr)

    def test_check_destructive_requires_write_gate(self):
        completed = run_cli(
            "--check", "--destructive", env=clean_env(**valid_calendar_env())
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn(
            "ICLOUD_MCP_INTEGRATION_WRITES=true is required for --destructive",
            completed.stderr,
        )

    def test_check_destructive_accepts_complete_write_gates(self):
        completed = run_cli(
            "--check", "--destructive", env=clean_env(**valid_mail_env())
        )
        self.assertEqual(completed.returncode, 0, completed.stderr)
        self.assertIn("Destructive request: on", completed.stdout)

    def test_run_requires_explicit_confirmation(self):
        completed = run_cli("--output", "/tmp/not-used", env=clean_env(**valid_calendar_env()))
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("--confirm-live", completed.stderr)

    def test_run_refuses_repository_output_before_any_test(self):
        target = live_evidence.ROOT / "live-evidence-test-output"
        completed = run_cli(
            "--confirm-live",
            "--output",
            str(target),
            env=clean_env(**valid_calendar_env()),
        )
        self.assertNotEqual(completed.returncode, 0)
        self.assertIn("outside the repository", completed.stderr)
        self.assertFalse(target.exists())


if __name__ == "__main__":
    unittest.main()
