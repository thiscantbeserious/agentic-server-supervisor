#!/bin/sh
# Loads deploy/agy-pin.env into $GITHUB_ENV for a CI step. $GITHUB_ENV's
# file-command format accepts only bare KEY=value lines, GitHub's runner
# fails the step outright on anything else (the comments this file exists
# to carry included), so `cat deploy/agy-pin.env >> "$GITHUB_ENV"` cannot
# be used directly. This filters comments and blank lines, but a
# malformed assignment still fails the build loudly rather than quietly
# exporting nothing: a typo here must not surface many steps later as a
# confusing download error against an empty AGY_URL_AMD64, the same
# silent-degradation class this project refuses everywhere else.
#
# Run standalone (GITHUB_ENV=/some/file deploy/agy-pin-load.sh) to check
# deploy/agy-pin.env locally without a runner; nothing here is CI-only.
set -eu

script_dir="$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)"
pin_file="$script_dir/agy-pin.env"
expected="AGY_VERSION AGY_URL_AMD64 AGY_SHA512_AMD64 AGY_URL_ARM64 AGY_SHA512_ARM64"
seen=""

fail() {
  echo "agy-pin-load.sh: $1" >&2
  exit 1
}

while IFS= read -r line || [ -n "$line" ]; do
  # Trailing whitespace (including a stray \r from a CRLF checkout) is
  # tolerated; leading whitespace is not, deploy/agy-pin.env is flush-left.
  line="$(printf '%s' "$line" | sed 's/[[:space:]]*$//')"
  case "$line" in
    ''|'#'*) continue ;;
  esac
  case "$line" in
    [A-Za-z_]*=*) ;;
    *) fail "malformed line in $pin_file, expected KEY=value: $line" ;;
  esac
  key="${line%%=*}"
  value="${line#*=}"
  case "$key" in
    *[!A-Za-z0-9_]*) fail "malformed variable name in $pin_file: $key" ;;
  esac
  [ -n "$value" ] || fail "$key in $pin_file has an empty value"
  case " $expected " in
    *" $key "*) ;;
    *) fail "unexpected variable $key in $pin_file, expected one of: $expected" ;;
  esac
  seen="$seen $key"
  echo "$line" >> "$GITHUB_ENV"
done < "$pin_file"

for key in $expected; do
  case " $seen " in
    *" $key "*) ;;
    *) fail "$pin_file is missing $key" ;;
  esac
done
