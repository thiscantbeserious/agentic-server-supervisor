# lib.sh, sourced by every script under test/vm/. Shared boot/ssh/assertion
# helpers for the two VM E2E jobs (R6). Not a standalone script, has no
# shebang and is never executed directly.
#
# Kept out of Go and out of container_test.go on purpose: these jobs boot a
# real kernel under QEMU, something neither `go test` nor a container build
# can do, and that is the entire point, see contracts/runtime.md R6 for why
# a container smoke test cannot substitute for this.
set -u
set -o pipefail

vm_log() { printf '[vm] %s\n' "$*" >&2; }

# vm_detect_accel: prints "kvm" or "tcg" on stdout, logs which and why on
# stderr. /dev/kvm on a GitHub-hosted runner is present on some runner
# generations and absent on others, and that is explicitly not something
# this project treats as a reason to skip, a slow TCG run is a real signal,
# a skipped job is silence.
vm_detect_accel() {
  if [ -e /dev/kvm ]; then
    sudo chmod 666 /dev/kvm 2>/dev/null || true
    if [ -r /dev/kvm ] && [ -w /dev/kvm ]; then
      vm_log "accel: kvm (/dev/kvm present and accessible)"
      printf 'kvm\n'
      return 0
    fi
  fi
  vm_log "accel: tcg (/dev/kvm absent or inaccessible on this runner, falling back to software emulation, expect a slower boot)"
  printf 'tcg\n'
}

# vm_gen_ssh_key DIR: writes DIR/id_ed25519(.pub), 0600 on the private half.
vm_gen_ssh_key() {
  dir="$1"
  mkdir -p "$dir"
  ssh-keygen -q -t ed25519 -N '' -f "$dir/id_ed25519"
}

# vm_make_seed_iso USERDATA METADATA OUT.ISO
vm_make_seed_iso() {
  userdata="$1" metadata="$2" out="$3"
  genisoimage -output "$out" -volid cidata -joliet -rock "$userdata" "$metadata" >/dev/null
}

# vm_boot DISK SEED_ISO PORT PIDFILE SERIALLOG ACCEL [EXTRA_QEMU_ARGS...]
# SEED_ISO may be the empty string (no cloud-init CD-ROM attached, used when
# booting an already-provisioned image that no longer needs first-boot setup).
vm_boot() {
  disk="$1" seed="$2" port="$3" pidfile="$4" seriallog="$5" accel="$6"
  shift 6
  args=(-name vm-e2e -m 2560 -smp 2 -accel "$accel" -cpu max
        -drive "file=$disk,if=virtio,format=qcow2"
        # Named explicitly rather than "-nic user,hostfwd=...": that form
        # lets qemu pick the NIC model, which depends on the machine type
        # and qemu version rather than on what the guest actually has a
        # driver for. genericcloud images are built expecting virtio-net;
        # the host-side forward was proven listening and accepting
        # connections while the guest never answered, "no working NIC
        # driver" fits that exactly. Naming the device removes the guess.
        -netdev "user,id=net0,hostfwd=tcp::${port}-:22"
        -device virtio-net-pci,netdev=net0
        -serial "file:$seriallog" -monitor none -display none
        -daemonize -pidfile "$pidfile")
  if [ -n "$seed" ]; then
    # media=cdrom, not if=virtio: a NoCloud seed is conventionally a
    # CD-ROM, and cloud images are built and tested against exactly that
    # datasource path. A virtio block device carrying an ISO9660
    # filesystem is something cloud-init CAN in principle recognise by
    # volume label, but it is not the path genericcloud images are
    # verified against, and got this VM a healthy, unreachable boot: no
    # datasource claimed, no user created, no key installed, login prompt
    # up, nobody able to answer it.
    args+=(-drive "file=$seed,media=cdrom")
  fi
  # -daemonize forks to the background AFTER startup, it does not stop the
  # foreground qemu process from inheriting this shell's stdin in the
  # meantime. In CI stdin is already at EOF (no terminal), and with no
  # -serial/-monitor target explicitly named neither one takes it, so the
  # guest's getty asking for input hit that EOF and qemu exited the instant
  # the login prompt appeared: full boot, no time for vm_wait_ssh to ever
  # run. -serial file:... and -monitor none above remove both consumers of
  # stdio; the stdin redirect below is a second, independent guard so a
  # future flag added here cannot silently reopen the same failure.
  qemu-system-x86_64 "${args[@]}" "$@" < /dev/null
}

