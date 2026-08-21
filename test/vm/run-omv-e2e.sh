#!/bin/bash
# run-omv-e2e.sh BASE_QCOW2 WORKDIR
#
# Variant 2: boots a disposable copy-on-write overlay of the cached,
# already-provisioned OMV base image (BASE_QCOW2, produced once by
# build-omv-image.sh and pulled from GHCR by the caller, see the
# vm-e2e-omv job in .github/workflows/ci.yml) and runs install.sh against
# a real OMV host. This is the variant that exists to catch what no
# fixture can: the removal cascade, the platform config markers, and
# compose-root detection against a real, absolute-symlink layout.
set -eu
set -o pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$here/lib.sh"

BASE_QCOW2="${1:?usage: run-omv-e2e.sh BASE_QCOW2 WORKDIR}"
WORKDIR="${2:?usage: run-omv-e2e.sh BASE_QCOW2 WORKDIR}"
REPO_ROOT="$(cd "$here/../.." && pwd)"
mkdir -p "$WORKDIR"

accel="$(vm_detect_accel)"
vm_gen_ssh_key "$WORKDIR/ssh"

# Copy-on-write overlay: BASE_QCOW2 is never opened for writing. A run
# cannot contaminate the pulled image, which is what makes it safe to
# share across every branch and every future run.
qemu-img create -f qcow2 -b "$BASE_QCOW2" -F qcow2 "$WORKDIR/run.qcow2"

vm_boot "$WORKDIR/run.qcow2" "" 2224 "$WORKDIR/qemu.pid" "$WORKDIR/serial.log" "$accel"

# Covers every exit path, not just the wait-for-ssh timeout below: an
# install.sh (or assertion) failure discovered well after SSH came up
# used to leave every diagnostic silent, only the wait-timeout branch
# ever called one. Not lib.sh's vm_on_exit_keyauth: this variant
# authenticates with sshpass against a throwaway password account, no
# key, see the comment below for why.
vm_on_exit_omv() {
  code=$?
  if [ "$code" -ne 0 ]; then
    vm_log "run failed (exit $code), dumping diagnostics"
    vm_dump_boot_diagnosis "$WORKDIR/serial.log" "$WORKDIR/qemu.pid" 2224
    vm_log "host side: one real ssh attempt, verbose, stderr not swallowed"
    sshpass -p e2e ssh -v -p 2224 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=10 e2e@127.0.0.1 true 2>&1 | tail -n 30 >&2 || true
  fi
  vm_stop "$WORKDIR/qemu.pid"
}
trap vm_on_exit_omv EXIT

# No cloud-init CD-ROM here: the SSH key is already the one baked into the
# base image at build time (build-omv-image.sh provisions with the SAME
# lib.sh, but that key lives only in that job's own WORKDIR, gone by the
# time this job runs). So this script authorizes its own fresh key by
# writing it straight into the still-offline overlay's filesystem with
# virt-customize-free means: mount is unavailable without extra tooling on
# a stock runner, so instead the base image's provisioning step
# (provision-omv.sh) leaves password auth enabled for a throwaway
# "e2e"/"e2e" account for this narrow purpose, root only reachable via
# sudo, never exposed past this disposable VM's lifetime.
deadline=$((SECONDS + 240))
up=0
while [ "$SECONDS" -lt "$deadline" ]; do
  if sshpass -p e2e ssh -p 2224 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
        -o ConnectTimeout=5 e2e@127.0.0.1 true 2>/dev/null; then
    up=1
    break
  fi
  sleep 3
done
if [ "$up" != 1 ]; then
  vm_log "run-omv-e2e: VM never came up (or came up unreachable)"
  exit 1
fi
ssh_pw() { sshpass -p e2e ssh -p 2224 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=10 e2e@127.0.0.1 "$@"; }
scp_pw_repo() {
  tar -C "$REPO_ROOT" --exclude=.git -cf - . | \
    sshpass -p e2e ssh -p 2224 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null e2e@127.0.0.1 \
      "mkdir -p /home/e2e/repo && tar -C /home/e2e/repo -xf -"
}

vm_log "run-omv-e2e: verifying the pulled image is what its tag claims"
# The one assertion R6 requires beyond the shared install.sh checks below:
# if a cached image were ever wrong (built from a different provisioning
# script than the tag implies, or corrupted in transit), this fails loudly
# here rather than the run silently exercising the wrong OMV version.
# ${Package} rather than the names written into the format: dpkg-query
# applies its format once per package, so a two-line format queried for two
# packages emits four lines, each package's version once under every label.
# That compared as a mismatch against a correct image and reported it as
# cache corruption, which is a far more alarming thing than it was.
if ! ssh_pw "diff <(dpkg-query -W -f='\${Package} \${Version}\n' openmediavault postfix) /etc/vm-e2e-expected-versions"; then
  vm_log "FAIL: pulled/built OMV base image does not match its own recorded provisioning versions, cache content mismatch"
  exit 1
fi

scp_pw_repo
ssh_pw "chmod +x /home/e2e/repo/install.sh"

