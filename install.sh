#!/bin/bash
# install.sh — R5. The one deliberate bash artifact in this
# repo: it runs on the host as root before the sentinel image exists,
# needs apt-get and systemctl, and shipping a second Go binary to bam
# just to write a handful of config files is more moving parts than an
# idempotent script. Upgrade path: a "sentinel install" subcommand
# if the host part ever grows.
#
# ponytail: kept as a single bash script rather than promoted to a Go
# subcommand for that reason — the ceiling is "the host part grows past
# what a shell script can do cleanly," not a line count.
#
# The binding spec is contracts/runtime.md R5.
set -u
set -o pipefail

PROG="install.sh"
MARK_BEGIN="# >>> agentic-server-supervisor (managed) >>>"
MARK_END="# <<< agentic-server-supervisor (managed) <<<"

CHECK=0
DRY_RUN=0
MAILRISE_HOST="127.0.0.1"
MAILRISE_PORT="8025"
ENV_FILE="./.env"
ENV_FILE_EXPLICIT=0

# Stack creation (curl | sudo bash, nothing copied onto the host first).
# REPO_SLUG is this script's own repository — it is not a generic
# installer for someone else's fork, so this is a constant, not a flag.
REPO_SLUG="thiscantbeserious/agentic-server-supervisor"
REF="${SENTINEL_REF:-main}"
STACK_DIR=""
LAYOUT=""
COMPOSE_NAME=""
ENV_NAME=""
# Set to 1 only when resolve_stack_dir found 2+ compose-root candidates
# under --check/--dry-run and (correctly) refused to pick one — the run
# block below reads this to skip step0b_secrets/step6 rather than
# preview a plan against the unrelated default ./.env, the same
# "state left stale past an early return" shape RENDER_COLLAPSED had.
STACK_UNRESOLVED=0

usage() {
  cat <<EOF
usage: $PROG [--check] [--dry-run] [--mailrise-host HOST] [--mailrise-port PORT] [--env-file PATH] [--stack-dir PATH] [--ref REF] [-h|--help]

  --check                report drift, change nothing; exit 0 if converged, 1 if not
  --dry-run               print every action it would take, change nothing, exit 0
  --mailrise-host HOST    SMTP host smartd/ZED mail is delivered to (default 127.0.0.1)
  --mailrise-port PORT    SMTP port (default 8025)
  --env-file PATH         use this exact env file, unmodified layout (default ./.env);
                          mutually exclusive with --stack-dir
  --stack-dir PATH        create/use the compose stack here — fetches docker-compose.yml
                          and mailrise.conf from this script's own repo, prompts for the
                          bot token/chat id/mailrise password if missing (default:
                          detected — an OpenMediaVault compose root's own "<name>/sentinel"
                          if exactly one is found on this host, /opt/sentinel if none are;
                          several candidates are never picked silently — a numbered menu
                          asks which one, or type a path of your own; offered
                          interactively, Enter accepts a single detected default)
  --ref REF                git ref to fetch deploy/docker-compose.yml and
                          mailrise.conf.example from (default: main, or \$SENTINEL_REF)
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
  # None of the three value-taking flags accepts a value starting with
  # "-" (a hostname, a port, a path) — so a missing value followed by
  # the next flag, long ("--mailrise-host --check") or short
  # ("--env-file -h"), must not silently consume that flag as the
  # value. On a script that runs as root and is meant to run
  # unattended, that would turn a requested --check/-h into a real run
  # against a nonsense smarthost or path.
  case "$2" in
    -*) echo "$PROG: $1 requires a value (got the flag '$2')" >&2; usage >&2; exit 64 ;;
  esac
}

while [ $# -gt 0 ]; do
  case "$1" in
    --check) CHECK=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --mailrise-host) require_value "$@"; MAILRISE_HOST="$2"; shift 2 ;;
    --mailrise-port) require_value "$@"; MAILRISE_PORT="$2"; shift 2 ;;
    --env-file) require_value "$@"; ENV_FILE="$2"; ENV_FILE_EXPLICIT=1; shift 2 ;;
    --stack-dir) require_value "$@"; STACK_DIR="$2"; shift 2 ;;
    --ref) require_value "$@"; REF="$2"; shift 2 ;;
    -h|--help) usage; exit 0 ;;
    *) echo "$PROG: unknown argument: $1" >&2; usage >&2; exit 64 ;;
  esac
done

if [ "$ENV_FILE_EXPLICIT" -eq 1 ] && [ -n "$STACK_DIR" ]; then
  echo "$PROG: --env-file and --stack-dir are mutually exclusive — --env-file targets one file you already filled in yourself; --stack-dir creates a whole new stack" >&2
  usage >&2
  exit 64
fi

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

# Set by docker_preflight, below, once — never assumed. The sentinel
# stack cannot start without both, and the two are checked separately
# because "docker missing" and "docker present but the compose plugin
# is not" are different operator actions to fix.
DOCKER_OK=0
COMPOSE_OK=0

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

  # Computed here, before the CHECK/DRY_RUN early returns below, so a
  # caller under either mode can still report "would collapse N blocks"
  # accurately — block_count is known already at this point, and a
  # collapse is a fact about the file's CURRENT content, not about
  # whether this call is about to write anything.
  if [ "$block_count" -gt 1 ]; then
    RENDER_COLLAPSED="$block_count"
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
      #
      # `repl` carries desired_block, which embeds operator data
      # (step3's `password ${smtp_pass}`) — passed via `env` + ENVIRON,
      # NOT `awk -v repl=...`. `awk -v` performs escape-sequence
      # processing on the assigned value: `awk -v r="p\tb" 'BEGIN{print
      # r}'` prints a literal TAB, not the four characters `p\tb`. This
      # is the third instance of the same family of bug as `sed`'s
      # `s///` and bash's own `${var//pat/rep}` — a mechanism that looks
      # like plain string interpolation but silently reinterprets
      # backslash/ampersand sequences in the value it is handed.
      # `ENVIRON` performs no such processing, so a password containing
      # a literal backslash sequence reaches msmtprc unchanged. `b`/`e`
      # stay as literal script constants (never operator data) but are
      # routed through the same `env`/`ENVIRON` mechanism here too,
      # rather than leaving one `-v` in THIS function "safe by luck" if
      # it is ever handed something else. Three other `awk -v` sites
      # remain elsewhere in this script (outside_block/outside_after's
      # scans, and the pre-existing-`-m`-line comment-out), all carrying
      # only MARK_BEGIN/MARK_END/DISABLED_MARK — fixed script constants,
      # never operator data, so left as `-v`.
      env b="$MARK_BEGIN" e="$MARK_END" repl="$desired_block" awk '
        $0==ENVIRON["b"] {if (!seen) {print ENVIRON["repl"]; seen=1} skip=1; next}
        $0==ENVIRON["e"] {if (skip) {skip=0; next}}
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

# replace_token TEMPLATE TOKEN VALUE — every literal occurrence of TOKEN
# in TEMPLATE replaced with VALUE, printed to stdout. Deliberately NOT
# bash's own "${template//TOKEN/VALUE}" pattern substitution: that was
# measured to have the SAME pitfall sed's s/// has, just less
# documented — bash 5.2's own replacement text treats an unescaped '&'
# as "the matched text" too. Reproduced directly:
# x="hello WORLD bye"; echo "${x//WORLD/A&B}" prints "hello AWORLDB bye",
# not "hello A&B bye". This function uses only prefix/suffix REMOVAL
# (${var%%pat*}, ${var#*pat}) and plain "$value" concatenation — neither
# operation has any "replacement text" concept, so no character VALUE
# contains can ever be treated as special. VALUE is used exactly once
# per occurrence via ordinary variable interpolation, which is always
# 100% literal in bash.
replace_token() {
  local __tpl="$1" __token="$2" __value="$3" __result="" __rest="$1"
  while [[ "$__rest" == *"$__token"* ]]; do
    __result="${__result}${__rest%%"$__token"*}${__value}"
    __rest="${__rest#*"$__token"}"
  done
  printf '%s' "${__result}${__rest}"
}

