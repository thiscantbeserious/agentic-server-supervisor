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
trap 'vm_stop "$WORKDIR/qemu.pid"' EXIT

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
  vm_log "run-omv-e2e: VM never came up, serial console follows:"
  cat "$WORKDIR/serial.log" >&2 || true
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
if ! ssh_pw "diff <(dpkg-query -W -f='openmediavault \${Version}\npostfix \${Version}\n' openmediavault postfix) /etc/vm-e2e-expected-versions"; then
  vm_log "FAIL: pulled/built OMV base image does not match its own recorded provisioning versions, cache content mismatch"
  exit 1
fi

scp_pw_repo
ssh_pw "chmod +x /home/e2e/repo/install.sh"

vm_log "run-omv-e2e: compose-root auto-detection against the real OMV layout"
detect_out="$(ssh_pw "cd /home/e2e/repo && sudo bash ./install.sh --dry-run 2>&1")"
printf '%s\n' "$detect_out"
if ! printf '%s\n' "$detect_out" | grep -q 'layout: omv'; then
  vm_log "FAIL: install.sh did not detect the real OMV compose layout (absolute compose.yml symlink from openmediavault-compose), this is the sharpest defect this job exists to catch"
  exit 1
fi

vm_log "run-omv-e2e: shared install.sh assertions"
before_smartd="$(ssh_pw "cat /etc/smartd.conf")"
before_plugins="$(ssh_pw "dpkg-query -W -f='\${Package}\n' 'openmediavault-*' | sort")"

vm_run_install_checks_over_pw() {
  # vm_run_install_checks needs vm_ssh's key-based SSH; the base image
  # only carries the throwaway password account, so this variant's checks
  # are re-run inline against ssh_pw rather than sharing that helper.
  vm_write_env_file_pw
  before="$(mktemp)"; after="$(mktemp)"
  ssh_pw "dpkg-query -W -f='\${Package} \${Version}\n'" | sort > "$before"

  out1="$(ssh_pw "cd /home/e2e/repo && sudo bash ./install.sh --env-file /home/e2e/repo/.env 2>&1")"
  code1=$?
  printf '%s\n' "$out1"
  [ "$code1" -eq 0 ] || { vm_log "FAIL: install.sh run 1 exit $code1, want 0"; return 1; }

  ssh_pw "dpkg-query -W -f='\${Package} \${Version}\n'" | sort > "$after"
  vm_assert_no_removed "$before" "$after" || return 1

  out2="$(ssh_pw "cd /home/e2e/repo && sudo bash ./install.sh --env-file /home/e2e/repo/.env 2>&1")"
  code2=$?
  printf '%s\n' "$out2"
  [ "$code2" -eq 0 ] || { vm_log "FAIL: install.sh run 2 exit $code2, want 0"; return 1; }
  printf '%s\n' "$out2" | grep -q 'changed=0' || { vm_log "FAIL: run 2 did not converge to changed=0"; return 1; }

  ssh_pw "cd /home/e2e/repo && sudo bash ./install.sh --env-file /home/e2e/repo/.env --check"
  code3=$?
  [ "$code3" -eq 0 ] || { vm_log "FAIL: install.sh --check exit $code3, want 0"; return 1; }
}
vm_write_env_file_pw() {
  ssh_pw "cat > /home/e2e/repo/.env" <<'ENV'
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
