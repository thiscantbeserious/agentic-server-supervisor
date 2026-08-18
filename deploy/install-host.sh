#!/bin/bash
# deploy/install-host.sh — R5. The one deliberate bash artifact in this
# repo: it runs on the host as root before the sentinel image exists,
# needs apt-get and systemctl, and shipping a second Go binary to bam just
# to write three config files is more moving parts than a ~150-line
# idempotent script. Upgrade path: a "sentinel install-host" subcommand if
# the host part ever grows.
#
# ponytail: the one deliberate bash artifact. It runs on the host as root
# before the image exists, needs apt-get and systemctl, and shipping a
# second Go binary to bam to write three config files is more moving parts
# than a 120-line idempotent script. Upgrade path: a "sentinel
# install-host" subcommand if the host part ever grows.
#
# The binding spec is contracts/runtime.md R5.
set -u
set -o pipefail

PROG="install-host.sh"
MARK_BEGIN="# >>> agentic-server-supervisor (managed) >>>"
MARK_END="# <<< agentic-server-supervisor (managed) <<<"

CHECK=0
DRY_RUN=0
MAILRISE_HOST="127.0.0.1"
MAILRISE_PORT="8025"
ENV_FILE="./.env"

usage() {
  cat <<EOF
usage: $PROG [--check] [--dry-run] [--mailrise-host HOST] [--mailrise-port PORT] [--env-file PATH] [-h|--help]

  --check                report drift, change nothing; exit 0 if converged, 1 if not
  --dry-run               print every action it would take, change nothing, exit 0
  --mailrise-host HOST    SMTP host smartd/ZED mail is delivered to (default 127.0.0.1)
  --mailrise-port PORT    SMTP port (default 8025)
  --env-file PATH         file that receives JOURNAL_GID= (default ./.env)
  -h, --help              show this help
EOF
}

changed=0
report_lines=()

note() { report_lines+=("$1"); }

while [ $# -gt 0 ]; do
  case "$1" in
    --check) CHECK=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --mailrise-host) MAILRISE_HOST="${2:-}"; shift 2 ;;
    --mailrise-port) MAILRISE_PORT="${2:-}"; shift 2 ;;
    --env-file) ENV_FILE="${2:-}"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "$PROG: unknown argument: $1" >&2; usage >&2; exit 64 ;;
  esac
done

if [ "$EUID" -ne 0 ]; then
  echo "$PROG: must run as root (EUID=0)" >&2
  exit 77
fi

if ! command -v apt-get >/dev/null 2>&1 || ! command -v systemctl >/dev/null 2>&1; then
  echo "$PROG: requires Debian (apt-get) and systemd (systemctl)" >&2
  exit 69
fi

TRANSIENT_FAIL=0

# --- helpers -----------------------------------------------------------

# render_managed_block FILE PREAMBLE BODY MODE
# Ensures FILE contains exactly one managed block (marker-delimited),
# content outside the markers untouched, atomic replace via mktemp +
# install. Returns 0 if the file changed, 1 if it was already converged.
# In --check/--dry-run mode, only reports/prints; never writes.
render_managed_block() {
  file="$1"; preamble="$2"; body="$3"; mode="$4"

  desired_block="$MARK_BEGIN
${preamble}${body}
$MARK_END"

  existing=""
  if [ -f "$file" ]; then
    existing="$(sed -n "/^${MARK_BEGIN}\$/,/^${MARK_END}\$/p" "$file")"
  fi

  if [ "$existing" = "$desired_block" ] && [ -f "$file" ]; then
    return 1
  fi

  if [ "$CHECK" -eq 1 ]; then
    return 0
  fi

  if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] would write managed block to $file"
    return 0
  fi

  tmp="$(mktemp)"
  if [ -f "$file" ]; then
    # First modification (ever): keep a .bak-<epoch> copy.
    existing_bak="$(ls "${file}".bak-* 2>/dev/null | head -n1)"
    if [ -z "$existing_bak" ]; then
      cp -p "$file" "${file}.bak-$(date +%s)" 2>/dev/null || true
    fi
    if grep -q "^${MARK_BEGIN}\$" "$file" 2>/dev/null; then
      # Replace the existing block in place, preserving surrounding content.
      awk -v b="$MARK_BEGIN" -v e="$MARK_END" -v repl="$desired_block" '
        $0==b {print repl; skip=1; next}
        $0==e {if (skip) {skip=0; next}}
        skip {next}
        {print}
      ' "$file" > "$tmp"
    else
      cat "$file" > "$tmp"
      printf '\n%s\n' "$desired_block" >> "$tmp"
    fi
  else
    printf '%s\n' "$desired_block" > "$tmp"
  fi

  install -m "$mode" -o root -g root "$tmp" "$file"
  rm -f "$tmp"
  return 0
}

