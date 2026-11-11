#!/usr/bin/env bash
# Build the release archives and checksums.txt for one version.
#
#   scripts/build-release.sh <version> <output dir>
set -euo pipefail

version="${1:?usage: build-release.sh <version> <output dir>}"
out="$(mkdir -p "${2:?usage: build-release.sh <version> <output dir>}" && cd "$2" && pwd)"
cd "$(dirname "$0")/.."

for target in linux/amd64 linux/arm64 darwin/amd64 darwin/arm64; do
	os="${target%/*}" arch="${target#*/}"
	stage="$(mktemp -d)"
	CGO_ENABLED=0 GOOS="$os" GOARCH="$arch" go build -trimpath \
		-ldflags "-s -w -X github.com/s-aga-r/cairn/internal/cli.Version=${version#v}" \
		-o "$stage/cairn" ./cmd/cairn
	cp LICENSE README.md "$stage/"
	# The archive name has no version, so releases/latest/download finds it.
	tar -czf "$out/cairn_${os}_${arch}.tar.gz" -C "$stage" cairn LICENSE README.md
	rm -rf "$stage"
done

(cd "$out" && sha256sum cairn_*.tar.gz >checksums.txt)
echo "Release $version built in $out"
