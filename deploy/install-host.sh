#!/bin/bash
# deploy/install-host.sh — R5. The one deliberate bash artifact in this
# repo: it runs on the host as root before the sentinel image exists,
# needs apt-get and systemctl, and shipping a second Go binary to bam
# just to write a handful of config files is more moving parts than an
# idempotent script. Upgrade path: a "sentinel install-host" subcommand
# if the host part ever grows.
#
# ponytail: kept as a single bash script rather than promoted to a Go
# subcommand for that reason — the ceiling is "the host part grows past
# what a shell script can do cleanly," not a line count.
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

require_value() {
  # A flag with no following argument must fail, not silently consume
  # nothing: without this, `shift 2` on a 1-element remainder shifts 0
  # (bash) and re-enters the same case branch on the same $1 forever.
  if [ $# -lt 2 ]; then
    echo "$PROG: $1 requires a value" >&2
    usage >&2
    exit 64
  fi
}

while [ $# -gt 0 ]; do
  case "$1" in
    --check) CHECK=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --mailrise-host) require_value "$@"; MAILRISE_HOST="$2"; shift 2 ;;
    --mailrise-port) require_value "$@"; MAILRISE_PORT="$2"; shift 2 ;;
    --env-file) require_value "$@"; ENV_FILE="$2"; shift 2 ;;
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

# Whether the package each later step depends on is actually present.
# Set by step1 after it knows the real outcome (already-installed,
# freshly installed, or failed) — never assumed from step1's own exit
# status alone, since apt-get install with multiple packages does not
# guarantee all-or-nothing. A step that configures a service or a mail
# target for a package that isn't there is the half-configured host this
# script exists to prevent, so each dependent step checks this instead
# of running blind after a package failure.
RASDAEMON_OK=0
MSMTP_OK=0

# MAIL_OK means mail will actually work: the msmtp package is present
# AND step3 wrote a credentialed /etc/msmtprc. Package presence alone is
# not enough — smartd's -m target and ZED_EMAIL_PROG both hand mail to
# msmtp regardless of whether msmtp has anything to send with, so a step
# that only checked MSMTP_OK would point live alert paths at an msmtp
# with no config file at all whenever credentials are the missing piece
# rather than the package. Steps 3, 4 and 5 all gate on this ONE flag —
# not three separate checks that could drift apart from each other
# again — because the property that matters is identical for all three:
# a step whose output cannot work must not run. Computed once, right
# after step1, by compute_mail_status.
MAIL_OK=0
MAIL_NOT_OK_REASON=""
SMTP_USER=""
SMTP_PASS=""
# Set only for the credentials-missing case specifically (never for a
# missing package, which is TRANSIENT_FAIL/75 — retryable by installing
# the package). Missing credentials are permanent until a human edits
# --env-file, so R5 (contract) now assigns this its own exit 78, the
# same code C2 uses for "required ops input missing" on the sentinel
# binary — 75 would tell rollout automation to retry a condition that
# never resolves without that edit.
MISSING_ENV_INPUT=0

