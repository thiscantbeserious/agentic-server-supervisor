#!/bin/bash
# fetch-debian-cloud-image.sh OUT_QCOW2
#
# Downloads a pinned Debian 13 (trixie) genericcloud amd64 image. Pinned to
# a dated snapshot, never the "latest" symlink: "latest" changing underneath
# an unrelated push would move both VM jobs' baseline without anything in
# this repository recording that it happened, and would silently invalidate
# the OMV base image's content-hash tag's meaning (the tag would still
# match its inputs textually while the base it names had quietly changed).
#
# genericcloud rather than plain "generic": it is the smaller of the two
# cloud-init-ready images and both variants need nothing the plain
# "generic" image adds (this is disposable CI infrastructure, not a target
# platform of its own).
set -eu
set -o pipefail

OUT="${1:?usage: fetch-debian-cloud-image.sh OUT_QCOW2}"

DEBIAN_CLOUD_IMAGE_URL="${DEBIAN_CLOUD_IMAGE_URL:-https://cloud.debian.org/images/cloud/trixie/20250814-2204/debian-13-genericcloud-amd64-20250814-2204.qcow2}"
DEBIAN_CLOUD_IMAGE_SHA512_URL="${DEBIAN_CLOUD_IMAGE_URL%/*}/SHA512SUMS"

curl -fsSL -o "$OUT" "$DEBIAN_CLOUD_IMAGE_URL"

# Verified against the image's own published checksum file rather than a
# hash pinned in this repo: the repo would otherwise need updating every
# time the snapshot below changes, and a stale local pin fails exactly the
# way a stale cache does, wrong and silent. The URL is still pinned, so
# what this integrity-checks against cannot itself drift.
sums="$(curl -fsSL "$DEBIAN_CLOUD_IMAGE_SHA512_URL")"
want="$(printf '%s\n' "$sums" | awk -v f="$(basename "$DEBIAN_CLOUD_IMAGE_URL")" '$2 == f || $2 == "*"f {print $1}')"
if [ -z "$want" ]; then
  echo "fetch-debian-cloud-image: $(basename "$DEBIAN_CLOUD_IMAGE_URL") not listed in SHA512SUMS, refusing to trust an unverified image" >&2
  exit 1
fi
got="$(sha512sum "$OUT" | cut -d' ' -f1)"
if [ "$got" != "$want" ]; then
  echo "fetch-debian-cloud-image: checksum mismatch for $OUT (got $got, want $want)" >&2
  exit 1
fi
echo "fetch-debian-cloud-image: OK, $OUT verified against upstream SHA512SUMS"
