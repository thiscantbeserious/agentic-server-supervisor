#!/bin/sh
# deploy/agy-build-args.sh — R1 ops helper. Fetches the vendor's own
# manifest and prints the --build-arg triple for the CURRENT agy release,
# so nobody hand-assembles a download URL and the operator sees the
# version they are about to pin before pinning it.
#
#   docker build -f deploy/Dockerfile -t sentinel:dev \
#     $(deploy/agy-build-args.sh) \
#     .
#
# This script RESOLVES; it never builds and never downloads the tarball
# itself. The vendor installer is deliberately not run here either — it
# resolves to latest with no record of which version an image contains,
# discarding the reproducibility the pin exists to provide
# (contracts/runtime.md R1). POSIX sh, no jq — the vendor's own installer
# parses this exact manifest with sed, so this reuses that approach
# rather than adding a dependency (C1: stdlib/no-new-deps in the runtime
# path; this is the one ops-side bash artifact already sanctioned for
# install-host.sh, and the same reasoning applies here).
set -eu

MANIFEST_URL="https://antigravity-cli-auto-updater-974169037036.us-central1.run.app/manifests/linux_amd64.json"

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

json="$(fetch "$MANIFEST_URL")" || {
  echo "agy-build-args.sh: could not fetch $MANIFEST_URL" >&2
  exit 1
}

# Extract a top-level "key": "value" pair with sed — the manifest is
# flat JSON today (no nesting), same assumption the vendor's own
# installer makes about its own file. Round-4 review item 2: that
# assumption is about a remote document that can change without notice,
# and a greedy match would silently take the LAST occurrence instead of
# failing if it ever nests — this round exists because a synthetic
# fixture certified a wrong assumption about a vendor artifact, so the
# same discipline applies here: count matches and refuse anything other
# than exactly 1, rather than silently guessing.
field() {
  count="$(printf '%s\n' "$json" | grep -c ""$1"" || true)"
  if [ "$count" -ne 1 ]; then
    echo "agy-build-args.sh: manifest has $count occurrences of "$1", expected exactly 1 — refusing to guess" >&2
    exit 1
  fi
  printf '%s\n' "$json" | sed -n 's/.*"'"$1"'"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p'
}

version="$(field version)"
url="$(field url)"
sha512="$(field sha512)"

if [ -z "$version" ] || [ -z "$url" ] || [ -z "$sha512" ]; then
  echo "agy-build-args.sh: manifest missing version/url/sha512 — got:" >&2
  printf '%s\n' "$json" >&2
  exit 1
fi

# Item 1: the sha512 comes from the same document as the URL, so this is
# defense-in-depth rather than a real integrity boundary (a compromised
# manifest could serve a matching pair over either scheme) — but it costs
# one line to refuse a plaintext download outright rather than silently
# downgrading a value that is about to be handed straight to wget/curl.
case "$url" in
  https://*) ;;
  *) echo "agy-build-args.sh: manifest URL is not https:// — refusing: $url" >&2; exit 1 ;;
esac

echo "agy-build-args.sh: resolved agy $version from $MANIFEST_URL" >&2

printf -- '--build-arg AGY_URL=%s --build-arg AGY_SHA512=%s --build-arg AGY_VERSION=%s\n' \
  "$url" "$sha512" "$version"
