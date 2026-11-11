#!/usr/bin/env bash
# Run the test suite against real PostgreSQL, MariaDB, and S3 services,
# and fail unless statement coverage is 100%.
#
#   scripts/test.sh [go test flags]
#
# Set CAIRN_TEST_ROOT to reuse services already started with
# scripts/test-env.sh; otherwise they start in a temporary directory.
set -euo pipefail

cd "$(dirname "$0")/.."

if [ -z "${CAIRN_TEST_ROOT:-}" ]; then
	CAIRN_TEST_ROOT="$(mktemp -d)"
	scripts/test-env.sh start "$CAIRN_TEST_ROOT"
	trap 'scripts/test-env.sh stop "$CAIRN_TEST_ROOT"; rm -rf "$CAIRN_TEST_ROOT"' EXIT
fi
export CAIRN_TEST_CONFIG="$CAIRN_TEST_ROOT/cairn.json"

# The test helper package holds no product code, so it is not measured.
packages="$(go list ./... | grep -v /internal/testenv | paste -sd, -)"
profile="${CAIRN_COVER_PROFILE:-coverage.out}"

# -p 1: the packages share the database servers.
go test -p 1 -count=1 -coverpkg="$packages" -coverprofile="$profile" "$@" ./...

total="$(go tool cover -func="$profile" | awk '/^total:/ {print $3}')"
echo "Statement coverage: $total"
if [ "$total" != "100.0%" ]; then
	echo "Coverage is below 100%. Uncovered:" >&2
	go tool cover -func="$profile" | awk '$3 != "100.0%" && !/^total:/' >&2
	exit 1
fi