# vm_dump_boot_diagnosis SERIALLOG PIDFILE PORT: prints the host+guest
# state of an unreachable-SSH failure, everything that does NOT depend on
# which auth method the caller uses (the actual ssh attempt is the
# caller's own one line, key-based for most callers, sshpass for the OMV
# variant's password account, see run-omv-e2e.sh).
#
# The guest half: a plain tail of the end of boot shows a healthy login
# prompt and nothing else, the login prompt is not the failure, datasource
# detection and DHCP happen early, well before the end of a short tail.
# The host half (added after a run where the guest side was already
# proven healthy, cloud-init claimed the datasource, created the user,
# started sshd, yet SSH still never connected): whether qemu is even
# still running, and whether the forwarded port is listening on the host
# at all, "nothing listening" and "listening but refusing" are different
# bugs the caller's own ssh attempt distinguishes right after this.
vm_dump_boot_diagnosis() {
  seriallog="$1" pidfile="$2" port="$3"

  vm_log "cloud-init datasource / network / user-creation lines:"
  grep -iE 'cloud-init|datasource|ds-identify|useradd|authorized_keys|nocloud|dhcp|dhcp4|link becomes ready|eth0|console-netinfo' "$seriallog" >&2 \
    || vm_log "(none found, cloud-init may never have run at all)"
  vm_log "last 120 lines of $seriallog:"
  tail -n 120 "$seriallog" >&2 || true

  vm_log "host side: qemu process"
  if [ -f "$pidfile" ] && kill -0 "$(cat "$pidfile")" 2>/dev/null; then
    vm_log "qemu (pid $(cat "$pidfile")) is still running"
  else
    vm_log "qemu is NOT running, it exited before the wait gave up"
  fi

  vm_log "host side: is anything listening on 127.0.0.1:$port"
  ss -ltnp 2>/dev/null | grep -E "[:.]$port\b" >&2 || vm_log "(nothing listening on port $port)"
}

vm_stop() {
  pidfile="$1"
  [ -f "$pidfile" ] || return 0
  pid="$(cat "$pidfile")"
  kill "$pid" 2>/dev/null || true
  for _ in $(seq 1 20); do
    kill -0 "$pid" 2>/dev/null || return 0
    sleep 1
  done
  kill -9 "$pid" 2>/dev/null || true
}

# vm_on_exit_keyauth WORKDIR PORT KEY USER: the EXIT trap body shared by the
# two key-authenticated callers (run-debian-e2e.sh, build-omv-image.sh).
# Diagnostics used to run only on the explicit "VM never came up" branch,
# so a failure anywhere later (install.sh itself, an assertion after SSH
# was already up) left every diagnostic silent, exactly the case that hid
# both this bug and, before it, the exit-64 usage error. An EXIT trap
# covers every path out of the script, not one branch of it, code $?
# must be read as this function's first statement, it is the exit status
# that triggered the trap and is gone the instant anything else runs.
vm_on_exit_keyauth() {
  code=$?
  workdir="$1" port="$2" key="$3" user="$4"
  if [ "$code" -ne 0 ]; then
    vm_log "run failed (exit $code), dumping diagnostics"
    vm_dump_boot_diagnosis "$workdir/serial.log" "$workdir/qemu.pid" "$port"
    vm_log "host side: one real ssh attempt, verbose, stderr not swallowed"
    ssh -v -i "$key" -p "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=10 -o BatchMode=yes "$user@127.0.0.1" true 2>&1 | tail -n 30 >&2 || true
  fi
  vm_stop "$workdir/qemu.pid"
}

# vm_install_exit_trap WORKDIR PORT KEY USER: registers vm_on_exit_keyauth
# for the whole rest of the script. Values are expanded now, not deferred
# to when the trap fires, since this function's own locals ($workdir etc.)
# do not exist any more by then.
vm_install_exit_trap() {
  # shellcheck disable=SC2064
  trap "vm_on_exit_keyauth '$1' '$2' '$3' '$4'" EXIT
}