pkg_installed() {
  dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q "install ok installed"
}

# --- step 1: packages ----------------------------------------------------

step1() {
  need=()
  for p in rasdaemon lm-sensors msmtp msmtp-mta; do
    pkg_installed "$p" || need+=("$p")
  done
  if [ "${#need[@]}" -eq 0 ]; then
    note "step1 packages: already installed (rasdaemon lm-sensors msmtp msmtp-mta)"
    return
  fi
  note "step1 packages: installing ${need[*]}"
  if [ "$CHECK" -eq 1 ]; then changed=$((changed+1)); return; fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] would run: apt-get install -y --no-install-recommends ${need[*]}"
    changed=$((changed+1))
    return
  fi
  if ! apt-get install -y --no-install-recommends "${need[@]}"; then
    echo "$PROG: apt-get install failed" >&2
    TRANSIENT_FAIL=1
    return
  fi
  changed=$((changed+1))
}

# --- step 2: rasdaemon service -------------------------------------------

step2() {
  enabled=0; active=0
  systemctl is-enabled --quiet rasdaemon 2>/dev/null && enabled=1
  systemctl is-active --quiet rasdaemon 2>/dev/null && active=1
  if [ "$enabled" -eq 1 ] && [ "$active" -eq 1 ]; then
    note "step2 rasdaemon: already enabled and active"
    return
  fi
  note "step2 rasdaemon: enabling and starting"
  if [ "$CHECK" -eq 1 ]; then changed=$((changed+1)); return; fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] would run: systemctl enable --now rasdaemon"
    changed=$((changed+1))
    return
  fi
  if ! systemctl enable --now rasdaemon; then
    echo "$PROG: systemctl enable --now rasdaemon failed" >&2
    TRANSIENT_FAIL=1
    return
  fi
  changed=$((changed+1))
}

# --- step 3: /etc/msmtprc -------------------------------------------------

step3() {
  file="/etc/msmtprc"
  auth_line="auth off"
  if [ -f "$ENV_FILE" ] && grep -q "^MAILRISE_SMTP_USER=" "$ENV_FILE" 2>/dev/null \
     && grep -q "^MAILRISE_SMTP_PASS=" "$ENV_FILE" 2>/dev/null; then
    auth_line="auth on"
  fi
  hn="$(hostname -f 2>/dev/null || hostname)"
  body="account sentinel
host ${MAILRISE_HOST}
port ${MAILRISE_PORT}
${auth_line}
from sentinel@${hn}
account default : sentinel"

  if render_managed_block "$file" "" "$body" 0600; then
    note "step3 /etc/msmtprc: updated"
    changed=$((changed+1))
  else
    note "step3 /etc/msmtprc: already converged"
  fi
}

# --- step 4: smartd ------------------------------------------------------

SMARTD_LINE='DEVICESCAN -a -o on -S on -n standby,q -W 4,45,55 -m smartd@mailrise.xyz -M exec /usr/share/smartmontools/smartd-runner'

