#!/usr/bin/env bash
# Test install.sh against a release that is built and served locally.
set -euo pipefail

cd "$(dirname "$0")/.."
work="$(mktemp -d)"
trap 'kill "$server" 2>/dev/null; rm -rf "$work"' EXIT

scripts/build-release.sh v9.9.9 "$work/release" >/dev/null
python3 -m http.server 18765 --bind 127.0.0.1 --directory "$work/release" >/dev/null 2>&1 &
server=$!
sleep 1

export CAIRN_BASE_URL="http://127.0.0.1:18765" CAIRN_INSTALL_DIR="$work/bin"
sh install.sh
[ "$("$work/bin/cairn" --version)" = "cairn version 9.9.9" ] || { echo "wrong version installed" >&2; exit 1; }

# A corrupt download must not be installed.
rm "$work/bin/cairn"
echo corrupt >>"$work/release/cairn_$(go env GOOS)_$(go env GOARCH).tar.gz"
if sh install.sh 2>/dev/null || [ -e "$work/bin/cairn" ]; then
	echo "install.sh accepted a corrupt archive" >&2
	exit 1
fi
echo "install.sh works"