# vm_wait_ssh PORT KEY USER TIMEOUT_S
vm_wait_ssh() {
  port="$1" key="$2" user="$3" timeout="${4:-240}"
  deadline=$((SECONDS + timeout))
  while [ "$SECONDS" -lt "$deadline" ]; do
    if ssh -i "$key" -p "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
           -o ConnectTimeout=5 -o BatchMode=yes "$user@127.0.0.1" true 2>/dev/null; then
      vm_log "ssh up on port $port after $((SECONDS - (deadline - timeout))) s"
      return 0
    fi
    sleep 3
  done
  return 1
}

vm_ssh() {
  port="$1" key="$2" user="$3"; shift 3
  ssh -i "$key" -p "$port" -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o ConnectTimeout=10 "$user@127.0.0.1" "$@"
}

vm_scp_repo() {
  port="$1" key="$2" user="$3" src="$4" dest="$5"
  tar -C "$src" --exclude=.git -cf - . | \
    vm_ssh "$port" "$key" "$user" "mkdir -p '$dest' && tar -C '$dest' -xf -"
}

# vm_dpkg_snapshot PORT KEY USER: prints "name version" per installed
# package, sorted, on stdout. Used before/after install.sh to prove the
# package database was not touched (the removal cascade this job exists
# to catch shows up here first, before any application-level symptom).
vm_dpkg_snapshot() {
  port="$1" key="$2" user="$3"
  vm_ssh "$port" "$key" "$user" "dpkg-query -W -f '\${Package} \${Version}\n'" | sort
}

# vm_assert_no_removed BEFORE_FILE AFTER_FILE: fails (prints to stderr,
# returns 1) if any package present before is absent after. New packages
# are fine and expected (msmtp-mta, sentinel's own dependencies); a
# disappearance is exactly the OMV/postfix cascade this job exists to catch.
vm_assert_no_removed() {
  before="$1" after="$2"
  removed="$(comm -23 <(cut -d' ' -f1 "$before") <(cut -d' ' -f1 "$after"))"
  if [ -n "$removed" ]; then
    vm_log "packages removed by install.sh (this must never happen):"
    printf '%s\n' "$removed" >&2
    return 1
  fi
  vm_log "package removal check: clean, nothing removed"
}

# vm_write_env_file PORT KEY USER REMOTE_PATH: writes a fully-populated
# .env on the VM so install.sh's three require_secret prompts (Telegram
# token/chat id, mailrise password) are all pre-answered from the file and
# never reach /dev/tty. What SSH leaves genuinely absent is a controlling
# terminal for the SEPARATE confirm_monitoring_change [y/N] prompt (R5),
# which has no --env-file answer by design, exercising exactly the
# documented "no controlling terminal -> default applied, stated
# explicitly" path rather than working around it.
vm_write_env_file() {
  port="$1" key="$2" user="$3" remote_path="$4"
  vm_ssh "$port" "$key" "$user" "cat > '$remote_path'" <<'ENV'
TELEGRAM_BOT_TOKEN=123456789:vm-e2e-not-a-real-token
TELEGRAM_CHAT_ID=123456789
MAILRISE_SMTP_USER=vm-e2e
MAILRISE_SMTP_PASS=vm-e2e-password-not-a-real-secret
APPRISE_BIND=127.0.0.1
MAILRISE_BIND=127.0.0.1
APPRISE_PUID=1000
APPRISE_PGID=1000
TZ=UTC
ENV
}

