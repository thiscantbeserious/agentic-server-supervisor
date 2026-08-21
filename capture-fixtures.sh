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
    -e 's/\b([0-9]{1,3}\.){3}[0-9]{1,3}\b/10.0.0.1/g' \
    -e 's/"?[Ss]erial[_ ]?[Nn]umber"?([": ]+)"?[A-Za-z0-9._-]+/serial_number\1"SERIAL"/g' \
    -e 's/\b(wwn-|nvme-eui\.|eui\.)[0-9a-fA-F]{8,}/\1WWN/g' \
    -e 's/\b[0-9a-fA-F]{16,}\b/GUID/g'
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
echo "$PROG: done. Review before sharing:"
echo "  grep -rniE 'serial|wwn|$HOST|[0-9]+\.[0-9]+\.[0-9]+\.[0-9]+' $OUT"
echo "  tar -czf ${OUT}.tar.gz -C $(dirname "$OUT") $(basename "$OUT")"
