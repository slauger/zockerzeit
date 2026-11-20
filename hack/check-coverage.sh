#!/usr/bin/env bash
# Fails if the total statement coverage in a Go cover profile is below a threshold.
# Usage: hack/check-coverage.sh cover.out 80
set -euo pipefail

profile="${1:?usage: $0 <cover.out> <threshold>}"
threshold="${2:?usage: $0 <cover.out> <threshold>}"

# cmd/ and the demo simulator are exercised manually, not by unit tests.
filtered="$(mktemp)"
trap 'rm -f "$filtered"' EXIT
grep -vE '/cmd/|/internal/demo/' "$profile" > "$filtered"

total="$(go tool cover -func="$filtered" | awk '/^total:/ {gsub("%", "", $3); print $3}')"
echo "total coverage: ${total}% (threshold ${threshold}%)"
if awk -v t="$total" -v min="$threshold" 'BEGIN { exit !(t < min) }'; then
  echo "coverage below threshold" >&2
  exit 1
fi
