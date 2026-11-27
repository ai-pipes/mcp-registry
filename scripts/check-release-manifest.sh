#!/usr/bin/env bash
# Release guard: reads a manifest list (OCI image index or Docker manifest list) as JSON on stdin and
# fails unless its images are exactly two, linux/amd64 with the manifest digest $1 and linux/arm64
# with $2. The attestation manifests buildx adds (platform unknown/unknown, annotation
# vnd.docker.reference.type=attestation-manifest) are allowed only when they describe one of those
# two images. Used by .github/workflows/release.yml after the merge (once per tag) and by the
# rehearsal's dry assembly (on the output of `docker buildx imagetools create --dry-run`).
#   docker buildx imagetools inspect --raw <ref> | bash scripts/check-release-manifest.sh <amd64> <arm64>
# Needs jq. Exit codes: 0 match, 1 mismatch or unreadable input, 2 bad invocation.
set -euo pipefail

digest_re='^sha256:[0-9a-f]{64}$'
if [ "$#" -ne 2 ] || ! [[ "$1" =~ $digest_re ]] || ! [[ "$2" =~ $digest_re ]]; then
  echo "usage: bash scripts/check-release-manifest.sh <amd64 manifest digest> <arm64 manifest digest> < index.json" >&2
  exit 2
fi

problems="$(jq -r --arg amd64 "$1" --arg arm64 "$2" '
  def platform: "\(.platform.os // "?")/\(.platform.architecture // "?")\(if .platform.variant then "/" + .platform.variant else "" end)";
  def attestation: (.annotations // {})["vnd.docker.reference.type"] == "attestation-manifest";
  (if (.mediaType == "application/vnd.oci.image.index.v1+json"
       or .mediaType == "application/vnd.docker.distribution.manifest.list.v2+json")
   then empty else "not a manifest list (mediaType \(.mediaType // "none"))" end),
  ([(.manifests // [])[] | select(attestation | not)] as $images
   | (if ($images | length) == 2 then empty
      else "\($images | length) image(s) \([$images[] | platform]), want exactly linux/amd64 and linux/arm64" end),
     ($images[] | select(platform != "linux/amd64" and platform != "linux/arm64")
      | "unexpected platform \(platform) (\(.digest))"),
     ({"linux/amd64": $amd64, "linux/arm64": $arm64} | to_entries[] as $want
      | [$images[] | select(platform == $want.key)] as $found
      | if ($found | length) != 1 then "\($found | length) image(s) for \($want.key), want 1"
        elif $found[0].digest != $want.value then "\($want.key) is \($found[0].digest), want \($want.value)"
        else empty end)),
  ((.manifests // [])[] | select(attestation)
   | (.annotations["vnd.docker.reference.digest"] // "") as $ref
   | if $ref == $amd64 or $ref == $arm64 then empty
     else "attestation \(.digest) describes \($ref), not one of the two images" end)
')" || { echo "check-release-manifest: the input is not a JSON manifest list" >&2; exit 1; }

if [ -n "$problems" ]; then
  echo "check-release-manifest: refused:" >&2
  sed 's/^/  /' <<<"$problems" >&2
  exit 1
fi
echo "manifest list: linux/amd64 $1, linux/arm64 $2"