# --- stack creation (curl | sudo bash, nothing copied onto the host first) ---
#
# When --env-file was given explicitly, none of this runs: ENV_FILE is
# exactly what the operator passed, matching every prior version of this
# script (ENV_FILE_EXPLICIT gates the two calls in the run section below).
# This section exists only for the opposite case, where nothing exists on
# the host yet and the script itself has to decide where the stack lives,
# create it, and fill in the handful of values only a human or the host
# itself can supply.

# have_tty succeeds only if this process has a controlling terminal
# reachable at /dev/tty. Under `curl -fsSL URL | sudo bash`, fd 0 IS the
# piped script, not a place to read an operator's answer from — reading a
# prompt from stdin in that shape would silently consume a line of shell
# source as a bot token. /dev/tty is the one path that still refers to the
# real terminal (or, under `ssh -t`, the allocated pty) regardless of what
# stdin is bound to.
have_tty() {
  { : <>/dev/tty; } 2>/dev/null
}

# prompt_with_default PROMPT DEFAULT — shows PROMPT and DEFAULT, reads one
# line from /dev/tty, echoes DEFAULT verbatim if the operator just pressed
# Enter. Never used for secrets.
prompt_with_default() {
  local val=""
  printf '%s [%s]: ' "$1" "$2" > /dev/tty
  IFS= read -r val < /dev/tty
  if [ -z "$val" ]; then
    printf '%s' "$2"
  else
    printf '%s' "$val"
  fi
}

# prompt_secret PROMPT — reads one line from /dev/tty with terminal echo
# off (read -rs), so the value never appears on screen, in scrollback, or
# in any session log a terminal multiplexer might keep. Same class of rule
# as C7's prohibition on logging the apprise key: a secret that touches
# this script's own output at all is a secret that can leak through a log
# nobody thought to protect.
prompt_secret() {
  local val=""
  printf '%s' "$1" > /dev/tty
  read -rs val < /dev/tty
  printf '\n' > /dev/tty
  printf '%s' "$val"
}

# require_secret NAME PROMPT CURRENT SILENT — sets REQUIRE_SECRET_RESULT.
# Deliberately NOT called as val="$(require_secret ...)": command
# substitution forks a subshell, and MISSING_ENV_INPUT=1 set inside a
# subshell never reaches the parent shell — an earlier version of this
# function did exactly that and silently lost the flag, which is the
# kind of bug this whole design exists to prevent (a missing secret
# reported as fine). A global result variable has no subshell to lose
# it in.
#
# Returns CURRENT unchanged if already non-empty: idempotent re-run,
# never re-prompt for a value already in the env file. Otherwise prompts
# (silently, read -rs, if SILENT=1; visibly, read -r, if SILENT=0 — a
# chat id is not a credential) when a terminal is available. Under
# --check/--dry-run, which must never prompt, records the gap as drift
# instead. With no terminal at all on a real run, sets MISSING_ENV_INPUT
# and returns empty — never a silent empty write, which is the one
# failure mode worse than any prompt: a supervisor stack that starts,
# reports healthy, and never delivers a single notification.
require_secret() {
  local name="$1" prompt="$2" current="$3" silent="$4"
  REQUIRE_SECRET_RESULT=""
  if [ -n "$current" ]; then
    REQUIRE_SECRET_RESULT="$current"
    return
  fi
  if [ "$CHECK" -eq 1 ] || [ "$DRY_RUN" -eq 1 ]; then
    note "stack secrets: $name not yet set (would prompt)"
    changed=$((changed+1))
    return
  fi
  if ! have_tty; then
    echo "$PROG: $name is required and not already set in $ENV_FILE, but no controlling terminal is available to prompt for it (this is what \`curl | bash\` looks like without a real terminal) — re-run with a terminal attached, or pre-fill $ENV_FILE and pass --env-file" >&2
    MISSING_ENV_INPUT=1
    return
  fi
  if [ "$silent" -eq 1 ]; then
    REQUIRE_SECRET_RESULT="$(prompt_secret "$prompt")"
  else
    local val=""
    printf '%s' "$prompt" > /dev/tty
    IFS= read -r val < /dev/tty
    REQUIRE_SECRET_RESULT="$val"
  fi
  # A terminal was available and the operator answered — but Enter with
  # nothing typed is not an answer. Treated identically to the no-TTY
  # case above: this is the same "I still don't have it" outcome by a
  # different route, and MAILRISE_SMTP_USER/PASS only escaped this
  # exact gap by luck (compute_mail_status re-reads the env file
  # independently and catches a blank password on its own) — TOKEN and
  # CHAT_ID have no such second check, and compose does not :?-guard
  # them, so an empty answer here used to install a stack that reports
  # success with no Telegram credentials at all.
  if [ -z "$REQUIRE_SECRET_RESULT" ]; then
    echo "$PROG: $name was left empty at the prompt — it is still required; re-run and this script will prompt only for what is still missing" >&2
    MISSING_ENV_INPUT=1
  fi
}

# verb_phrase ACTUAL WOULD — the top-level run summary is the last thing
# an operator reads before deciding to write to a real host, so it must
# never claim more than the mode actually did. The per-action lines
# already say "[dry-run] would ..."; this keeps the summary lines that
# roll those up honest under --check/--dry-run too, which never write.
verb_phrase() {
  if [ "$CHECK" -eq 1 ] || [ "$DRY_RUN" -eq 1 ]; then
    printf '%s' "$2"
  else
    printf '%s' "$1"
  fi
}

# env_var_value FILE KEY — current value of KEY in FILE, quote-stripped,
# empty if absent. Reuses strip_quotes for the same reason
# compute_mail_status already does: a quoted value in an operator-edited
# file must not carry its quote characters into a credential.
env_var_value() {
  local file="$1" key="$2"
  [ -f "$file" ] || { printf ''; return; }
  strip_quotes "$(grep "^${key}=" "$file" 2>/dev/null | tail -n1 | cut -d= -f2-)"
}

# upsert_env_var FILE KEY VALUE — sets KEY=VALUE in FILE only if KEY is
# currently absent or empty there; a key already carrying a non-empty
# value is left untouched, so re-running never clobbers an operator's own
# edit or a value a prior run already prompted for. VALUE="" is a no-op —
# the caller is responsible for not calling this when a required secret
# is still unknown, and skipping the write here too is a second guard
# against ever staging an empty assignment. Mode 0600 root:root always:
# unlike step6's ENV_FILE (which may pre-exist under the old --env-file
# path and must preserve its owner), this file is one this script is
# creating fresh.
upsert_env_var() {
  local file="$1" key="$2" value="$3"
  local current=""
  [ -f "$file" ] && current="$(grep "^${key}=" "$file" 2>/dev/null | tail -n1 | cut -d= -f2-)"
  if [ -n "$current" ] || [ -z "$value" ]; then
    return 1
  fi
  if [ "$CHECK" -eq 1 ] || [ "$DRY_RUN" -eq 1 ]; then
    return 0
  fi
  local tmp
  tmp="$(mktemp)"
  if [ -f "$file" ]; then
    cp "$file" "$tmp"
    if [ -s "$tmp" ] && [ -n "$(tail -c1 "$tmp")" ]; then
      printf '\n' >> "$tmp"
    fi
  fi
  printf '%s=%s\n' "$key" "$value" >> "$tmp"
  if ! install -m 0600 -o root -g root "$tmp" "$file"; then
    echo "$PROG: failed to write $key to $file" >&2
    TRANSIENT_FAIL=1
    rm -f "$tmp"
    return 1
  fi
  rm -f "$tmp"
  return 0
}

