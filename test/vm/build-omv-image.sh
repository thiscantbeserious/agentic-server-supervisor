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
trap 'vm_stop "$WORKDIR/qemu.pid"' EXIT

if ! vm_wait_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e 240; then
  vm_log "build-omv-image: VM never came up, serial console follows:"
  cat "$WORKDIR/serial.log" >&2 || true
  exit 1
fi

tar -C "$here" -cf - provision-omv.sh | \
  vm_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e "tar -xf - && chmod +x provision-omv.sh"
vm_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e "sudo bash ./provision-omv.sh"

vm_log "build-omv-image: provisioning done, shutting down cleanly"
vm_ssh 2223 "$WORKDIR/ssh/id_ed25519" e2e "sudo shutdown -h now" || true
for _ in $(seq 1 60); do
  kill -0 "$(cat "$WORKDIR/qemu.pid")" 2>/dev/null || break
  sleep 2
done
trap - EXIT

mv "$WORKDIR/debian-base.qcow2" "$OUT"
vm_log "build-omv-image: OMV base image ready at $OUT"
