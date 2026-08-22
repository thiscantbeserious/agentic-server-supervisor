#!/usr/bin/env bash
# Capture real host output as test fixtures.
#
# Run this on the host after install.sh has completed, when lm-sensors,
# rasdaemon and smartd are present and configured. The fixtures the test
# suite ships today were written from an idea of what these commands emit;
# these are what they actually emit.
#
#   curl -fsSL https://raw.githubusercontent.com/thiscantbeserious/agentic-server-supervisor/main/capture-fixtures.sh | sudo bash
#
# Read-only: every command below reads. Nothing is installed, started,
# stopped or written outside the output directory.
#
# Output is scrubbed before it is written. Disk serials, WWNs, pool GUIDs,
# the hostname and any IP address are replaced with stable placeholders, so
# the result can go into a public repository. The scrubbing is deliberate
# rather than hopeful: read what it produces before committing it.
set -u
set -o pipefail

PROG="capture-fixtures.sh"
OUT="${1:-/tmp/sentinel-fixtures}"
HOST="$(hostname 2>/dev/null || echo host)"

if [ "${EUID:-$(id -u)}" -ne 0 ]; then
  echo "$PROG: must run as root: smartctl and the journal need it" >&2
  exit 77
fi

mkdir -p "$OUT" || exit 75

# scrub STDIN: identifiers that describe this machine rather than its behaviour.
scrub() {
  sed -E \
    -e "s/\b${HOST}\b/HOSTNAME/g" \
    `# Octets validated, and the match must not sit inside a longer` \
    `# dotted run. The permissive form matched "1.000.204.886" inside a` \
    `# German-formatted byte count and turned a disk capacity into` \
    `# "10.0.0.1.016 bytes", a fixture describing hardware that cannot` \
    `# exist. Scrubbing that corrupts is worse than scrubbing that` \
    `# misses: a leak is caught by review, a corrupted fixture is not.` \
    -e 's/(^|[^0-9.])((25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])\.){3}(25[0-5]|2[0-4][0-9]|1[0-9]{2}|[1-9]?[0-9])($|[^0-9.])/\110.0.0.1\5/g' \
    -e 's/"?[Ss]erial[_ ]?[Nn]umber"?([": ]+)"?[A-Za-z0-9._-]+/serial_number\1"SERIAL"/g' \
    `# smartd writes serials as "S/N:VALUE" and WWNs as "WWN:5-001b44-...",` \
    `# neither of which the rules above see. Both appear in every journal` \
    `# line smartd emits about a disk, so a capture from a host with SMART` \
    `# monitoring on leaks every drive serial it has.` \
    -e 's|S/N:[A-Za-z0-9._-]+|S/N:SERIAL|g' \
    -e 's/WWN:[0-9a-fA-F-]+/WWN:WWN/g' \
    -e 's/(LU WWN Device Id:)[0-9a-fA-F ]+/\1 WWN/g' \
    -e 's/\b(wwn-|nvme-eui\.|eui\.)[0-9a-fA-F]{8,}/\1WWN/g' \
    `# by-id paths carry the model AND the serial, joined by the last` \
    `# underscore. The model is worth keeping, it is what the parsers key` \
    `# on; the serial is not.` \
    -e 's|(/dev/disk/by-id/[a-z]+-[A-Za-z0-9._-]*)_[A-Za-z0-9]+|\1_SERIAL|g' \
    `# Exactly 32 hex, which is what systemd machine, boot and invocation` \
    `# ids are. The old {16,} form also matched 16-digit journal` \
    `# timestamps, replacing every __REALTIME_TIMESTAMP with "GUID" and` \
    `# destroying the one field the journal parsing tests need.` \
    -e 's/\b[0-9a-f]{32}\b/GUID/g'
}

# verify OUT, refuses to call a capture finished while it still carries
# identifiers. The previous version printed a grep for the operator to run
# and called that a review, which put the burden of catching a scrubbing
# bug on the person least able to know what the scrubber intended. These
# patterns are what leaked from a real host, not what seemed plausible.
verify() {
  local dir="$1" bad=0 hits
  local -a checks=(
    'S/N:[A-Za-z0-9._-]+'
    'WWN:[0-9a-fA-F]{2,}'
    'LU WWN Device Id: *[0-9a-fA-F]{2,}'
    '/dev/disk/by-id/[a-z]+-[A-Za-z0-9._-]+_[A-Za-z0-9]{4,}(\b[^_]|$)'
  )
  for pat in "${checks[@]}"; do
    hits="$(grep -rEl "$pat" "$dir" 2>/dev/null | grep -v 'SERIAL\|WWN' || true)"
    if grep -rE "$pat" "$dir" 2>/dev/null | grep -qvE 'SERIAL|WWN:WWN'; then
      echo "$PROG: SCRUB FAILED, identifiers still present for /$pat/:" >&2
      grep -rEo "$pat" "$dir" 2>/dev/null | grep -vE 'SERIAL|WWN:WWN' | sort -u | head -5 >&2
      bad=1
    fi
  done
  if [ -n "$HOST" ] && grep -rqiE "\b${HOST}\b" "$dir" 2>/dev/null; then
    echo "$PROG: SCRUB FAILED, the hostname is still present" >&2
    bad=1
  fi
  return "$bad"
}

# cap FILE COMMAND... : record what produced the file next to the file itself.
cap() {
  local name="$1"; shift
  { printf '# produced by: %s\n' "$*"; "$@" 2>&1 | scrub; } > "${OUT}/${name}" \
    && printf '  %-34s %s\n' "$name" "ok" \
    || printf '  %-34s %s\n' "$name" "FAILED (kept, read it)"
}

echo "$PROG: writing to $OUT"

cap sensors.json            sensors -j
cap smartctl-scan.txt       smartctl --scan
first_disk="$(smartctl --scan 2>/dev/null | awk 'NR==1{print $1}')"
[ -n "$first_disk" ] && cap smartctl-device.txt smartctl -a "$first_disk"
cap smartd-journal.jsonl    journalctl -t smartd -o json --no-pager -n 200
cap zed-journal.jsonl       journalctl -t zed -o json --no-pager -n 200
cap kernel-journal.jsonl    journalctl -k -p 3 -o json --no-pager -n 200
cap rasdaemon-errors.txt    ras-mc-ctl --errors
cap zpool-status.txt        zpool status
cap compose-projects.json   docker compose ls --all --format json
# shellcheck disable=SC2016  # the single quotes are the point: $d expands
# inside the sh -c subshell, not here.
cap hwmon-names.txt         sh -c 'for d in /sys/class/hwmon/hwmon*; do [ -r "$d/name" ] && echo "$(basename "$d")=$(cat "$d/name")"; done'

echo
if verify "$OUT"; then
  echo "$PROG: done, and the scrub check found no identifiers. Still worth a look:"
  echo "  grep -rniE 'serial|wwn|$HOST|[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' $OUT"
  echo "  tar -czf ${OUT}.tar.gz -C $(dirname "$OUT") $(basename "$OUT")"
else
  echo "$PROG: the capture is in $OUT and MUST NOT be shared as it stands." >&2
  echo "$PROG: the scrubbing missed something, see the lines above." >&2
  exit 1
fi