set_env_default() {  # KEY VALUE
  local key="$1" value="$2"
  if upsert_env_var "$ENV_FILE" "$key" "$value"; then
    changed=$((changed+1))
  fi
}

# write_file_if_differs FILE MODE SRC_PATH — installs SRC_PATH to FILE at
# MODE only if the content differs (or FILE is missing). Same
# mktemp+install atomic-replace pattern as render_managed_block, but for
# a file this script owns in full rather than one shared with an
# operator's own content, so there is no marker block: the whole file is
# compared and replaced.
write_file_if_differs() {
  local file="$1" mode="$2" src="$3"
  if [ -f "$file" ] && cmp -s "$file" "$src"; then
    return 1
  fi
  if [ "$CHECK" -eq 1 ]; then
    return 0
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] would write $file (mode $mode)"
    return 0
  fi
  if ! install -m "$mode" -o root -g root "$src" "$file"; then
    echo "$PROG: failed to install $file" >&2
    TRANSIENT_FAIL=1
    return 1
  fi
  return 0
}

# ensure_dir DIR MODE DESC — idempotent mkdir with the same
# note/changed/--check/--dry-run bookkeeping every other step uses.
ensure_dir() {
  local dir="$1" mode="$2" desc="$3"
  if [ -d "$dir" ]; then
    note "$desc: $dir already exists"
    return
  fi
  note "$desc: $(verb_phrase "creating" "would create") $dir"
  if [ "$CHECK" -eq 1 ]; then
    changed=$((changed+1))
    return
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] would create $dir (mode $mode)"
    changed=$((changed+1))
    return
  fi
  # mkdir -m only applies its mode to the deepest directory when -p
  # creates more than one level, so the mode is set explicitly afterward
  # rather than trusted to -m -p together.
  if ! mkdir -p "$dir" || ! chmod "$mode" "$dir"; then
    echo "$PROG: failed to create $dir" >&2
    TRANSIENT_FAIL=1
    return
  fi
  changed=$((changed+1))
}

# ensure_symlink LINK TARGET_BASENAME — creates LINK -> TARGET_BASENAME
# (relative, so the stack directory stays movable) only if it does not
# already point there. Never touches a LINK that exists as a regular
# file — that would mean real content already lives there, and silently
# replacing it is exactly the kind of surprise this whole script exists
# to avoid.
ensure_symlink() {
  local link="$1" target="$2"
  if [ -L "$link" ]; then
    if [ "$(readlink "$link")" = "$target" ]; then
      note "stack symlink: $link -> $target already correct"
      return
    fi
  elif [ -e "$link" ]; then
    echo "$PROG: $link exists and is not a symlink to $target — refusing to overwrite it" >&2
    TRANSIENT_FAIL=1
    return
  fi
  note "stack symlink: $(verb_phrase "creating" "would create") $link -> $target"
  if [ "$CHECK" -eq 1 ]; then
    changed=$((changed+1))
    return
  fi
  if [ "$DRY_RUN" -eq 1 ]; then
    echo "[dry-run] would symlink $link -> $target"
    changed=$((changed+1))
    return
  fi
  if ! ln -sfn "$target" "$link"; then
    echo "$PROG: failed to create symlink $link" >&2
    TRANSIENT_FAIL=1
    return
  fi
  changed=$((changed+1))
}

# ensure_curl installs curl if it is missing — needed to fetch the stack
# files from this script's own repository. Best-effort under
# --check/--dry-run (never installs anything there; reports the gap).
ensure_curl() {
  if command -v curl >/dev/null 2>&1; then
    return 0
  fi
  if [ "$CHECK" -eq 1 ] || [ "$DRY_RUN" -eq 1 ]; then
    echo "$PROG: curl is not installed — cannot fetch the stack files (would run: apt-get install -y curl)" >&2
    return 1
  fi
  apt-get update -qq || true
  apt-get install -y --no-install-recommends curl || true
  command -v curl >/dev/null 2>&1
}

# fetch_repo_file REPO_PATH SIGNATURE — downloads REPO_PATH from this
# script's own repository at $REF into FETCHED_TMP. Checks the HTTP
# status explicitly and that the payload contains SIGNATURE — a cheap
# defence against a captive portal, a proxy, or a bad ref answering 200
# with something that is not the file asked for, which `curl -f` alone
# does not catch when the intermediary itself answers success.
fetch_repo_file() {
  local repo_path="$1" signature="$2"
  local url="https://raw.githubusercontent.com/${REPO_SLUG}/${REF}/${repo_path}"
  # code and rc are declared separately from the assignment on purpose:
  # `local code="$(curl ...)"` would make `local` itself the last command
  # in that statement, so `rc=$?` right after would capture local's exit
  # status (always 0) instead of curl's — the whole status check below
  # would silently become dead code that still looks correct.
  local code rc
  FETCHED_TMP="$(mktemp)"
  code="$(curl -fsSL -w '%{http_code}' -o "$FETCHED_TMP" "$url" 2>/dev/null)"
  rc=$?
  if [ "$rc" -ne 0 ] || [ "$code" != "200" ]; then
    echo "$PROG: failed to fetch ${repo_path} from ref '${REF}' (curl exit=${rc}, http=${code:-none}) — check network access and that --ref/\$SENTINEL_REF names a real ref" >&2
    rm -f "$FETCHED_TMP"; FETCHED_TMP=""
    return 1
  fi
  if [ ! -s "$FETCHED_TMP" ] || ! grep -q "$signature" "$FETCHED_TMP"; then
    echo "$PROG: fetched ${repo_path} from ref '${REF}' does not look right (empty, or missing expected content) — refusing to write it" >&2
    rm -f "$FETCHED_TMP"; FETCHED_TMP=""
    return 1
  fi
  return 0
}

# invoking_home — the home directory of the user who ran sudo, not
# root's. `sudo bash` sets SUDO_USER; resolved via getent rather than
# trusting $HOME, which under sudo may already be root's unless the
# invocation happens to preserve it.
invoking_home() {
  local u="${SUDO_USER:-}"
  if [ -n "$u" ]; then
    local h
    h="$(getent passwd "$u" 2>/dev/null | cut -d: -f6)"
    if [ -n "$h" ]; then
      printf '%s' "$h"
      return
    fi
  fi
  printf '%s' "$HOME"
}

# compose_root_looks_omv DIR — true if DIR is laid out the way OMV's
# compose plugin lays a shared-folder root out: at least one
# subdirectory holding "<name>.yml" with a "compose.yml" symlink
# pointing at it, the shape every OMV-created stack has regardless of
# which shared folder the operator picked for the plugin. This is the
# PRIMARY signal (over asking OMV itself, below): it tests the property
# that actually matters — "is this directory laid out the way OMV lays a
# compose root out" — rather than a path OMV happens to default to on
# this one host, and it still works when --stack-dir names a compose
# root this script would never have guessed. The trade-off it accepts:
# a root with zero stacks created yet has nothing to pattern-match, so
# this returns false for it — omv_confdbadm_compose_root below is what
# that specific case falls back to.
compose_root_looks_omv() {
  local root="$1" d base
  [ -d "$root" ] || return 1
  # Capped at 200 entries via `find ... | head`, which stops reading the
  # directory (SIGPIPE, once head has its 200 lines) rather than
  # scanning it in full. A bash glob ("$root"/*/) cannot do this: glob
  # expansion enumerates and sorts EVERY entry before the loop below
  # could even start, so a `break` inside the loop body only bounds the
  # cheap part — the expensive part (stat'ing every child of a wide
  # share) would already be done. Measured: 255ms warm/overlayfs for a
  # 23,019-entry directory with the unbounded glob; on a large,
  # cold-cache pool this is what turns the FIRST thing an operator ever
  # runs into a multi-second stall on an ordinary wide share
  # (Downloads, Backup) that was never an OMV compose root to begin
  # with. A real OMV compose root is recognisable from a handful of
  # children, so 200 is generous headroom, not a tight fit — not
  # exhaustive (a root whose first 200 entries are all non-stack
  # clutter would be missed), the same "not exhaustive" trade-off the
  # whole candidate scan already documents, one level deeper.
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    base="$(basename "$d")"
    if [ -f "${d}/${base}.yml" ] && [ -L "${d}/compose.yml" ] \
       && [ "$(readlink "${d}/compose.yml")" = "${base}.yml" ]; then
      return 0
    fi
  done < <(find "$root" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | head -n 200)
  return 1
}

