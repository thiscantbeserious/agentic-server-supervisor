#!/bin/bash
# provision-omv.sh runs once, as root, inside the VM that becomes the
# cached OMV base image (test/vm/build-omv-image.sh copies it in and
# executes it over SSH). Nothing here is exercised at test time, only at
# image-build time; the actual install.sh run happens later, against a
# disposable overlay of the image this script produces.
#
# Every input that changes what this script does must also be reflected in
# the image tag (test/vm/build-omv-image.sh computes it), or a changed
# script would silently test a stale cached image, exactly the failure
# this design exists to rule out.
set -eu
set -o pipefail

# Pinned rather than "latest": a moving install target would change the
# image out from under its own content-hash tag without the tag noticing.
# openmediavault.org's own documented install command as of this writing.
# UNVERIFIED beyond documentation: no local sandbox can run apt against a
# real Debian mirror, so whether this URL and this exact invocation still
# work is proven only by the first real CI run, not by anything here.
OMV_INSTALL_URL="https://packages.openmediavault.org/public/install"

echo "== provision-omv: base OS =="
apt-get update
apt-get install -y curl ca-certificates

echo "== provision-omv: running OMV's own installer =="
curl -fsSL "$OMV_INSTALL_URL" | bash

echo "== provision-omv: OMV compose plugin =="
# The plugin that actually writes the compose.yml symlinks this job exists
# to check, contracts/runtime.md's compose-root detection bug was invisible
# in every fixture because every fixture used a relative symlink no real
# OMV host ever produces; only the plugin's own code path can reproduce it.
apt-get install -y openmediavault-compose

echo "== provision-omv: materialising a real compose stack =="
# omv-confdbadm writes the compose service into OMV's config database;
# omv-salt deploy is what OMV itself runs on every config change (the same
# path the web UI triggers) and is what actually creates the on-disk
# directory, the "<name>.yml" file and the "compose.yml" symlink pointing
# at it with an ABSOLUTE target. Command shape taken from OMV's own salt
# state files for the compose plugin; UNVERIFIED against a real install,
# flagged the same as the install URL above.
mkdir -p /sharedfolders/compose-e2e
cat > /sharedfolders/compose-e2e/e2e-probe.yml <<'YAML'
services:
  probe:
    image: busybox
    command: ["true"]
YAML
omv-confdbadm create --uuid conf.service.compose.file \
  --data "{\"name\":\"e2e-probe\",\"path\":\"/sharedfolders/compose-e2e/e2e-probe.yml\",\"enable\":true}" \
  || echo "provision-omv: WARN, omv-confdbadm compose registration failed, compose-root assertion may find no stack" >&2
omv-salt deploy run compose 2>&1 || echo "provision-omv: WARN, omv-salt deploy failed" >&2

echo "== provision-omv: throwaway login for the test-time job =="
# The base image is built once (this script) and reused, key-based, by
# build-omv-image.sh's own boot. Every LATER run of run-omv-e2e.sh boots
# an already-baked overlay with no cloud-init CD-ROM attached (attaching
# one would mean re-seeding a "first boot" that already happened), so it
# has no channel to inject a fresh key. A fixed low-privilege password
# account, sudo via NOPASSWD, is the whole tradeoff: it never leaves this
# disposable, network-isolated VM, and it is discarded with the overlay
# at the end of every run.
useradd -m -s /bin/bash e2e || true
echo 'e2e:e2e' | chpasswd
usermod -aG sudo e2e
echo 'e2e ALL=(ALL) NOPASSWD:ALL' > /etc/sudoers.d/90-e2e
chmod 440 /etc/sudoers.d/90-e2e
sed -i 's/^#\?PasswordAuthentication.*/PasswordAuthentication yes/' /etc/ssh/sshd_config
systemctl restart ssh

echo "== provision-omv: console network self-report on every future boot =="
# This variant boots the already-provisioned image directly, no cloud-init
# CD-ROM, so the runcmd equivalent in cloud-init-user-data.tmpl never runs
# here. Baked in as a real systemd unit instead of leaving this variant
# quietly less diagnosable: it costs one file and one enable, and runs on
# every boot of every overlay built from this base, not only this one.
cat > /etc/systemd/system/console-netinfo.service <<'UNIT'
[Unit]
Description=Report guest network state to the console for VM E2E diagnostics
After=network.target

[Service]
Type=oneshot
StandardOutput=journal+console
ExecStart=/bin/sh -c 'echo "=== console-netinfo ==="; ip -br addr; ss -ltn; echo "=== console-netinfo end ==="'

[Install]
WantedBy=multi-user.target
UNIT
systemctl enable console-netinfo.service

echo "== provision-omv: recording expected versions for the cache-content assertion =="
{
  dpkg-query -W -f='openmediavault ${Version}\n' openmediavault
  dpkg-query -W -f='postfix ${Version}\n' postfix
} > /etc/vm-e2e-expected-versions

echo "== provision-omv: done =="
