#!/bin/bash
# build-omv-image.sh OUT_QCOW2 WORKDIR
#
# Boots a fresh Debian image, runs provision-omv.sh against it as root over
# SSH, shuts it down cleanly, and leaves the resulting disk at OUT_QCOW2.
# This is the expensive path (a full OMV install), run only on a cache
# miss: see the "vm-omv-image" job in .github/workflows/ci.yml, which is
# the only job that calls this script, and the only VM job holding
# packages:write, for exactly that reason.
#
# The disk this produces is the cached artifact itself, never written to
# again after this script returns; every test run boots a throwaway
# qcow2 overlay backed by it (test/vm/run-omv-e2e.sh), so a run can never
# contaminate the thing other runs and other branches pull.
set -eu
set -o pipefail
here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
# shellcheck source=./lib.sh
. "$here/lib.sh"

OUT="${1:?usage: build-omv-image.sh OUT_QCOW2 WORKDIR}"
WORKDIR="${2:?usage: build-omv-image.sh OUT_QCOW2 WORKDIR}"
mkdir -p "$WORKDIR"

accel="$(vm_detect_accel)"
vm_gen_ssh_key "$WORKDIR/ssh"
pubkey="$(cat "$WORKDIR/ssh/id_ed25519.pub")"
sed "s#__SSH_PUBKEY__#$pubkey#" "$here/cloud-init-user-data.tmpl" > "$WORKDIR/user-data"
cp "$here/cloud-init-meta-data" "$WORKDIR/meta-data"
vm_make_seed_iso "$WORKDIR/user-data" "$WORKDIR/meta-data" "$WORKDIR/seed.iso"

bash "$here/fetch-debian-cloud-image.sh" "$WORKDIR/debian-base.qcow2"
qemu-img resize "$WORKDIR/debian-base.qcow2" 12G

vm_boot "$WORKDIR/debian-base.qcow2" "$WORKDIR/seed.iso" 2223 \
        "$WORKDIR/qemu.pid" "$WORKDIR/serial.log" "$accel"
# Covers every exit path (provisioning failing partway included), not
# just the wait-for-ssh timeout; cleared below once the image is known
# good, so it never fires on the ordinary post-shutdown exit.
vm_install_exit_trap "$WORKDIR" 2223 "$WORKDIR/ssh/id_ed25519" e2e

if ! vm_wait_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e 240; then
  vm_log "build-omv-image: VM never came up (or came up unreachable)"
  exit 1
fi

# The cloud image is pinned to a dated snapshot, so the kernel it boots is
# whatever was current then, and Debian's archive carries versioned headers
# only for the kernel that is current now. That combination cannot be
# resolved from inside the guest: the running kernel's headers are simply
# gone, so ZFS has nothing to build its module against. Moving to the
# archive's kernel first, and rebooting into it, is what makes the two agree.
#
# Deliberately before OpenMediaVault rather than inside the provisioning
# script: a reboot ends the session the script runs in, and once OMV is
# installed reconnecting depends on group membership the script has not
# granted yet, so the same reboot placed later reconnects to a refusal.
vm_log "build-omv-image: aligning the kernel with the archive, then rebooting into it"
vm_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e \
  "sudo apt-get update && sudo apt-get install -y linux-image-cloud-amd64 linux-headers-cloud-amd64"
vm_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e "sudo systemctl reboot" || true

# Without this the next command races the shutdown and runs against the
# kernel being replaced, which is the state this reboot exists to leave.
sleep 5
if ! vm_wait_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e 240; then
  vm_log "build-omv-image: VM did not come back after the kernel reboot"
  exit 1
fi
vm_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e "uname -r"

tar -C "$here" -cf - provision-omv.sh | \
  vm_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e "tar -xf - && chmod +x provision-omv.sh"
vm_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e "sudo bash ./provision-omv.sh"

vm_log "build-omv-image: provisioning done, shutting down cleanly"
vm_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e "sudo shutdown -h now" || true
for _ in $(seq 1 60); do
  kill -0 "$(cat "$WORKDIR/qemu.pid")" 2>/dev/null || break
  sleep 2
done

# The shutdown above is tolerated for failing, so on its own this loop just
# ends after two minutes and the image below gets copied out from under a
# still-running QEMU, with the guest's writes unflushed and its filesystem
# never quiesced. That publishes as a success and the corruption only
# surfaces later, in whatever boots the image. Refuse instead.
if kill -0 "$(cat "$WORKDIR/qemu.pid")" 2>/dev/null; then
  vm_log "build-omv-image: guest did not power off, refusing to publish an image copied from a live VM"
  vm_dump_boot_diagnosis "$WORKDIR/serial.log" "$WORKDIR/qemu.pid" 2223 || true
  exit 1
fi

trap - EXIT

mv "$WORKDIR/debian-base.qcow2" "$OUT"
vm_log "build-omv-image: OMV base image ready at $OUT"