step4() {
  file="/etc/smartd.conf"
  preamble=""
  # A pre-existing unmanaged -m line is disabled in place, inside the
  # managed block's preamble, and reported (R5 idempotency contract).
  if [ -f "$file" ]; then
    # Scan only the content OUTSIDE the managed block — the managed
    # DEVICESCAN line itself legitimately contains "-m ", and scanning the
    # whole file (minus just the two marker lines) would re-detect our own
    # output as "pre-existing unmanaged" on every run, defeating
    # idempotency (caught by the C12 two-consecutive-real-runs check).
    outside_block="$(awk -v b="$MARK_BEGIN" -v e="$MARK_END" '
      $0==b {skip=1; next}
      $0==e {skip=0; next}
      skip {next}
      {print}
    ' "$file")"
    existing_m="$(printf '%s\n' "$outside_block" | grep -E "^[^#].*-m " || true)"
    if [ -n "$existing_m" ]; then
      preamble="# disabled by agentic-server-supervisor
"
      while IFS= read -r l; do
        [ -n "$l" ] && preamble="${preamble}# ${l}
"
      done <<EOF2
$existing_m
EOF2
      note "step4 smartd: pre-existing -m line found, commented in managed block preamble"
    fi
  fi

  before_hash=""
  [ -f "$file" ] && before_hash="$(sha256sum "$file" 2>/dev/null | awk '{print $1}')"

  if render_managed_block "$file" "$preamble" "$SMARTD_LINE" 0644; then
    note "step4 /etc/smartd.conf: updated"
    changed=$((changed+1))
    if [ "$CHECK" -ne 1 ] && [ "$DRY_RUN" -ne 1 ]; then
      after_hash="$(sha256sum "$file" 2>/dev/null | awk '{print $1}')"
      if [ "$before_hash" != "$after_hash" ]; then
        systemctl restart smartd 2>/dev/null || {
          echo "$PROG: systemctl restart smartd failed" >&2
          TRANSIENT_FAIL=1
        }
      fi
    fi
  else
    note "step4 /etc/smartd.conf: already converged"
  fi
}

# --- step 5: ZED --------------------------------------------------------

step5() {
  dir="/etc/zfs/zed.d"
  file="${dir}/zed.rc"
  if [ ! -d "$dir" ]; then
    echo "$PROG: WARN: $dir does not exist — skipping ZED configuration" >&2
    note "step5 ZED: skipped (no /etc/zfs/zed.d)"
    return
  fi

  body='ZED_EMAIL_ADDR="zed@mailrise.xyz"
ZED_EMAIL_PROG="msmtp"
ZED_NOTIFY_VERBOSE=1'

  before_hash=""
  [ -f "$file" ] && before_hash="$(sha256sum "$file" 2>/dev/null | awk '{print $1}')"

  if render_managed_block "$file" "" "$body" 0644; then
    note "step5 $file: updated"
    changed=$((changed+1))
    if [ "$CHECK" -ne 1 ] && [ "$DRY_RUN" -ne 1 ]; then
      after_hash="$(sha256sum "$file" 2>/dev/null | awk '{print $1}')"
      if [ "$before_hash" != "$after_hash" ]; then
        systemctl restart zfs-zed 2>/dev/null || {
          echo "$PROG: systemctl restart zfs-zed failed" >&2
          TRANSIENT_FAIL=1
        }
      fi
    fi
  else
    note "step5 $file: already converged"
  fi
}

# --- step 6: JOURNAL_GID -------------------------------------------------

step6() {
  gid="$(getent group systemd-journal | cut -d: -f3)"
  if [ -z "$gid" ]; then
    echo "$PROG: systemd-journal group not found" >&2
    exit 70
  fi

  desired="JOURNAL_GID=${gid}"
  current=""
  if [ -f "$ENV_FILE" ]; then
    current="$(grep "^JOURNAL_GID=" "$ENV_FILE" 2>/dev/null | tail -n1)"
  fi

  if [ "$current" = "$desired" ]; then
    note "step6 JOURNAL_GID: already ${gid} in $ENV_FILE"
    return
  fi

  note "step6 JOURNAL_GID: setting to ${gid} in $ENV_FILE"
  if [ "$CHECK" -eq 1 ]; then changed=$((changed+1)); return; fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] would upsert ${desired} in $ENV_FILE"
    changed=$((changed+1))
    return
  fi

  tmp="$(mktemp)"
  if [ -f "$ENV_FILE" ]; then
    if grep -q "^JOURNAL_GID=" "$ENV_FILE" 2>/dev/null; then
      sed "s/^JOURNAL_GID=.*/${desired}/" "$ENV_FILE" > "$tmp"
    else
      cp "$ENV_FILE" "$tmp"
      printf '%s\n' "$desired" >> "$tmp"
    fi
  else
    printf '%s\n' "$desired" > "$tmp"
  fi
  install -m 0600 "$tmp" "$ENV_FILE"
  rm -f "$tmp"
  changed=$((changed+1))
}

# --- run -------------------------------------------------------------

step1
step2
step3
step4
step5
step6

echo "$PROG summary:"
for l in "${report_lines[@]}"; do
  echo "  - $l"
done
echo "changed=${changed}"

if [ "$TRANSIENT_FAIL" -eq 1 ]; then
  exit 75
fi

if [ "$CHECK" -eq 1 ]; then
  if [ "$changed" -eq 0 ]; then
    exit 0
  fi
  exit 1
fi

exit 0
