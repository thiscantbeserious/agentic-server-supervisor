#!/bin/sh
# deploy/agy-build-args.sh — R1 ops helper. Fetches the vendor's own
# manifest — once per architecture, amd64 AND arm64 (contracts/runtime.md
# R1: the image is built and shipped for both) — and prints the full
# --build-arg set for the CURRENT agy release, so nobody hand-assembles
# a download URL and the operator sees the version they are about to pin
# before pinning it.
#
#   docker buildx build -f deploy/Dockerfile -t sentinel:dev \
#     --platform linux/amd64,linux/arm64 \
#     $(deploy/agy-build-args.sh) \
#     .
#
# This script RESOLVES; it never builds and never downloads either
# tarball itself. The vendor installer is deliberately not run here
# either — it resolves to latest with no record of which version an
# image contains, discarding the reproducibility the pin exists to
# provide (contracts/runtime.md R1). POSIX sh, no jq — the vendor's own
# installer parses this exact manifest with sed, so this reuses that
# approach rather than adding a dependency (C1: stdlib/no-new-deps in the
# runtime path; this is the one ops-side bash artifact already sanctioned
# for install.sh, and the same reasoning applies here).
set -eu

MANIFEST_BASE="https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests"

fetch() {
  if command -v curl >/dev/null 2>&1; then
    curl -fsSL "$1"
  elif command -v wget >/dev/null 2>&1; then
    wget -qO- "$1"
  else
    echo "agy-build-args.sh: need curl or wget on PATH" >&2
    exit 1
  fi
}

# Extract a top-level "key": "value" pair with sed — the manifest is
# flat JSON today (no nesting), same assumption the vendor's own
# installer makes about its own file. The occurrence count must be built
# from a genuinely-quoted key: writing the pattern as adjacent strings —
# `""$key""` — lets the shell collapse the empty-quote pairs around it,
# so the grep pattern ends up unquoted and matches nothing against a
# real, correctly-quoted manifest key. Assigning the key to its own
# variable first and quoting it explicitly in the pattern
# (`""$key"..."`) keeps the quoting literal instead of depending on
# string concatenation. `grep -o` emits one line per occurrence (works
# on a single-line/minified document too), and anchoring on the literal
# key-plus-colon means a VALUE containing the substring cannot inflate
# the count.
field() {
  manifest_json="$1"
  key="$2"
  count="$(printf '%s' "$manifest_json" | grep -o "\"$key\"[[:space:]]*:" | grep -c . || true)"
  if [ "$count" -ne 1 ]; then
    echo "agy-build-args.sh: manifest has $count occurrences of \"$key\", expected exactly 1 — refusing to guess" >&2
    exit 1
  fi
  printf '%s\n' "$manifest_json" | sed -n 's/.*"'"$key"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

# resolve_arch ARCH prints "url sha512 version" (space-separated, none of
# which can themselves contain a space in a well-formed manifest) for one
# architecture's manifest. Every failure path names the architecture, so
# a build breaks with something the operator can act on rather than a
# bare "field missing".
resolve_arch() {
  arch="$1"
  manifest_url="$MANIFEST_BASE/linux_${arch}.json"
  manifest_json="$(fetch "$manifest_url")" || {
    echo "agy-build-args.sh: could not fetch $manifest_url" >&2
    exit 1
  }

  version="$(field "$manifest_json" version)"
  url="$(field "$manifest_json" url)"
  sha512="$(field "$manifest_json" sha512)"

  if [ -z "$version" ] || [ -z "$url" ] || [ -z "$sha512" ]; then
    echo "agy-build-args.sh: $manifest_url missing version/url/sha512 — got:" >&2
    printf '%s\n' "$manifest_json" >&2
    exit 1
  fi

  # Defense-in-depth, not a real integrity boundary (the sha512 comes
  # from the same document as the URL) — but it costs one line to refuse
  # a plaintext download outright rather than silently downgrading a
  # value about to be handed straight to wget/curl.
  case "$url" in
    https://*) ;;
    *) echo "agy-build-args.sh: $manifest_url URL is not https:// — refusing: $url" >&2; exit 1 ;;
  esac

  echo "agy-build-args.sh: resolved agy $version ($arch) from $manifest_url" >&2
  printf '%s %s %s\n' "$url" "$sha512" "$version"
}

amd64_line="$(resolve_arch amd64)"
arm64_line="$(resolve_arch arm64)"

amd64_url="$(printf '%s' "$amd64_line" | cut -d' ' -f1)"
amd64_sha512="$(printf '%s' "$amd64_line" | cut -d' ' -f2)"
amd64_version="$(printf '%s' "$amd64_line" | cut -d' ' -f3)"
arm64_url="$(printf '%s' "$arm64_line" | cut -d' ' -f1)"
arm64_sha512="$(printf '%s' "$arm64_line" | cut -d' ' -f2)"
arm64_version="$(printf '%s' "$arm64_line" | cut -d' ' -f3)"

if [ "$amd64_version" != "$arm64_version" ]; then
  echo "agy-build-args.sh: WARNING: amd64 manifest is $amd64_version but arm64 manifest is $arm64_version — the vendor has released asynchronously; using $amd64_version for AGY_VERSION (an OCI label only, does not affect which binary is fetched)" >&2
fi

printf -- '--build-arg AGY_URL_AMD64=%s --build-arg AGY_SHA512_AMD64=%s --build-arg AGY_URL_ARM64=%s --build-arg AGY_SHA512_ARM64=%s --build-arg AGY_VERSION=%s\n' \
  "$amd64_url" "$amd64_sha512" "$arm64_url" "$arm64_sha512" "$amd64_version"
