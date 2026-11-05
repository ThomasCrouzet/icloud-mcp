#!/usr/bin/env python3
"""Compatibility wrapper for scripts/check_release.py registry."""

import sys

from check_release import main

if __name__ == "__main__":
    sys.exit(main(["registry"]))