# omv_confdbadm_bin — locates the omv-confdbadm binary without trusting
# a bare name on PATH: it lives at /usr/sbin/omv-confdbadm, not
# /usr/bin, and a normal user's PATH does not include /usr/sbin at all.
# `sudo`'s secure_path usually does (this script always runs as root),
# so `command -v` alone would usually work — "usually" is exactly the
# word that means it is not something to rely on. Absolute path first,
# `command -v` as a fallback for a host where the plugin ships it
# somewhere else.
omv_confdbadm_bin() {
  [ -x /usr/sbin/omv-confdbadm ] && { printf '%s' /usr/sbin/omv-confdbadm; return 0; }
  command -v omv-confdbadm 2>/dev/null
}

# omv_confdbadm_compose_root — best-effort SECONDARY signal, authoritative
# when it works: asks OMV's own config database where the compose
# plugin's shared folder lives, instead of inferring it from disk
# layout. It exists for the one case structural detection cannot see at
# all — a freshly enabled compose plugin with zero stacks created yet,
# so there is nothing on disk to pattern-match.
#
# omv-confdbadm requires root. Confirmed against a real OMV host: run
# unprivileged it does not print a clean error, it emits a multi-line
# Python traceback (ending in a config-database load failure) and exits
# non-zero. This script always runs as root (checked at startup) so
# that specific failure should not occur in practice, but the parsing
# below treats it as a live threat rather than an assumption: the exit
# status is captured explicitly (not folded into `cmd || return 1`, so
# it is auditable at a glance) and checked before ANY extraction runs,
# and the captured output must additionally look like the JSON this
# command is documented to emit (starts with '{' or '[') before the
# grep/sed pipeline below ever touches it. A traceback goes to stderr
# (discarded here) so stdout is empty in that case regardless, and the
# JSON-shape guard is the second, independent reason a traceback could
# never be scraped for a UUID- or path-shaped substring even if it
# somehow reached stdout — a loose parser could otherwise hand back a
# Python library path out of the traceback's own file references as if
# it were a real compose root.
#
# The SUCCESSFUL output shape (the `sharedfolderref` → `conf.system.
# sharedfolder` UUID resolution below) is still NOT verified against a
# real OMV host as part of this change — CLAUDE.md keeps T8's live
# validation against bam read-only, and the read-only probe that did run
# needs root to see a real answer, which the operator declined to hand
# over interactively. `conf.service.compose` is documented, across the
# OMV versions consulted while writing this, to carry the shared folder
# as a reference (`sharedfolderref`, a UUID) resolved through a second
# lookup (`conf.system.sharedfolder`) — this remains an assumption until
# a real successful run confirms it. Any shape this does not recognise
# degrades to "unknown" (return 1), never a guess. No caller ever
# prefers this over an explicit structural "no" — it only answers the
# empty-root case compose_root_looks_omv cannot.
omv_confdbadm_compose_root() {
  local bin cfg rc ref path
  bin="$(omv_confdbadm_bin)"
  [ -n "$bin" ] || return 1

  cfg="$("$bin" read conf.service.compose 2>/dev/null)"
  rc=$?
  [ "$rc" -eq 0 ] && [ -n "$cfg" ] || return 1
  # $() strips only TRAILING newlines, never leading whitespace — a
  # well-formed answer that happens to start with a blank line or
  # leading spaces would otherwise be rejected by the shape guard below
  # exactly like a real failure would be. The real output shape is
  # still unverified, so this trims defensively rather than betting on
  # a format nobody has confirmed: better to tolerate benign leading
  # whitespace than to leave the fresh-install zero-stacks case
  # permanently broken by a formatting detail this script never
  # actually depends on. Trims only for the shape check; extraction
  # below (grep -o, not anchored to the start) never needed this.
  cfg="${cfg#"${cfg%%[![:space:]]*}"}"
  case "$cfg" in
    '{'*) ;;
    *) return 1 ;;
  esac
  ref="$(printf '%s' "$cfg" | grep -o '"sharedfolderref"[[:space:]]*:[[:space:]]*"[^"]*"' | head -n1 | sed -E 's/.*"([^"]*)"$/\1/')"
  [ -n "$ref" ] || return 1

  cfg="$("$bin" read conf.system.sharedfolder 2>/dev/null)"
  rc=$?
  [ "$rc" -eq 0 ] && [ -n "$cfg" ] || return 1
  cfg="${cfg#"${cfg%%[![:space:]]*}"}"
  case "$cfg" in
    '['*|'{'*) ;;
    *) return 1 ;;
  esac
  # Flattened to one line first, then matched as a single flat `{...}`
  # OBJECT containing our uuid — not a line-adjacency `grep -B2` between
  # separately-matched "uuid" and "path" lines. A `-B2` window silently
  # assumes "path" sits within two lines AFTER "uuid" in the source
  # text; real `omv-confdbadm read` output was not confirmed to be
  # formatted that way (root access to check was declined), and if it
  # pretty-prints with "path" before "uuid", or more than two lines
  # apart, `-B2` never matches at all — this signal would silently stay
  # "unknown" forever on a real host, exactly the class of bug a
  # showcase-only test fixture cannot catch. Matching within one flat
  # object is both order- and distance-independent for the one shape
  # that matters (sharedfolder entries are flat records, no nested
  # braces), and `[^{}]*` stops at the first `}` so it cannot span
  # multiple array entries by accident.
  local flat obj
  flat="$(printf '%s' "$cfg" | tr '
