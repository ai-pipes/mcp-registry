#!/bin/sh
# Downloads the native libraries the `onnx` build needs into <dest>, verifying every SHA-256
# (the downloads and the extracted files) against the values pinned in pins.env. Nothing downloaded
# is ever executed: archives are only listed and unpacked with tar (one named member), and the
# libraries are loaded later by the Go program. Idempotent: a library that is already in <dest>
# with the right hash is left alone. A mismatch is an error, never a warning.
#
# Usage (through `make native-libs` and the Dockerfile stage go-native):
#   fetch.sh <platform> <dest>          platform: darwin-arm64, linux-arm64 or linux-amd64
# The URLs and hashes are read from pins.env next to this script (the only list of them).
set -eu

platform=${1:?platform}
dest=${2:?dest}

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=pins.env
. "$here/pins.env"
key=$(echo "$platform" | tr '-' '_')
get() { eval "printf '%s' \"\${$1_$key:-}\""; }
ORT_MEMBER=$(get ORT_MEMBER)
ORT_FILE=$(get ORT_FILE)
ORT_FILE_SHA256=$(get ORT_SHA)
TOK_TGZ_SHA256=$(get TOK_SHA_TGZ)
TOK_FILE_SHA256=$(get TOK_SHA)
ORT_URL=$ORT_NPM_URL
ORT_TGZ_SHA256=$ORT_NPM_SHA256
TOK_URL="$TOK_BASE_URL/libtokenizers.$platform.tar.gz"
[ -n "$ORT_FILE" ] && [ -n "$ORT_MEMBER" ] && [ -n "$ORT_FILE_SHA256" ] && [ -n "$TOK_TGZ_SHA256" ] && [ -n "$TOK_FILE_SHA256" ] || { echo "native-libs: no pinned libraries for $platform" >&2; exit 1; }

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

fail() { echo "native-libs: $*" >&2; exit 1; }

# have <file> <sha256>: the file exists with that hash.
have() { [ -f "$1" ] && [ "$(sha256 "$1")" = "$2" ]; }

work=$(mktemp -d "${TMPDIR:-/tmp}/native-libs.XXXXXX")
trap 'rm -rf "$work"' EXIT
trap 'rm -rf "$work"; exit 1' INT TERM
mkdir -p "$dest"

# fetch <url> <expected sha256> <out>: download and verify.
fetch() {
	echo "native-libs: downloading $1"
	curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 --connect-timeout 20 -o "$3" "$1" || fail "download of $1 failed"
	got=$(sha256 "$3")
	[ "$got" = "$2" ] || fail "$1: sha256 $got, expected $2"
}

# unpack <archive> <member> <expected sha256> <target>: one member out of a tar.gz, verified.
unpack() {
	mkdir -p "$work/x"
	tar -xzf "$1" -C "$work/x" "$2" || fail "$2 is not in the archive"
	got=$(sha256 "$work/x/$2")
	[ "$got" = "$3" ] || fail "$2: sha256 $got, expected $3"
	mv "$work/x/$2" "$4.partial"
	mv "$4.partial" "$4"
}

if have "$dest/$ORT_FILE" "$ORT_FILE_SHA256"; then
	echo "native-libs: $ORT_FILE ok ($platform)"
else
	rm -f "$dest/$ORT_FILE"
	fetch "$ORT_URL" "$ORT_TGZ_SHA256" "$work/ort.tgz"
	unpack "$work/ort.tgz" "$ORT_MEMBER" "$ORT_FILE_SHA256" "$dest/$ORT_FILE"
	rm -f "$work/ort.tgz"
	echo "native-libs: $ORT_FILE installed ($platform)"
fi

if have "$dest/libtokenizers.a" "$TOK_FILE_SHA256"; then
	echo "native-libs: libtokenizers.a ok ($platform)"
else
	rm -f "$dest/libtokenizers.a"
	fetch "$TOK_URL" "$TOK_TGZ_SHA256" "$work/tok.tgz"
	unpack "$work/tok.tgz" libtokenizers.a "$TOK_FILE_SHA256" "$dest/libtokenizers.a"
	echo "native-libs: libtokenizers.a installed ($platform)"
fi