# compute_mail_status reads --env-file ONCE, right after step1 (so
# MSMTP_OK already reflects package reality), and sets MAIL_OK plus the
# reason steps 3-5 report when it is not 1. SMTP_USER/SMTP_PASS are
# cached here so step3 does not re-parse the same file.
compute_mail_status() {
  SMTP_USER=""
  SMTP_PASS=""
  if [ -f "$ENV_FILE" ]; then
    SMTP_USER="$(strip_quotes "$(grep "^MAILRISE_SMTP_USER=" "$ENV_FILE" 2>/dev/null | tail -n1 | cut -d= -f2-)")"
    SMTP_PASS="$(strip_quotes "$(grep "^MAILRISE_SMTP_PASS=" "$ENV_FILE" 2>/dev/null | tail -n1 | cut -d= -f2-)")"
  fi
  if [ "$MSMTP_OK" -ne 1 ]; then
    MAIL_OK=0
    MAIL_NOT_OK_REASON="msmtp package not installed"
    return
  fi
  if [ -z "$SMTP_USER" ] || [ -z "$SMTP_PASS" ]; then
    # mailrise enforces SMTP AUTH unconditionally (R4 requires both
    # MAILRISE_SMTP_USER and MAILRISE_SMTP_PASS with `:?`), so a real
    # host always has them. An operator who has not filled .env in yet
    # needs telling, not a config that writes "auth off" and looks
    # converged while every mail send fails silently.
    MAIL_OK=0
    MAIL_NOT_OK_REASON="MAILRISE_SMTP_USER/MAILRISE_SMTP_PASS missing or empty in $ENV_FILE"
    echo "$PROG: $MAIL_NOT_OK_REASON — mail delivery (msmtprc, smartd -m, ZED) will not be configured" >&2
    if [ "$CHECK" -ne 1 ] && [ "$DRY_RUN" -ne 1 ]; then
      MISSING_ENV_INPUT=1
    fi
    return
  fi
  MAIL_OK=1
}

# --- helpers -----------------------------------------------------------

# render_managed_block FILE PREAMBLE BODY MODE
# Ensures FILE contains exactly one managed block (marker-delimited),
# content outside the markers untouched, atomic replace via mktemp +
# install. Returns 0 if the file changed, 1 if it was already converged.
# In --check/--dry-run mode, only reports/prints; never writes.
#
# Sets RENDER_COLLAPSED to the number of managed blocks found when more
# than one existed (0 otherwise), so callers can report a collapse
# distinctly from an ordinary content update. Everything between our own
# markers is OUR content, never the operator's — unlike the pre-existing
# smartd -m line, which had to survive as a comment because it belonged
# to them — so a second block found alongside the first is our own mess
# (a half-finished run, a restored backup, a merge) and collapsing it
# destroys nothing an operator wrote. A host that cannot resolve this
# without a human is worse than one that fixes it and says so.
render_managed_block() {
  file="$1"; preamble="$2"; body="$3"; mode="$4"
  RENDER_COLLAPSED=0

  desired_block="$MARK_BEGIN
${preamble}${body}
$MARK_END"

  existing=""
  block_count=0
  if [ -f "$file" ]; then
    existing="$(sed -n "/^${MARK_BEGIN}\$/,/^${MARK_END}\$/p" "$file")"
    block_count="$(grep -c "^${MARK_BEGIN}\$" "$file" 2>/dev/null || true)"
    [ -z "$block_count" ] && block_count=0
  fi

  if [ "$existing" = "$desired_block" ] && [ -f "$file" ] && [ "$block_count" -le 1 ]; then
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
    if [ "$block_count" -ge 1 ]; then
      # Emit the desired block once, at the FIRST begin marker; every
      # later marker pair (and its content) is dropped entirely rather
      # than re-emitted — collapsing N blocks into 1, not just refreshing
      # the content of each.
      if [ "$block_count" -gt 1 ]; then
        RENDER_COLLAPSED="$block_count"
      fi
      awk -v b="$MARK_BEGIN" -v e="$MARK_END" -v repl="$desired_block" '
        $0==b {if (!seen) {print repl; seen=1} skip=1; next}
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

  if ! install -m "$mode" -o root -g root "$tmp" "$file"; then
    # An unchecked `install` (read-only /etc, full disk) would report
    # success — "updated" and changed+1 — while writing nothing. Fail
    # loud instead: TRANSIENT_FAIL makes the whole run exit 75 (safe to
    # re-run), and returning 1 here means the caller's "already
    # converged" branch runs rather than "updated" — imperfect wording
    # for this one case, but it never claims a write that did not
    # happen, and the exit code is the honest signal an operator or a
    # script checking $? actually reads.
    echo "$PROG: failed to install $file" >&2
    TRANSIENT_FAIL=1
    rm -f "$tmp"
    return 1
  fi
  rm -f "$tmp"
  return 0
}