' ' ')"
  obj="$(printf '%s' "$flat" | grep -o "{[^{}]*}" | grep "\"uuid\"[[:space:]]*:[[:space:]]*\"${ref}\"" | head -n1)"
  [ -n "$obj" ] || return 1
  path="$(printf '%s' "$obj" | grep -o '"path"[[:space:]]*:[[:space:]]*"[^"]*"' | head -n1 | sed -E 's/.*"([^"]*)"$/\1/')"
  [ -n "$path" ] || return 1

  # Defense in depth beyond the JSON-shape guard above: even a value
  # that legitimately starts with '{'/'[' and parses cleanly must still
  # look like a real shared-folder mount, not an arbitrary string
  # (a future omv-confdbadm error mode that emits well-formed JSON
  # instead of a Python traceback is not something this script can rule
  # out). A compose root is never rooted under one of these system
  # directories on any real host.
  case "$path" in
    /|/usr*|/lib*|/bin*|/sbin*|/etc*|/proc*|/sys*|/dev*|/run*|/boot*) return 1 ;;
    /*) ;;
    *) return 1 ;;
  esac
  printf '%s' "$path"
}

# candidate_compose_roots — a BOUNDED list of directories worth checking
# structurally when nothing has yet said where OMV's compose root is.
# Deliberately fixed glob patterns, never a recursive `find /` or `find
# /srv` — a real walk of a multi-disk NAS pool can take minutes and
# looks indistinguishable from a hang. Each pattern reaches at most one
# level into a plausible parent (`/docker-compose` itself, one level
# under the shared-folder mount conventions OMV commonly uses —
# `/srv/dev-disk-by-uuid-*`, `/srv/dev-disk-by-label-*`, `/srv/*` — and
# one level under `/opt` and `/var/lib`, the two other places a manually
# configured shared folder is commonly rooted). A bare `/*` scan of the
# whole filesystem root was deliberately left out: it would need
# explicit pruning of /proc, /sys, /dev, /run and similar pseudo
# filesystems to stay bounded, and every realistic OMV shared-folder
# convention is already covered by the patterns below. Not exhaustive —
# a custom location matching none of these needs --stack-dir, and
# resolve_stack_dir says so explicitly whenever it prints the candidate
# list.
candidate_compose_roots() {
  local d
  for d in /docker-compose \
           /srv/dev-disk-by-uuid-*/* /srv/dev-disk-by-label-*/* /srv/* \
           /opt/* /var/lib/*; do
    [ -d "$d" ] && printf '%s\n' "$d"
  done
}

# count_existing_stacks DIR — how many subdirectories of DIR are shaped
# like an OMV-managed stack (the same "<name>.yml" + "compose.yml"
# symlink shape compose_root_looks_omv checks for). Used only to give an
# operator choosing among several candidates something to tell them
# apart by besides the bare path.
count_existing_stacks() {
  local root="$1" d base n=0
  # Same bounded find|head as compose_root_looks_omv, same reason — a
  # bash glob would enumerate the whole directory before this loop
  # could even start counting. Only reached for candidates already
  # confirmed structurally OMV-shaped (the ambiguous-menu display), so
  # lower-frequency than that scan, but no reason to reintroduce the
  # unbounded cost here.
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    base="$(basename "$d")"
    if [ -f "${d}/${base}.yml" ] && [ -L "${d}/compose.yml" ]; then
      n=$((n+1))
    fi
  done < <(find "$root" -mindepth 1 -maxdepth 1 -type d 2>/dev/null | head -n 200)
  printf '%d' "$n"
}

# detect_omv_compose_root_candidates — every directory that plausibly IS
# an OMV compose root, one per line, deduplicated by resolved path.
# Deliberately a LIST rather than a single "best" answer: a host can
# have more than one directory matching the shape (a leftover from a
# migrated install, a second data disk with its own shared folder), and
# picking one silently is the same guess-on-the-operator's-behalf this
# whole detection redesign exists to eliminate. Combines both signals —
# structural matches among candidate_compose_roots, PLUS
# omv_confdbadm_compose_root's answer even when it names a root with
# zero stacks in it yet (the one case structural detection cannot see
# at all) — rather than preferring one signal over the other.
detect_omv_compose_root_candidates() {
  local d resolved cfg seen=""
  while IFS= read -r d; do
    [ -n "$d" ] || continue
    if compose_root_looks_omv "$d"; then
      resolved="$(readlink -f "$d")"
      case " $seen " in
        *" $resolved "*) ;;
        *)
          seen="$seen $resolved"
          printf '%s\n' "$resolved"
          ;;
      esac
    fi
  done <<CANDIDATES
$(candidate_compose_roots)
CANDIDATES
  if cfg="$(omv_confdbadm_compose_root)" && [ -n "$cfg" ] && [ -d "$cfg" ]; then
    resolved="$(readlink -f "$cfg")"
    case " $seen " in
      *" $resolved "*) ;;
      *) printf '%s\n' "$resolved" ;;
    esac
  fi
}

# detect_layout inspects STACK_DIR's PARENT itself, never how STACK_DIR
# was chosen — an explicit --stack-dir pointing under a structurally
# OMV-shaped root gets the OMV layout exactly the same as the
# auto-detected default would. A plain directory gets docker-compose.yml
# + .env directly: sentinel.yml plus a compose.yml symlink would be
# pointless indirection anywhere that is not actually an OMV compose
# root.
detect_layout() {
  local parent cfgroot
  parent="$(dirname "$STACK_DIR")"
  if compose_root_looks_omv "$parent"; then
    LAYOUT="omv"
    COMPOSE_NAME="sentinel.yml"
    ENV_NAME="sentinel.env"
    return
  fi
  # No sibling stack to pattern-match against — a first stack on a
  # fresh OMV install, or an ordinary directory that just happens to be
  # empty, look identical to structural detection. Fall back to asking
  # OMV directly; anything short of an authoritative match (no
  # omv-confdbadm, an unparsable answer, a different path entirely)
  # means plain, exactly like any ordinary directory would get.
  if cfgroot="$(omv_confdbadm_compose_root)" && [ -n "$cfgroot" ] \
     && [ "$(readlink -f "$cfgroot" 2>/dev/null)" = "$parent" ]; then
    LAYOUT="omv"
    COMPOSE_NAME="sentinel.yml"
    ENV_NAME="sentinel.env"
    return
  fi
  LAYOUT="plain"
  COMPOSE_NAME="docker-compose.yml"
  ENV_NAME=".env"
}

# resolve_stack_dir sets STACK_DIR. Precedence: --stack-dir flag always
# wins outright — no scan, no prompt, the operator being explicit beats
# anything this script could detect. Otherwise the candidate count
# decides:
#   0 candidates  — the conventional /opt/sentinel default, exactly as
#                   before detection existed at all.
#   1 candidate   — proposed as the default, with a one-line reason so
#                   the operator sees it was DETECTED, not decreed.
#   2+ candidates — never picked silently: a numbered menu (real run,
#                   terminal only) with an option to type a path of its
#                   own; --check/--dry-run reports the ambiguity and the
#                   full list without prompting (dry-run/check must
#                   never prompt, and hiding the ambiguity in a preview
#                   is worse than showing it); no terminal at all is
#                   exit 78, same as the no-candidate case below, naming
#                   every candidate in the message.
# The one/several distinction all funnel through interactively (real
# run, terminal) into the same prompt_with_default/have_tty shape 0
# candidates already used, so a real run with exactly one candidate and
# a real run with zero candidates read identically to the operator
# except for the proposed default and the one-line "why".
resolve_stack_dir() {
  if [ -n "$STACK_DIR" ]; then
    return
  fi
  local candidates count=0 c
  candidates="$(detect_omv_compose_root_candidates)"
  if [ -n "$candidates" ]; then
    count="$(printf '%s\n' "$candidates" | grep -c .)"
  fi

  if [ "$count" -eq 0 ]; then
    resolve_stack_dir_prompt "/opt/sentinel" ""
    return
  fi

  if [ "$count" -eq 1 ]; then
    echo "$PROG: detected a possible OpenMediaVault compose root at $candidates" >&2
    resolve_stack_dir_prompt "${candidates}/sentinel" ""
    return
  fi

  # 2+ candidates: never guessed. Listed with enough context (path, how
  # many existing stacks) that a choice between them means something,
  # and flagged as possibly incomplete — the scan is bounded (see
  # candidate_compose_roots) and a custom location can still be missed.
  echo "$PROG: multiple possible OpenMediaVault compose roots found — refusing to guess which one you meant:" >&2
  local i=1 selected=""
  while IFS= read -r c; do
    [ -n "$c" ] || continue
    echo "  $i) $c ($(count_existing_stacks "$c") existing stack(s))" >&2
    i=$((i+1))
  done <<CANDS
$candidates
CANDS
  echo "  (this list is not guaranteed exhaustive — pass --stack-dir directly if yours is not shown)" >&2

  if [ "$CHECK" -eq 1 ] || [ "$DRY_RUN" -eq 1 ]; then
    note "stack directory: ambiguous — $count possible OMV compose roots found (see stderr for the list); pass --stack-dir to choose"
    changed=$((changed+1))
    STACK_UNRESOLVED=1
    return
  fi

  if have_tty; then
    local choice
    printf 'Enter a number (1-%d), or type a path: ' "$count" > /dev/tty
    IFS= read -r choice < /dev/tty
    case "$choice" in
      ''|*[!0-9]*)
        selected="$choice"
        ;;
      *)
        if [ "$choice" -ge 1 ] && [ "$choice" -le "$count" ]; then
          selected="$(printf '%s\n' "$candidates" | sed -n "${choice}p")/sentinel"
        fi
        ;;
    esac
    if [ -z "$selected" ]; then
      echo "$PROG: no valid selection made — refusing to guess; re-run and choose a number or a path" >&2
      exit 78
    fi
    STACK_DIR="$selected"
    return
  fi

  echo "$PROG: no controlling terminal available to choose among them (this is what \`curl | sudo bash\` looks like without a real terminal) — re-run with a terminal attached, or pass --stack-dir explicitly" >&2
  exit 78
}

# resolve_stack_dir_prompt PROPOSED _RESERVED — the shared real-run/
# check/dry-run/no-tty decision every candidate-count branch above ends
# in once it has settled on a single PROPOSED default: offered
# interactively (Enter accepts it) when a terminal is available, used
# silently under --check/--dry-run (which must never prompt), or — no
# terminal, a real run — refused outright with exit 78 rather than
# guessing a directory to write into. Factored out because the 0- and
# 1-candidate cases are otherwise identical but for how PROPOSED was
# chosen.
resolve_stack_dir_prompt() {
  local proposed="$1"
  if [ "$CHECK" -eq 1 ] || [ "$DRY_RUN" -eq 1 ]; then
    STACK_DIR="$proposed"
    return
  fi
  if have_tty; then
    STACK_DIR="$(prompt_with_default "Install the stack in" "$proposed")"
    return
  fi
  echo "$PROG: no --stack-dir given and no controlling terminal available to prompt for one (this is what \`curl | sudo bash\` looks like without a real terminal) — refusing to guess where to write the stack; re-run with a terminal attached, or pass --stack-dir explicitly" >&2
  exit 78
}

# docker_preflight runs before any host mutation and checks whether the
# sentinel stack could actually start: the docker CLI, a reachable
# daemon, and the compose PLUGIN (`docker compose`, not the legacy
# standalone `docker-compose` binary this project's compose file is
# never invoked through). Deliberately a WARNING, never fatal: steps
# 1-5 (rasdaemon, msmtp, smartd, ZED) have standalone value on a host
# that never runs a single container, and this script does not install
# docker itself, so refusing the whole run over a runtime it cannot
# provide would be worse than reporting it and continuing. What must
# never happen is the run summary implying the stack is ready when it
# is not — every step downstream that depends on DOCKER_OK/COMPOSE_OK
# (step_apprise_seed) says so explicitly, not just here.
docker_preflight() {
  if ! command -v docker >/dev/null 2>&1; then
    note "docker preflight: docker not found on PATH — install docker before the stack can be started; the steps above/below still apply on their own"
    return
  fi
  if ! docker info >/dev/null 2>&1; then
    note "docker preflight: docker found but the daemon is not reachable (not running, or this user lacks permission) — the stack cannot be started until it is"
    return
  fi
  DOCKER_OK=1
  if docker compose version >/dev/null 2>&1; then
    COMPOSE_OK=1
    note "docker preflight: docker daemon reachable, compose plugin present"
    return
  fi
  if command -v docker-compose >/dev/null 2>&1; then
    note "docker preflight: only the legacy standalone docker-compose binary is present — this stack needs the compose PLUGIN (\`docker compose\`), not docker-compose; install docker-compose-plugin"
    return
  fi
  note "docker preflight: docker daemon reachable but the compose plugin is missing (\`docker compose version\` failed) — install docker-compose-plugin before the stack can be started"
}

# step0a_layout resolves where the stack lives, fetches
# deploy/docker-compose.yml from this script's own repo at $REF, and
# creates the directory/compose-file/symlink layout. Runs before step1:
# a missing terminal or a bad --ref is worth knowing before spending time
# on apt-get.
step0a_layout() {
  resolve_stack_dir
  # STACK_DIR stays empty here only in one case: --check/--dry-run found
  # several possible compose roots and (correctly) refused to pick one —
  # resolve_stack_dir already reported the ambiguity and the candidate
  # list. There is no coherent directory left to preview further steps
  # against, so this step stops here rather than resolving "" through
  # readlink -f (which would silently resolve to the current directory).
  if [ -z "$STACK_DIR" ]; then
    return
  fi
  # dirname (in detect_layout, below) is purely lexical, so a stack
  # directory reached through a symlink into an OMV compose root would
  # otherwise be classified "plain" even though it really sits inside
  # one — the files land in the right place on disk while OMV never
  # enumerates the stack, which produces exactly the "the stack looks
  # fine but the plugin doesn't know it exists" shape this whole design
  # exists to avoid. readlink -f resolves every symlink component and
  # still works when the deepest component does not exist yet, the
  # common case for a stack being created for the first time.
  STACK_DIR="$(readlink -f "$STACK_DIR")"
  # Refusing the compose root ITSELF (not a stack directory inside one)
  # follows the same two signals as detect_layout: structural shape
  # first (STACK_DIR already holds other stacks), OMV's own config
  # second (settles it even for a root with nothing in it yet). Neither
  # signal is a literal path — a directory that merely happens to be
  # named /docker-compose but is not actually a detected root is left
  # alone; see compose_root_looks_omv/omv_confdbadm_compose_root above.
  if compose_root_looks_omv "$STACK_DIR"; then
    echo "$PROG: --stack-dir $STACK_DIR is itself an OMV compose root — it already holds other stacks laid out the way OMV's compose plugin lays them out — not a stack directory inside one; writing a compose file there would sit alongside every other stack's own directory instead of inside one; use a subdirectory, e.g. ${STACK_DIR}/sentinel" >&2
    exit 64
  fi
  local omv_root_cfg
  omv_root_cfg="$(omv_confdbadm_compose_root)" || omv_root_cfg=""
  if [ -n "$omv_root_cfg" ] && [ "$(readlink -f "$omv_root_cfg" 2>/dev/null)" = "$STACK_DIR" ]; then
    echo "$PROG: --stack-dir $STACK_DIR is the compose root OMV's own configuration reports, not a stack directory inside it; use a subdirectory, e.g. ${STACK_DIR}/sentinel" >&2
    exit 64
  fi
  detect_layout
  ENV_FILE="${STACK_DIR}/${ENV_NAME}"
  echo "$PROG: stack directory: $STACK_DIR (layout: $LAYOUT, ref: $REF)" >&2

  if ! ensure_curl; then
    echo "$PROG: curl is required to fetch the stack files and could not be installed" >&2
    TRANSIENT_FAIL=1
    return
  fi

  ensure_dir "$STACK_DIR" 0700 "stack directory"
  ensure_dir "${STACK_DIR}/mailrise" 0700 "mailrise directory"

  if fetch_repo_file "deploy/docker-compose.yml" "container_name: sentinel"; then
    local compose_path="${STACK_DIR}/${COMPOSE_NAME}"
    if write_file_if_differs "$compose_path" 0644 "$FETCHED_TMP"; then
      note "stack compose file: $compose_path $(verb_phrase "written/updated" "would be written/updated") (fetched from ref $REF)"
      changed=$((changed+1))
    else
      note "stack compose file: $compose_path already matches ref $REF"
    fi
    rm -f "$FETCHED_TMP"
  else
    TRANSIENT_FAIL=1
  fi

  if [ "$LAYOUT" = "omv" ]; then
    ensure_symlink "${STACK_DIR}/compose.yml" "$COMPOSE_NAME"
    ensure_symlink "${STACK_DIR}/.env" "$ENV_NAME"
  fi
}

# step0b_secrets fills in ENV_FILE: the three prompted values (bot token,
# chat id, mailrise password) plus everything derivable without asking
# (JOURNAL_GID is left to the existing step6; the rest default here).
# Every field goes through set_env_default, which never overwrites a
# value already present — a re-run after a transient failure only fills
# in what is still missing, never re-prompts for what is already there.
step0b_secrets() {
  local existing_token existing_chat existing_smtp_user existing_smtp_pass
  existing_token="$(env_var_value "$ENV_FILE" TELEGRAM_BOT_TOKEN)"
  existing_chat="$(env_var_value "$ENV_FILE" TELEGRAM_CHAT_ID)"
  existing_smtp_user="$(env_var_value "$ENV_FILE" MAILRISE_SMTP_USER)"
  existing_smtp_pass="$(env_var_value "$ENV_FILE" MAILRISE_SMTP_PASS)"

  local token chat smtp_pass smtp_user
  require_secret TELEGRAM_BOT_TOKEN "Telegram bot token (from @BotFather): " "$existing_token" 1
  token="$REQUIRE_SECRET_RESULT"
  require_secret TELEGRAM_CHAT_ID "Telegram chat id: " "$existing_chat" 0
  chat="$REQUIRE_SECRET_RESULT"
  require_secret MAILRISE_SMTP_PASS "mailrise SMTP password (written into both $ENV_NAME and mailrise.conf): " "$existing_smtp_pass" 1
  smtp_pass="$REQUIRE_SECRET_RESULT"
  smtp_user="$existing_smtp_user"
  [ -z "$smtp_user" ] && smtp_user="sentinel"

  local before_changed=$changed
  set_env_default TELEGRAM_BOT_TOKEN "$token"
  set_env_default TELEGRAM_CHAT_ID "$chat"
  set_env_default MAILRISE_SMTP_USER "$smtp_user"
  set_env_default MAILRISE_SMTP_PASS "$smtp_pass"
  set_env_default APPRISE_BIND "127.0.0.1"
  set_env_default MAILRISE_BIND "127.0.0.1"
  set_env_default APPRISE_PUID "1000"
  set_env_default APPRISE_PGID "1000"
  set_env_default TZ "UTC"
  set_env_default SENTINEL_TAG "latest"
  set_env_default AGY_CREDENTIALS_DIR "$(invoking_home)/.gemini"
  set_env_default SENTINEL_MAIL_FROM "sentinel@mailrise.xyz"
  set_env_default SENTINEL_MAIL_TO "sentinel@mailrise.xyz"
  set_env_default LOG_LEVEL "INFO"
  if [ "$changed" -gt "$before_changed" ]; then
    note "stack env: $(verb_phrase "wrote" "would write") $((changed-before_changed)) field(s) to $ENV_FILE"
  elif [ "$MISSING_ENV_INPUT" -eq 1 ]; then
    note "stack env: $ENV_FILE unchanged — still missing the input reported above"
  else
    note "stack env: $ENV_FILE already had every field"
  fi

  if [ "$MISSING_ENV_INPUT" -eq 1 ]; then
    note "stack mailrise.conf: not written — still missing required input above"
    return
  fi
  if [ -z "$token" ] || [ -z "$chat" ] || [ -z "$smtp_user" ] || [ -z "$smtp_pass" ]; then
    # require_secret now sets MISSING_ENV_INPUT for every real-run path
    # that can return empty (no terminal, or an empty answer at one),
    # and the guard above already returned for that case — so on a real
    # run, reaching here with an empty value means only --check/--dry-run
    # skipped the prompt entirely, per require_secret's own contract.
    # mailrise.conf's content depends on all four, so there is nothing
    # meaningful to render yet beyond the note require_secret already
    # recorded for each missing one.
    note "stack mailrise.conf: not previewed — depends on values not yet set"
    return
  fi

  if fetch_repo_file "deploy/mailrise/mailrise.conf.example" "REPLACE_BOT_TOKEN"; then
    # replace_token, not sed and not bash's own "${tpl//pat/rep}": both
    # were measured to treat an unescaped '&' in the replacement text as
    # "the matched text", not literal data — sed's s/// documents this;
    # bash's own pattern substitution has the identical behavior far
    # less documented (reproduced directly: x="hello WORLD bye";
    # echo "${x//WORLD/A&B}" prints "hello AWORLDB bye"). sed's `/`
    # delimiter collision was the other half of the original bug: a
    # password containing one crashed sed outright and left a
    # ZERO-BYTE mailrise.conf that this script still reported as
    # "written". replace_token uses only prefix/suffix removal and
    # plain variable interpolation — no replacement-text concept at
    # all, so no character in a token/chat id/password can ever be
    # treated as special.
    local tpl
    tpl="$(cat "$FETCHED_TMP")"
    rm -f "$FETCHED_TMP"
    tpl="$(replace_token "$tpl" REPLACE_BOT_TOKEN "$token")"
    tpl="$(replace_token "$tpl" REPLACE_CHAT_ID "$chat")"
    tpl="$(replace_token "$tpl" REPLACE_SMTP_USER "$smtp_user")"
    tpl="$(replace_token "$tpl" REPLACE_SMTP_PASS "$smtp_pass")"

    local rendered
    rendered="$(mktemp)"
    printf '%s\n' "$tpl" > "$rendered"

    # Fail-closed regardless of substitution mechanism: a value that
    # itself contained the literal text "REPLACE_" (contrived, but
    # cheaper to reject than to reason about) or any other unexpected
    # substitution gap must never be written and reported as done —
    # that is a stack that "installs successfully" and cannot deliver a
    # single notification.
    if grep -q 'REPLACE_' "$rendered"; then
      echo "$PROG: rendered mailrise.conf still contains an unreplaced REPLACE_ token — refusing to write it" >&2
      TRANSIENT_FAIL=1
      rm -f "$rendered"
      return
    fi

    local mailrise_path="${STACK_DIR}/mailrise/mailrise.conf"
    if write_file_if_differs "$mailrise_path" 0644 "$rendered"; then
      note "stack mailrise.conf: $(verb_phrase "written" "would be written") (mode 0644 — see deploy/README.md, NOT 0600)"
      changed=$((changed+1))
    else
      note "stack mailrise.conf: already up to date"
    fi
    rm -f "$rendered"
  else
    TRANSIENT_FAIL=1
  fi
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
  note "step1 packages: $(verb_phrase "installing" "would install") ${need[*]}"
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
  note "step2 rasdaemon: $(verb_phrase "enabling and starting" "would enable and start")"
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
      note "step3 /etc/msmtprc: $(verb_phrase "collapsed ${RENDER_COLLAPSED} managed blocks into 1" "would collapse ${RENDER_COLLAPSED} managed blocks into 1")"
    else
      note "step3 /etc/msmtprc: $(verb_phrase "updated" "would be updated")"
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
      note "step4 /etc/smartd.conf: $(verb_phrase "collapsed ${RENDER_COLLAPSED} managed blocks into 1" "would collapse ${RENDER_COLLAPSED} managed blocks into 1")"
    else
      note "step4 /etc/smartd.conf: $(verb_phrase "updated" "would be updated")"
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
      note "step5 $file: $(verb_phrase "collapsed ${RENDER_COLLAPSED} managed blocks into 1" "would collapse ${RENDER_COLLAPSED} managed blocks into 1")"
    else
      note "step5 $file: $(verb_phrase "updated" "would be updated")"
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

  note "step6 JOURNAL_GID: $(verb_phrase "setting" "would set") to ${gid} in $ENV_FILE"
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
  # (JOURNAL_GID is the one field install.sh itself writes into it)
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

# --- apprise seed ----------------------------------------------------

# step_apprise_seed registers the Telegram target with apprise-api's
# own HTTP API — POST /add/KEY with urls=tgram://TOKEN/CHAT_ID — the
# same registration deploy/README.md documents and
# internal/notify/seed.go performs from inside the container; this is
# the host-side equivalent, reading straight from ENV_FILE rather than
# a rendered config file, since install.sh has no such file to render.
# Writing apprise/sentinel.cfg by hand does NOT register the key (see
# deploy/README.md) — POST is the only real registration, and this is
# the one step of this script that is not idempotent by "already
# converged" file comparison: apprise-api's own /add is itself an
# idempotent upsert, so re-POSTing identical urls on every run is
# correct rather than something to detect and skip.
#
# Deliberately does NOT run `docker compose up -d` itself. This script
# writes and validates config; the operator drives every container
# lifecycle action (CLAUDE.md — "the user drives all rollout actions").
# Seeding therefore happens on whichever run finds the stack already
# up: a run before that reports exactly what remains (install docker,
# `docker compose up -d`) and that re-running this script afterward is
# what completes it.
step_apprise_seed() {
  local bind key token chat endpoint
  bind="$(env_var_value "$ENV_FILE" APPRISE_BIND)"; [ -n "$bind" ] || bind="127.0.0.1"
  key="$(env_var_value "$ENV_FILE" APPRISE_KEY)"; [ -n "$key" ] || key="sentinel"
  token="$(env_var_value "$ENV_FILE" TELEGRAM_BOT_TOKEN)"
  chat="$(env_var_value "$ENV_FILE" TELEGRAM_CHAT_ID)"
  endpoint="http://${bind}:8000/add/${key}"

  if [ -z "$token" ] || [ -z "$chat" ]; then
    note "apprise seed: skipped — TELEGRAM_BOT_TOKEN/TELEGRAM_CHAT_ID not both set in $ENV_FILE yet"
    return
  fi

  if [ "$CHECK" -eq 1 ] || [ "$DRY_RUN" -eq 1 ]; then
    note "apprise seed: would register the Telegram target with apprise at $endpoint once the stack is up (nothing is sent under --check/--dry-run)"
    return
  fi

  if [ "$DOCKER_OK" -ne 1 ] || [ "$COMPOSE_OK" -ne 1 ]; then
    note "apprise seed: skipped — docker/compose not ready (see docker preflight above); install docker, run 'docker compose up -d' in the stack directory, then re-run this script to seed apprise"
    return
  fi

  # local declared separately from the assignment on purpose (as
  # fetch_repo_file already does above): `local status="$(curl ...)"`
  # would make `local` itself the last command in that statement, so
  # `curl_rc=$?` right after would capture local's exit status (always
  # 0) instead of curl's.
  local tmp status curl_rc
  tmp="$(mktemp)"
  # The token reaches curl only via stdin (--data-urlencode urls@-),
  # never as a command-line argument — argv is visible to any other
  # process on the host via ps; stdin is not. -f/--fail is deliberately
  # NOT used: it would make curl treat 204 (and any 4xx/5xx) as a
  # transport error indistinguishable from "apprise unreachable",
  # exactly the two failure modes this step must tell apart.
  status="$(printf '%s' "tgram://${token}/${chat}" | curl -sS --max-time 5 -o "$tmp" -w '%{http_code}' -X POST --data-urlencode urls@- "$endpoint" 2>/dev/null)"
  curl_rc=$?
  rm -f "$tmp"

  if [ "$curl_rc" -ne 0 ]; then
    # Not reachable is a normal intermediate state, not a fault: this
    # step runs before the operator has necessarily brought the stack
    # up (install.sh never does that itself). No TRANSIENT_FAIL here —
    # only the two branches below, where apprise DID answer and told
    # us the registration failed, do that.
    note "apprise seed: could not reach apprise at $endpoint (the stack may not be up yet) — run 'docker compose up -d' in the stack directory, then re-run this script"
    return
  fi
  # N.3.1's rule applies here too: a 204 from apprise-api means the key
  # was never registered, not that it succeeded quietly. Unlike
  # "unreachable" above, apprise DID answer here — this is a definite,
  # present failure of the primary notification path, not a stack
  # that merely isn't up yet, and reporting it via exit 0 would tell
  # anything reading $? that the run succeeded. TRANSIENT_FAIL (exit
  # 75) rather than a fresh fatal code: apprise can plausibly still be
  # mid-startup even though it's already answering HTTP (its own
  # config store not yet writable), and re-running this script is the
  # documented remedy either way — the same "retryable" contract exit
  # 75 already carries for every other step.
  if [ "$status" = "204" ]; then
    note "apprise seed: apprise responded 204 — the key was NOT registered (apprise-api's documented silent-failure response); notifications will not be delivered until this is fixed"
    TRANSIENT_FAIL=1
    return
  fi
  case "$status" in
    2[0-9][0-9])
      note "apprise seed: registered the Telegram target with apprise (key=$key)"
      changed=$((changed+1))
      ;;
    *)
      note "apprise seed: apprise responded HTTP $status registering the Telegram target — notifications will not be delivered until this is fixed"
      TRANSIENT_FAIL=1
      ;;
  esac
}

# --- run -------------------------------------------------------------

# Before any host mutation (R5): a docker/compose gap does not stop
# this run, but every later note about the stack's readiness depends
# on knowing about it first.
docker_preflight

if [ "$ENV_FILE_EXPLICIT" -ne 1 ]; then
  step0a_layout
  # STACK_UNRESOLVED (set only by resolve_stack_dir's ambiguous
  # --check/--dry-run branch) means ENV_FILE is still whatever it
  # defaulted to (./.env) rather than a real stack's env file — running
  # step0b_secrets here would preview "would write 11 field(s) to
  # ./.env", a plan against a file the ambiguity report already said
  # this run is not proceeding with. Skipped, not silently: the
  # ambiguity note is already in the summary, this just keeps the rest
  # of it from reading like a coherent plan around that one line.
  if [ "$STACK_UNRESOLVED" -ne 1 ]; then
    step0b_secrets
  else
    note "stack env: skipped — no stack directory was resolved (see the ambiguity report above)"
  fi
fi

step1
# Must run after step1 (needs the real RASDAEMON_OK/MSMTP_OK it sets)
# and before step3/4/5 (which all gate on the MAIL_OK it computes).
compute_mail_status
step2
step3
step4
step5
# Same STACK_UNRESOLVED gate as step0b_secrets above, for the same
# reason: step6 upserts JOURNAL_GID into ENV_FILE, and with no stack
# directory resolved that is still the unrelated ./.env default.
if [ "$STACK_UNRESOLVED" -ne 1 ]; then
  step6
else
  note "step6 JOURNAL_GID: skipped — no stack directory was resolved (see the ambiguity report above)"
fi

# Same STACK_UNRESOLVED gate: with no stack directory resolved, ENV_FILE
# is still the unrelated ./.env default and there is no coherent
# TELEGRAM_* pair to read.
if [ "$STACK_UNRESOLVED" -ne 1 ]; then
  step_apprise_seed
else
  note "apprise seed: skipped — no stack directory was resolved (see the ambiguity report above)"
fi

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