# vm_run_install_checks PORT KEY USER REMOTE_REPO_DIR --stack-dir PATH [MORE INSTALL ARGS...]
#
# Deliberately tests the --stack-dir path, never --env-file: --stack-dir is
# what the documented `curl | sudo bash` flow actually runs (create the
# stack, fetch the compose file, prompt for secrets, write them);
# --env-file is the older "I already filled in a file myself" path, and a
# suite that only exercised that one would never test stack creation,
# most of what the installer does today. The two are mutually exclusive
# by install.sh's own design (one names a file to use as-is, the other
# names a directory to create and populate), passing both is a usage
# error, not a corner case, this function used to append --env-file to
# every call regardless of what the caller already passed, which is
# exactly the exit-64 "mutually exclusive" usage error this comment now
# prevents from recurring.
#
# --stack-dir with no controlling terminal exits 78 rather than assume
# consent for a missing secret (by design, R5), so the three secrets are
# pre-seeded into the stack's own env file BEFORE the first run, the
# same place install.sh would have written them after prompting, just
# arriving there without a prompt to answer. Assumes PATH is a plain
# (non-OMV) location, so install.sh's own layout detection resolves
# ENV_NAME to plain ".env"; every caller today points PATH at an
# ordinary directory for exactly this reason. A caller testing an actual
# OMV compose root would need to seed "sentinel.env" instead.
#
# The assertions R6 says both variants share: install.sh reaches exit 0
# (never yet achieved anywhere, see the design note this function's
# callers carry), a second run converges at changed=0, a following
# --check exits 0, and nothing was removed from the package database.
# File-creation-with-claimed-mode and the OMV-specific assertions (removal
# cascade beyond the generic package diff, config markers, compose root)
# are the caller's job, they need per-variant paths and expectations this
# function has no business knowing.
vm_run_install_checks() {
  port="$1" key="$2" user="$3" repo="$4"; shift 4

  stack_dir="" prev=""
  for a in "$@"; do
    [ "$prev" = "--stack-dir" ] && stack_dir="$a"
    prev="$a"
  done
  if [ -z "$stack_dir" ]; then
    vm_log "vm_run_install_checks: no --stack-dir in install args ($*), this helper only tests that mode"
    return 1
  fi

  vm_ssh "$port" "$key" "$user" "mkdir -p '$stack_dir'"
  vm_write_env_file "$port" "$key" "$user" "$stack_dir/.env"
  before="$(mktemp)"; after="$(mktemp)"
  vm_dpkg_snapshot "$port" "$key" "$user" > "$before"

  cmd="cd '$repo' && sudo bash ./install.sh $*"
  vm_log "install.sh run 1: $cmd"
  # `out1="$(cmd)"; code1=$?` looks right but is not, under `set -e` (every
  # caller of this function has it) the assignment itself is the simple
  # command `-e` checks: a nonzero exit from the substitution ends the
  # script on that line, before `code1=$?` or the printf below it ever
  # run. install.sh's exit-64 usage error was captured correctly (2>&1 is
  # inside the remote command, ssh's own stdout has it) and then never
  # printed, look identical to output being lost. `if var=$(cmd); then...`
  # is exempt from -e (a command tested by `if` never triggers it), so the
  # exit code and the captured text both survive to be reported.
  if out1="$(vm_ssh "$port" "$key" "$user" "$cmd 2>&1")"; then
    code1=0
  else
    code1=$?
  fi
  printf '%s\n' "$out1"
  if [ "$code1" -ne 0 ]; then
    vm_log "FAIL: install.sh run 1 exit $code1, want 0 (this is the headline assertion these jobs exist for)"
    return 1
  fi

  vm_dpkg_snapshot "$port" "$key" "$user" > "$after"
  vm_assert_no_removed "$before" "$after" || return 1

  vm_log "install.sh run 2 (idempotency): expect changed=0"
  if out2="$(vm_ssh "$port" "$key" "$user" "$cmd 2>&1")"; then
    code2=0
  else
    code2=$?
  fi
  printf '%s\n' "$out2"
  if [ "$code2" -ne 0 ]; then
    vm_log "FAIL: install.sh run 2 exit $code2, want 0"
    return 1
  fi
  if ! printf '%s\n' "$out2" | grep -q 'changed=0'; then
    vm_log "FAIL: install.sh run 2 did not report changed=0 (second run must converge)"
    return 1
  fi

  check_cmd="cd '$repo' && sudo bash ./install.sh $* --check"
  vm_log "install.sh --check after convergence: $check_cmd"
  if vm_ssh "$port" "$key" "$user" "$check_cmd"; then
    code3=0
  else
    code3=$?
  fi
  if [ "$code3" -ne 0 ]; then
    vm_log "FAIL: install.sh --check exit $code3, want 0"
    return 1
  fi

  vm_log "shared install.sh assertions: all passed"
}

# vm_content_hash FILE...: sha256 over the concatenation of every argument,
# files and literal strings alike, truncated to 16 hex chars for a usable
# image tag. Every input that determines what ends up on the provisioned
# disk must be listed by the caller, this function has no opinion on which.
vm_content_hash() {
  cat "$@" | sha256sum | cut -c1-16
}