pkg_installed() {
  dpkg-query -W -f='${Status}' "$1" 2>/dev/null | grep -q "install ok installed"
}

# strip_quotes removes ONE matched pair of surrounding single or double
# quotes from an env-file value. Quoting a value
# (MAILRISE_SMTP_PASS="secret") is ordinary env-file style, and
# .env.example being unquoted does not mean an operator's real .env is —
# unstripped, msmtp receives the quote characters as part of the
# password and authentication fails against the real value while a
# string check on the config file would still look fine.
strip_quotes() {
  local v="$1"
  if [ "${#v}" -ge 2 ]; then
    if [[ "$v" == \"*\" && "$v" == *\" ]]; then
      v="${v:1:-1}"
    elif [[ "$v" == \'*\' && "$v" == *\' ]]; then
      v="${v:1:-1}"
    fi
  fi
  printf '%s' "$v"
}

# --- step 1: packages ----------------------------------------------------

step1() {
  need=()
  for p in rasdaemon lm-sensors msmtp msmtp-mta; do
    pkg_installed "$p" || need+=("$p")
  done
  if [ "${#need[@]}" -eq 0 ]; then
    note "step1 packages: already installed (rasdaemon lm-sensors msmtp msmtp-mta)"
    RASDAEMON_OK=1
    MSMTP_OK=1
    return
  fi
  note "step1 packages: installing ${need[*]}"
  if [ "$CHECK" -eq 1 ]; then
    changed=$((changed+1))
    RASDAEMON_OK=1
    MSMTP_OK=1
    return
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] would run: apt-get install -y --no-install-recommends ${need[*]}"
    changed=$((changed+1))
    RASDAEMON_OK=1
    MSMTP_OK=1
    return
  fi
  # A host with stale apt lists fails with "Unable to locate package"
  # here, surfacing as exit 75 "safe to re-run" when re-running alone
  # does not fix it. update is idempotent and cheap; run it right
  # before the one step that needs current lists rather than assuming
  # they're fresh.
  apt-get update -qq || true
  if ! apt-get install -y --no-install-recommends "${need[@]}"; then
    echo "$PROG: apt-get install failed" >&2
    TRANSIENT_FAIL=1
  else
    changed=$((changed+1))
  fi
  # Checked directly rather than inferred from apt-get's own exit status:
  # a multi-package `apt-get install` is not guaranteed all-or-nothing, so
  # the steps below skip only the ones whose actual dependency is still
  # missing, not every package named in this run.
  pkg_installed rasdaemon && RASDAEMON_OK=1
  { pkg_installed msmtp || pkg_installed msmtp-mta; } && MSMTP_OK=1
  if [ "$RASDAEMON_OK" -ne 1 ]; then
    note "step1 packages: rasdaemon not installed — step2 will be skipped"
  fi
  if [ "$MSMTP_OK" -ne 1 ]; then
    note "step1 packages: msmtp not installed — steps 3-5's mail delivery will be skipped"
  fi
}

# --- step 2: rasdaemon service -------------------------------------------

step2() {
  if [ "$RASDAEMON_OK" -ne 1 ]; then
    note "step2 rasdaemon: skipped (rasdaemon package not installed)"
    return
  fi
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
  if [ "$MAIL_OK" -ne 1 ]; then
    note "step3 $file: skipped ($MAIL_NOT_OK_REASON)"
    return
  fi
  smtp_user="$SMTP_USER"
  smtp_pass="$SMTP_PASS"
  # mailrise enforces SMTP AUTH unconditionally (R4) and runs
  # tls: off on the LAN (internal/notify/smtpfallback.go's own comment
  # and its plainAuthNoTLS type document the same fact for sentinel's
  # own SMTP client). "auth on" tells msmtp to auto-pick the "safest"
  # method, and msmtp refuses PLAIN/LOGIN — the only methods a plain
  # mailrise listener offers — over a non-TLS connection even with
  # "tls off" set explicitly: verified against real msmtp 1.8.28 on
  # Debian 13 against a stub advertising "AUTH PLAIN LOGIN", "auth on"
  # + user/password + tls off still exits 69 "cannot use a secure
  # authentication method". Forcing the method explicitly ("auth
  # plain") is what bypasses that guard — the exact same fix
  # smtpfallback.go already applies on the Go side for the identical
  # reason, so this keeps both SMTP clients doing the same thing
  # against the same server.
  auth_line="auth plain
tls off"
  cred_lines="user ${smtp_user}
password ${smtp_pass}
"
  hn="$(hostname -f 2>/dev/null || hostname)"
  body="account sentinel
host ${MAILRISE_HOST}
port ${MAILRISE_PORT}
${auth_line}
${cred_lines}from sentinel@${hn}
account default : sentinel"

  if render_managed_block "$file" "" "$body" 0600; then
    if [ "$RENDER_COLLAPSED" -gt 0 ]; then
      note "step3 /etc/msmtprc: collapsed ${RENDER_COLLAPSED} managed blocks into 1"
    else
      note "step3 /etc/msmtprc: updated"
    fi
    changed=$((changed+1))
  else
    note "step3 /etc/msmtprc: already converged"
  fi
}

# --- step 4: smartd ------------------------------------------------------

SMARTD_LINE='DEVICESCAN -a -o on -S on -n standby,q -W 4,45,55 -m smartd@mailrise.xyz -M exec /usr/share/smartmontools/smartd-runner'

DISABLED_MARK="# disabled by agentic-server-supervisor: "

step4() {
  if [ "$MAIL_OK" -ne 1 ]; then
    note "step4 /etc/smartd.conf: skipped ($MAIL_NOT_OK_REASON — the DEVICESCAN mail target it configures cannot deliver)"
    return
  fi
  file="/etc/smartd.conf"
  preamble=""
  # Captured BEFORE any edit in this function, including the in-place
  # -m comment below — not just before render_managed_block's own
  # rewrite. Capturing it after the -m edit would make that edit's own
  # change to the file invisible to the before/after diff
  # render_managed_block uses to decide whether to restart smartd and
  # count `changed`: if the managed block itself already matched
  # (nothing for render_managed_block to rewrite), the -m
  # comment-in-place edit would land on disk with smartd never
  # restarted and `changed` never incremented for it, leaving the
  # operator's old mail target live under a "converged" summary.
  before_hash=""
  [ -f "$file" ] && before_hash="$(sha256sum "$file" 2>/dev/null | awk '{print $1}')"
  # A pre-existing unmanaged -m line is commented out WHERE IT STANDS
  # (contracts/runtime.md R5) — not merely noted in the managed block's
  # preamble while staying live at the top of the file.
  # Two active -m targets is not just untidy: real smartd refuses to
  # start at all with two ("Unable to register device ... Exiting"),
  # which would take the whole SMART path down. Never deleted — the
  # .bak-<epoch> copy plus this in-place comment are how the operator
  # gets their original line back.
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

    if [ -n "$existing_m" ] && [ "$CHECK" -ne 1 ] && [ "$DRY_RUN" -ne 1 ]; then
      existing_bak="$(ls "${file}".bak-* 2>/dev/null | head -n1)"
      if [ -z "$existing_bak" ]; then
        cp -p "$file" "${file}.bak-$(date +%s)" 2>/dev/null || true
      fi
      tmp_m="$(mktemp)"
      # Scoped to content OUTSIDE the managed block, exactly like the
      # detection scan above. Without the b/e tracking here, this awk
      # matches EVERY "-m "-containing line in the whole file — including
      # the managed block's own live DEVICESCAN line, which is never
      # prefixed with "#" and matches the same pattern. When a managed
      # block already exists alongside a still-live unmanaged line, an
      # unscoped version of this awk comments out the real DEVICESCAN
      # line too, alongside the operator's old one — self-healing rather
      # than an outage, since the edit runs BEFORE render_managed_block,
      # which then reads the block back damaged, finds it no longer
      # matches desired_block, and rewrites it from scratch, restoring
      # the live line (confirmed by mutation: removing the scoping still
      # leaves DEVICESCAN count 1 after a run). Scoping it is still the
      # correct fix — it avoids a pointless rewrite-and-restart cycle on
      # every run that finds a pre-existing -m line — just not a fix for
      # silent data loss, which it never was.
      awk -v mark="$DISABLED_MARK" -v b="$MARK_BEGIN" -v e="$MARK_END" '
        $0==b {inblock=1}
        $0==e {inblock=0}
        (!inblock) && /^[^#].*-m / {print mark $0; next}
        {print}
      ' "$file" > "$tmp_m"
      if ! install -m 0644 -o root -g root "$tmp_m" "$file"; then
        # Same failure this function's own comment already documents for
        # render_managed_block's install call above: an unchecked
        # install (read-only /etc, full disk) would report the -m line
        # as commented out while nothing was actually written. Fail
        # loud instead — TRANSIENT_FAIL surfaces as exit 75, and the
        # note says what actually happened.
        echo "$PROG: failed to install $file (commenting out pre-existing -m line)" >&2
        TRANSIENT_FAIL=1
        rm -f "$tmp_m"
        note "step4 smartd: failed to comment out pre-existing -m line"
      else
        rm -f "$tmp_m"
        note "step4 smartd: pre-existing -m line found, commented out where it stands"
      fi
      # Re-read after the in-place edit — the preamble below is derived
      # from the PERSISTENT marker (next block), not from this pre-edit
      # scan, so the recorded fact survives every future run even after
      # the live "-m " line no longer exists to re-detect.
    elif [ -n "$existing_m" ]; then
      note "step4 smartd: pre-existing -m line found (--check/--dry-run: not commented)"
    fi

    # The preamble is derived from the PERSISTENT in-place marker, not
    # from a live re-scan for an uncommented "-m " line — once step4 has
    # commented the original out, that live scan finds nothing on every
    # later run, and re-deriving the preamble from it would silently drop
    # the recorded fact (and un-converge the managed block) on run 2.
    # Scanning for our own marker instead makes the record — and the
    # block's content — stable forever after the one-time transition.
    outside_after="$(awk -v b="$MARK_BEGIN" -v e="$MARK_END" '
      $0==b {skip=1; next}
      $0==e {skip=0; next}
      skip {next}
      {print}
    ' "$file" 2>/dev/null)"
    disabled_lines="$(printf '%s\n' "$outside_after" | grep -F "$DISABLED_MARK" || true)"
    if [ -n "$disabled_lines" ]; then
      preamble="# disabled by agentic-server-supervisor (pre-existing config, commented in place — see backup)
"
    fi
  fi

  did_update=0
  if render_managed_block "$file" "$preamble" "$SMARTD_LINE" 0644; then
    if [ "$RENDER_COLLAPSED" -gt 0 ]; then
      note "step4 /etc/smartd.conf: collapsed ${RENDER_COLLAPSED} managed blocks into 1"
    else
      note "step4 /etc/smartd.conf: updated"
    fi
    changed=$((changed+1))
    did_update=1
  else
    note "step4 /etc/smartd.conf: already converged"
  fi

  # This must fire even when render_managed_block itself found nothing
  # to rewrite (did_update=0) — the -m in-place comment edit earlier in
  # this function is a real change render_managed_block never sees, so
  # gating the restart+changed bookkeeping on render_managed_block's own
  # return value alone would miss exactly that case.
  if [ "$CHECK" -ne 1 ] && [ "$DRY_RUN" -ne 1 ]; then
    after_hash="$(sha256sum "$file" 2>/dev/null | awk '{print $1}')"
    if [ "$before_hash" != "$after_hash" ]; then
      if [ "$did_update" -eq 0 ]; then
        changed=$((changed+1))
      fi
      systemctl restart smartd 2>/dev/null || {
        echo "$PROG: systemctl restart smartd failed" >&2
        TRANSIENT_FAIL=1
      }
    fi
  fi
}

# --- step 5: ZED --------------------------------------------------------

step5() {
  if [ "$MAIL_OK" -ne 1 ]; then
    note "step5 ZED: skipped ($MAIL_NOT_OK_REASON — ZED_EMAIL_PROG=msmtp would be unusable)"
    return
  fi
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
    if [ "$RENDER_COLLAPSED" -gt 0 ]; then
      note "step5 $file: collapsed ${RENDER_COLLAPSED} managed blocks into 1"
    else
      note "step5 $file: updated"
    fi
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
      # A .env with no trailing newline (common from a hand-edited or
      # backup-restored file) means this append lands on the SAME line
      # as the previous value with no separator — verified:
      # "MAILRISE_SMTP_PASS=secret" + append becomes
      # "MAILRISE_SMTP_PASS=secretJOURNAL_GID=7777", destroying both
      # values.
      if [ -n "$(tail -c1 "$tmp")" ]; then
        printf '\n' >> "$tmp"
      fi
      printf '%s\n' "$desired" >> "$tmp"
    fi
  else
    printf '%s\n' "$desired" > "$tmp"
  fi
  # Preserve the .env's EXISTING owner rather than letting `install`
  # default to root:root (this script requires EUID=0). If
  # `docker compose` is later run as a non-root ops user, a root:root
  # 0600 .env would be unreadable to them — this file is ops-authored
  # (JOURNAL_GID is the one field install-host.sh itself writes into it)
  # and stays owned by whoever created it. A fresh file (this script
  # creating .env for the first time) has no prior owner to preserve, so
  # it falls back to root:root.
  install_owner="0"
  install_group="0"
  if [ -f "$ENV_FILE" ]; then
    # Resolving by NAME (stat %U/%G) breaks for a uid with no
    # /etc/passwd entry (an .env restored from backup, copied off
    # another host, or living on a shared folder with foreign ownership)
    # — stat then prints the literal string "UNKNOWN", install rejects
    # it ("invalid user 'UNKNOWN'"), and without a checked exit status
    # the step would report "updated" and count it in changed while
    # writing nothing. Numeric ids never hit that: stat -c %u/%g always
    # returns a number, and install always accepts a number.
    existing_owner="$(stat -c '%u' "$ENV_FILE" 2>/dev/null || stat -f '%u' "$ENV_FILE" 2>/dev/null)"
    existing_group="$(stat -c '%g' "$ENV_FILE" 2>/dev/null || stat -f '%g' "$ENV_FILE" 2>/dev/null)"
    [ -n "$existing_owner" ] && install_owner="$existing_owner"
    [ -n "$existing_group" ] && install_group="$existing_group"
  fi
  if ! install -m 0600 -o "$install_owner" -g "$install_group" "$tmp" "$ENV_FILE"; then
    echo "$PROG: failed to install $ENV_FILE" >&2
    TRANSIENT_FAIL=1
    rm -f "$tmp"
    return
  fi
  rm -f "$tmp"
  changed=$((changed+1))
}

# --- run -------------------------------------------------------------

step1
# Must run after step1 (needs the real RASDAEMON_OK/MSMTP_OK it sets)
# and before step3/4/5 (which all gate on the MAIL_OK it computes).
compute_mail_status
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

# Checked before TRANSIENT_FAIL/75: missing ops input is permanent until
# a human edits --env-file, not a condition retrying resolves, so it
# gets its own code (R5) rather than sharing 75's "safe to re-run" label
# with a real package/service transient that could also be set alongside
# it in the same run.
if [ "$MISSING_ENV_INPUT" -eq 1 ]; then
  exit 78
fi

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
