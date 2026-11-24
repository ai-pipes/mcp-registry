#!/bin/sh
# Downloads the embedding model into <dest>, in the layout the embedder reads offline:
# <dest>/<MODEL_NAME>/<file>, the files named in pins.env (MODEL_FILE_<n>). The revision and the SHA-256 of
# every file are read from pins.env next to this script (the only list of them). A file already in
# <dest> with the right hash is left alone; a download is written to a .partial file, checked and
# only then renamed, so a mismatch leaves nothing behind and is an error, never a warning. Nothing
# downloaded is executed. Needs curl and sha256sum (or shasum), no Node.
#
# Usage (through `make native-model` and the Dockerfile stage model):
#   fetch-model.sh <dest>
set -eu

dest=${1:?dest}

here=$(cd "$(dirname "$0")" && pwd)
# shellcheck source=pins.env
. "$here/pins.env"
[ -n "${MODEL_NAME:-}" ] && [ -n "${MODEL_REVISION:-}" ] || { echo "native-model: pins.env has no model" >&2; exit 1; }

sha256() {
	if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1; else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

fail() { echo "native-model: $*" >&2; exit 1; }

n=1
while :; do
	eval "file=\${MODEL_FILE_$n:-}"
	eval "want=\${MODEL_SHA256_$n:-}"
	[ -n "$file" ] || break
	[ -n "$want" ] || fail "no SHA-256 pinned for $file"
	target="$dest/$MODEL_NAME/$file"
	if [ -f "$target" ] && [ "$(sha256 "$target")" = "$want" ]; then
		echo "native-model: $file ok (cached)"
	else
		mkdir -p "$(dirname "$target")"
		rm -f "$target" "$target.partial"
		url="https://huggingface.co/$MODEL_NAME/resolve/$MODEL_REVISION/$file"
		echo "native-model: downloading $url"
		curl -fsSL --proto '=https' --proto-redir '=https' --retry 3 --retry-all-errors --connect-timeout 20 --max-time 600 -o "$target.partial" "$url" ||
			{ rm -f "$target.partial"; fail "download of $url failed"; }
		got=$(sha256 "$target.partial")
		if [ "$got" != "$want" ]; then
			rm -f "$target.partial"
			fail "$url: sha256 $got, expected $want"
		fi
		mv "$target.partial" "$target"
		echo "native-model: $file ok"
	fi
	n=$((n + 1))
done
[ "$n" -gt 1 ] || fail "pins.env lists no model file"
echo "native-model: $MODEL_NAME@$MODEL_REVISION is in $dest/$MODEL_NAME"
