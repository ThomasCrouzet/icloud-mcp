import subprocess
import sys
import unittest
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


class VerifyReleaseCandidatesTest(unittest.TestCase):
    def test_selftest_rejects_altered_and_missing_candidates(self):
        result = subprocess.run(
            [sys.executable, str(ROOT / "scripts/verify_release_candidates.py"), "selftest"],
            cwd=ROOT, capture_output=True, text=True, timeout=60,
        )
        self.assertEqual(result.returncode, 0, result.stderr)
        self.assertIn("self-test passed", result.stdout)


if __name__ == "__main__":
    unittest.main()
