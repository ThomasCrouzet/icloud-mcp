#!/usr/bin/env bash
# Reproduce the shared local and CI checks without publishing anything.
# CI-only checks remain golangci-lint, govulncheck, multi-architecture builds,
# fuzz smoke tests, gitleaks, and the release and registry jobs.
set -euo pipefail

tmp="$(mktemp -d "${TMPDIR:-/tmp}/icloud-mcp-check.XXXXXX")"
trap 'rm -rf "$tmp"' EXIT

unformatted="$(gofmt -l .)"
if [ -n "$unformatted" ]; then
  echo "Unformatted files (run gofmt -w):" >&2
  echo "$unformatted" >&2
  exit 1
fi

go vet ./...
python3 scripts/security_guards.py .
python3 scripts/check_docs.py
python3 -m unittest scripts/test_check_docs.py
python3 scripts/check_platforms.py
python3 -m unittest scripts/test_check_platforms.py
go test ./... -race -coverprofile="$tmp/coverage.out" -covermode=atomic
python3 scripts/protocol_evidence.py --output "$tmp/protocol-evidence"
python3 scripts/protocol_evidence.py --merge-coverage "$tmp/coverage.out" \
  "$tmp/coverage.out" "$tmp/protocol-evidence/executable-coverage.out"
bash scripts/check_coverage.sh "$tmp/coverage.out"
