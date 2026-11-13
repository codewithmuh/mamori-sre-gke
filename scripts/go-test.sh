#!/usr/bin/env bash
# Runs one Go module's tests with the race detector and fails below 80 % total coverage.
# The same in CI and in `make test`.
#
# Usage: scripts/go-test.sh MODULE_DIR   (for example tools/spend-guard)
#   COVERAGE_MIN  default 80
set -euo pipefail

dir=${1:?usage: $0 MODULE_DIR}
min=${COVERAGE_MIN:-80}

cd "$dir"
go test -race -covermode=atomic -coverprofile=coverage.out ./...
total=$(go tool cover -func=coverage.out | awk '/^total:/ {sub("%", "", $3); print $3}')
echo "${dir}: total coverage ${total}%"
if awk -v t="$total" -v m="$min" 'BEGIN { exit !(t + 0 < m) }'; then
  message="${dir}: Go coverage ${total}% is below ${min}%"
  if [[ ${GITHUB_ACTIONS:-} == true ]]; then
    echo "::error::${message}"
  else
    echo "error: ${message}" >&2
  fi
  exit 1
fi
