#!/usr/bin/env bash
# Enforce the aggregate and per-package coverage floors for a Go coverprofile.
# Usage: scripts/check_coverage.sh [coverage.out]
set -euo pipefail

profile="${1:-coverage.out}"
threshold="${COVERAGE_THRESHOLD:-78}"

if [ ! -f "$profile" ]; then
  echo "coverage profile not found: $profile" >&2
  exit 1
fi

pct="$(go tool cover -func="$profile" | tail -1 | awk '{print $3}' | tr -d '%')"
echo "Total coverage: ${pct}% (threshold: ${threshold}%)"
awk -v p="$pct" -v t="$threshold" 'BEGIN { if (p + 0 < t) { exit 1 } }'

check_package_coverage() {
  package="$1"
  floor="$2"
  prefix="$(go list -f '{{.ImportPath}}' "$package")/"
  pct="$(awk -v prefix="$prefix" '
    NR > 1 {
      file = $1
      sub(/:.*/, "", file)
      if (index(file, prefix) != 1) next
      rest = substr(file, length(prefix) + 1)
      if (index(rest, "/") != 0) next
      statements = $(NF - 1)
      total += statements
      if ($NF > 0) covered += statements
    }
    END {
      if (total == 0) exit 2
      printf "%.1f", 100 * covered / total
    }
  ' "$profile")"
  echo "$package coverage: ${pct}% (threshold: ${floor}%)"
  awk -v p="$pct" -v t="$floor" 'BEGIN { if (p + 0 < t) exit 1 }'
}

check_package_coverage ./internal/contacts 65
check_package_coverage ./internal/mail 65
check_package_coverage ./internal/mail/imapadapter 60
check_package_coverage ./internal/mcptools 75
check_package_coverage ./internal/security 80
check_package_coverage ./internal/icloud 78
check_package_coverage ./internal/config 85
check_package_coverage ./internal/health 90
check_package_coverage ./cmd/icloud-mcp 55
