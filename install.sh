#!/bin/sh
# Install cairn from a GitHub release.
#
#   curl -fsSL https://raw.githubusercontent.com/s-aga-r/cairn/develop/install.sh | sh
#   curl -fsSL https://raw.githubusercontent.com/s-aga-r/cairn/develop/install.sh | sh -s -- v1.2.0
#
# The first argument, or CAIRN_VERSION, selects the version. The default is
# the latest release.
#
# Environment:
#   CAIRN_VERSION      version to install, for example v1.2.0 (default: latest)
#   CAIRN_INSTALL_DIR  directory for the binary (default: /usr/local/bin when
#                      writable, otherwise ~/.local/bin)
#   CAIRN_BASE_URL     download location (default: the GitHub releases of the
#                      repository). Use it for a mirror.
set -eu

repo="s-aga-r/cairn"
version="${1:-${CAIRN_VERSION:-latest}}"

main() {
	asset="cairn_$(detect_os)_$(detect_arch).tar.gz"
	base="$(base_url)"
	work="$(mktemp -d)"
	trap 'rm -rf "$work"' EXIT

	echo "Downloading $asset ($version)"
	fetch "$base/$asset" "$work/$asset"
	fetch "$base/checksums.txt" "$work/checksums.txt"
	verify_checksum

	tar -xzf "$work/$asset" -C "$work"
	dir="$(install_dir)"
	mkdir -p "$dir"
	install -m 0755 "$work/cairn" "$dir/cairn"

	echo "Installed $("$dir/cairn" --version) to $dir/cairn"
	case ":$PATH:" in
	*":$dir:"*) ;;
	*) echo "Add $dir to your PATH to run cairn." ;;
	esac
}

detect_os() {
	case "$(uname -s)" in
	Linux) echo linux ;;
	Darwin) echo darwin ;;
	*) fail "unsupported operating system: $(uname -s)" ;;
	esac
}

detect_arch() {
	case "$(uname -m)" in
	x86_64 | amd64) echo amd64 ;;
	aarch64 | arm64) echo arm64 ;;
	*) fail "unsupported architecture: $(uname -m)" ;;
	esac
}

base_url() {
	if [ -n "${CAIRN_BASE_URL:-}" ]; then
		echo "$CAIRN_BASE_URL"
	elif [ "$version" = "latest" ]; then
		echo "https://github.com/$repo/releases/latest/download"
	else
		# Accept both 1.2.0 and v1.2.0.
		echo "https://github.com/$repo/releases/download/v${version#v}"
	fi
}

install_dir() {
	if [ -n "${CAIRN_INSTALL_DIR:-}" ]; then
		echo "$CAIRN_INSTALL_DIR"
	elif [ -w /usr/local/bin ]; then
		echo /usr/local/bin
	else
		echo "$HOME/.local/bin"
	fi
}

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1" -o "$2" || fail "download failed: $1"
	elif command -v wget >/dev/null 2>&1; then
		wget -q "$1" -O "$2" || fail "download failed: $1"
	else
		fail "curl or wget is required"
	fi
}

verify_checksum() {
	expected="$(awk -v name="$asset" '$2 == name {print $1}' "$work/checksums.txt")"
	[ -n "$expected" ] || fail "checksums.txt has no entry for $asset"
	if command -v sha256sum >/dev/null 2>&1; then
		actual="$(sha256sum "$work/$asset" | awk '{print $1}')"
	else
		actual="$(shasum -a 256 "$work/$asset" | awk '{print $1}')"
	fi
	[ "$expected" = "$actual" ] || fail "checksum mismatch for $asset"
}

fail() {
	echo "install.sh: $1" >&2
	exit 1
}

main
