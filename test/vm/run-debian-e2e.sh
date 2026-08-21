#!/bin/bash
# run-debian-e2e.sh WORKDIR
#
# Variant 1: plain Debian trixie, cloud-init only, no OMV. The ordinary
# path install.sh must also get right: no mail transport agent present (so
# msmtp-mta is a legitimate, necessary install, not a conflict), no
# platform config markers (so the interactive monitoring prompt is
# reachable), plain stack layout. Cheap and disposable by design, this
# variant caches nothing beyond the base cloud image download (see
# fetch-debian-cloud-image.sh), so it stays fast enough to run on every
# push even when the OMV variant does not.
set -eu
set -o pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$here/lib.sh"

WORKDIR="${1:?usage: run-debian-e2e.sh WORKDIR}"
REPO_ROOT="$(cd "$here/../.." && pwd)"
mkdir -p "$WORKDIR"

accel="$(vm_detect_accel)"
vm_gen_ssh_key "$WORKDIR/ssh"
pubkey="$(cat "$WORKDIR/ssh/id_ed25519.pub")"
sed "s#__SSH_PUBKEY__#$pubkey#" "$here/cloud-init-user-data.tmpl" > "$WORKDIR/user-data"
cp "$here/cloud-init-meta-data" "$WORKDIR/meta-data"
vm_make_seed_iso "$WORKDIR/user-data" "$WORKDIR/meta-data" "$WORKDIR/seed.iso"

bash "$here/fetch-debian-cloud-image.sh" "$WORKDIR/debian-base.qcow2"
# Copy-on-write overlay, never boot the downloaded base directly: keeps the
# base reusable if this script is ever asked to run more than once against
# the same download, and matches the discipline the OMV variant uses for
# the same reason.
qemu-img create -f qcow2 -b "$WORKDIR/debian-base.qcow2" -F qcow2 "$WORKDIR/run.qcow2"
qemu-img resize "$WORKDIR/run.qcow2" 8G

vm_boot "$WORKDIR/run.qcow2" "$WORKDIR/seed.iso" 2222 \
        "$WORKDIR/qemu.pid" "$WORKDIR/serial.log" "$accel"
trap 'vm_stop "$WORKDIR/qemu.pid"' EXIT

if ! vm_wait_ssh 2222 "$WORKDIR/ssh/id_ed25519" e2e 240; then
  vm_log "run-debian-e2e: VM never came up, serial console follows:"
  tail -n 40 "$WORKDIR/serial.log" >&2 || true
  exit 1
fi

vm_scp_repo 2222 "$WORKDIR/ssh/id_ed25519" e2e "$REPO_ROOT" /home/e2e/repo
vm_ssh 2222 "$WORKDIR/ssh/id_ed25519" e2e "chmod +x /home/e2e/repo/install.sh"

vm_run_install_checks 2222 "$WORKDIR/ssh/id_ed25519" e2e /home/e2e/repo \
  --stack-dir /home/e2e/sentinel-stack

vm_log "run-debian-e2e: claimed-file assertions"
# One representative file per install.sh step, mode as R5 specifies:
# msmtprc is the sharpest (0600, the whole containment for a cleartext
# credential per R5's own reasoning), the stack's .env is the second
# credential-bearing file this run itself created.
vm_ssh 2222 "$WORKDIR/ssh/id_ed25519" e2e "stat -c '%a %U:%G' /etc/msmtprc" | grep -qx '600 root:root' \
  || { vm_log "FAIL: /etc/msmtprc is not 600 root:root"; exit 1; }
vm_ssh 2222 "$WORKDIR/ssh/id_ed25519" e2e "test -f /home/e2e/sentinel-stack/docker-compose.yml" \
  || { vm_log "FAIL: docker-compose.yml was not written into the stack dir"; exit 1; }

vm_log "run-debian-e2e: PASS"