vm_log "run-omv-e2e: compose-root auto-detection against the real OMV layout"
# `--dry-run` is expected to exit 0, but "expected" is not "guaranteed",
# and under `set -e` an `out="$(cmd)"` assignment that fails aborts the
# script on that line, before printf ever runs, exactly the bug that hid
# install.sh's own exit-64 usage output earlier. `|| true` keeps the
# captured text regardless of exit status; the grep right below is the
# actual assertion, not this command's exit code.
detect_out="$(ssh_pw "cd /home/e2e/repo && sudo bash ./install.sh --dry-run 2>&1")" || true
printf '%s\n' "$detect_out"
if ! printf '%s\n' "$detect_out" | grep -q 'layout: omv'; then
  vm_log "FAIL: install.sh did not detect the real OMV compose layout (absolute compose.yml symlink from openmediavault-compose), this is the sharpest defect this job exists to catch"
  exit 1
fi

vm_log "run-omv-e2e: shared install.sh assertions"
before_smartd="$(ssh_pw "cat /etc/smartd.conf")"
before_plugins="$(ssh_pw "dpkg-query -W -f='\${Package}\n' 'openmediavault-*' | sort")"

# vm_run_install_checks needs vm_ssh's key-based SSH; the base image
# only carries the throwaway password account, so this variant's checks
# are re-run inline against ssh_pw rather than sharing that helper. Same
# mode decision as the Debian variant and for the same reason: --stack-dir
# is what the real `curl | sudo bash` flow runs, --env-file alone would
# never exercise stack creation. The stack dir is an ordinary path, not
# an OMV compose root (that layout is already exercised separately, just
# above, via --dry-run's auto-detection), so install.sh resolves plain
# ".env" here, matching what this function seeds.
STACK_DIR_PW=/home/e2e/sentinel-stack
vm_write_env_file_pw() {
  ssh_pw "mkdir -p $STACK_DIR_PW && cat > $STACK_DIR_PW/.env" <<'ENV'
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

vm_run_install_checks_over_pw() {
  vm_write_env_file_pw
  before="$(mktemp)"; after="$(mktemp)"
  ssh_pw "dpkg-query -W -f='\${Package} \${Version}\n'" | sort > "$before"

  install_cmd="cd /home/e2e/repo && sudo bash ./install.sh --stack-dir $STACK_DIR_PW"
  vm_log "install.sh run 1: $install_cmd"
  # Same `set -e` trap as lib.sh's vm_run_install_checks (this function
  # exists only because that one needs key-based ssh, not because the
  # bug is different): `out="$(cmd)"; code=$?` under `set -e` never
  # reaches the `code=$?` line on a nonzero exit, the assignment itself
  # is what -e checks. `if out=$(cmd); then code=0; else code=$?; fi` is
  # exempt.
  if out1="$(ssh_pw "$install_cmd 2>&1")"; then
    code1=0
  else
    code1=$?
  fi
  printf '%s\n' "$out1"
  [ "$code1" -eq 0 ] || { vm_log "FAIL: install.sh run 1 exit $code1, want 0"; return 1; }

  ssh_pw "dpkg-query -W -f='\${Package} \${Version}\n'" | sort > "$after"
  vm_assert_no_removed "$before" "$after" || return 1

  if out2="$(ssh_pw "$install_cmd 2>&1")"; then
    code2=0
  else
    code2=$?
  fi
  printf '%s\n' "$out2"
  [ "$code2" -eq 0 ] || { vm_log "FAIL: install.sh run 2 exit $code2, want 0"; return 1; }
  printf '%s\n' "$out2" | grep -q 'changed=0' || { vm_log "FAIL: run 2 did not converge to changed=0"; return 1; }

  # Captured with 2>&1, same as run 1/run 2 above: a bare exit code says
  # nothing about what --check actually objected to, its own
  # stdout/stderr names the drift it found.
  if out3="$(ssh_pw "$install_cmd --check 2>&1")"; then
    code3=0
  else
    code3=$?
  fi
  printf '%s\n' "$out3"
  [ "$code3" -eq 0 ] || { vm_log "FAIL: install.sh --check exit $code3, want 0"; return 1; }
}
vm_run_install_checks_over_pw

vm_log "run-omv-e2e: removal-cascade assertion (postfix, OMV plugin set)"
ssh_pw "dpkg-query -W postfix" >/dev/null \
  || { vm_log "FAIL: postfix was removed, the exact cascade this job exists to catch (msmtp-mta vs postfix conflict resolved by removing OMV's own dependency)"; exit 1; }
after_plugins="$(ssh_pw "dpkg-query -W -f='\${Package}\n' 'openmediavault-*' | sort")"
if [ "$before_plugins" != "$after_plugins" ]; then
  vm_log "FAIL: OMV plugin set changed"
  diff <(printf '%s\n' "$before_plugins") <(printf '%s\n' "$after_plugins") >&2 || true
  exit 1
fi

vm_log "run-omv-e2e: platform config marker assertion (smartd.conf, zed.rc)"
after_smartd="$(ssh_pw "cat /etc/smartd.conf")"
if [ "$before_smartd" != "$after_smartd" ]; then
  vm_log "FAIL: /etc/smartd.conf changed even though it carries OMV's auto-generated marker, step4 must skip a marked file entirely"
  diff <(printf '%s\n' "$before_smartd") <(printf '%s\n' "$after_smartd") >&2 || true
  exit 1
fi
if ! printf '%s\n' "$after_smartd" | grep -q 'auto-generated by openmediavault'; then
  vm_log "FAIL: OMV's own marker is gone from /etc/smartd.conf, the base image no longer represents a real OMV host"
  exit 1
fi

vm_log "run-omv-e2e: PASS"
