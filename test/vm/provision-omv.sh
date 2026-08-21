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

# packages.openmediavault.org/public/install no longer exists (404 direct,
# 403 through the CDN); the maintained installer lives in the plugin
# developers' repository. Pinned to a commit rather than master: the URL
# feeds this image's content-hash tag, and a moving target would change what
# the image contains while the tag stayed put.
OMV_INSTALL_REF="6983f480513e23e8362a0f043879745df557cab7"
OMV_INSTALL_URL="https://raw.githubusercontent.com/OpenMediaVault-Plugin-Developers/installScript/${OMV_INSTALL_REF}/install"

echo "== provision-omv: base OS =="
apt-get update
apt-get install -y curl ca-certificates

echo "== provision-omv: running OMV's own installer =="
# -n skips the installer's network reconfiguration, which purges cloud-init
# and network-manager, rewrites the interfaces under systemd-networkd and
# reboots. That would strip this VM of the DHCP-over-slirp setup QEMU gives
# it and of the datasource the build boot depends on, so the machine that
# came back would not be the one being provisioned. -r additionally refuses
# any reboot: today every reboot path sits inside the block -n already
# skips, and this keeps that true if a later revision adds another one.
curl -fsSL "$OMV_INSTALL_URL" | bash -s -- -n -r

echo "== provision-omv: ZFS =="
# The target host stores its stacks on ZFS and sentinel reads zpool status,
# so the image that stands in for it gets a real pool rather than whichever
# filesystem is cheapest to create. It also supplies the mounted filesystem
# OMV needs before it will create a shared folder.
#
# The headers are named for the running kernel rather than the usual
# linux-headers-amd64: genericcloud images run the "cloud" kernel flavour,
# and the DKMS build silently has nothing to build against if the generic
# headers are installed instead.
apt-get install -y "linux-headers-$(uname -r)"
apt-get install -y openmediavault-zfs

# The pool sits on a file vdev inside the root filesystem rather than on a
# second disk, because everything downstream of this script moves the image
# as a single qcow2: the publish step copies one file into a scratch layer
# and the test-time job extracts one file back out. A second disk would be
# created, provisioned, and then silently dropped at publish, leaving an
# image whose pool exists only in the build log.
#
# It is a real pool either way, which is what the assertions and the
# collected fixtures need. The vdev is sparse, so the published image grows
# by what the pool actually holds, not by its nominal size.
mkdir -p /var/lib/vm-e2e
truncate -s 2G /var/lib/vm-e2e/tank.img
zpool create -f -m /tank tank /var/lib/vm-e2e/tank.img
# Without an explicit cachefile the pool is not imported when an overlay of
# this image boots later, and every test-time assertion below would look at
# an empty mountpoint.
zpool set cachefile=/etc/zfs/zpool.cache tank
systemctl enable zfs-import-cache.service zfs-mount.service
zpool status

echo "== provision-omv: OMV compose plugin =="
# The plugin that actually writes the compose.yml symlinks this job exists
# to check, contracts/runtime.md's compose-root detection bug was invisible
# in every fixture because every fixture used a relative symlink no real
# OMV host ever produces; only the plugin's own code path can reproduce it.
apt-get install -y openmediavault-compose

echo "== provision-omv: registering the pool with OMV =="
# Creating the pool does not tell OMV about it. Until a mountpoint exists in
# OMV's configuration database there is nothing a shared folder can point
# at, and the compose plugin's states all sit behind that shared folder.
# FsTab.set is what the zfs plugin itself calls for exactly this, with these
# fields. The magic uuid is OMV's "this object is new" marker, read from the
# package's own defaults rather than pasted in.
# shellcheck disable=SC1091
. /etc/default/openmediavault
NEW_UUID="${OMV_CONFIGOBJECT_NEW_UUID:?OMV_CONFIGOBJECT_NEW_UUID missing from /etc/default/openmediavault}"

rpc() { omv-rpc -u admin "$@"; }
jget() { python3 -c 'import json,sys; print(json.load(sys.stdin).get(sys.argv[1],""))' "$1"; }

mntentref="$(rpc "FsTab" "set" "{\"uuid\":\"$NEW_UUID\",\"fsname\":\"tank\",\"dir\":\"/tank\",\"type\":\"zfs\",\"opts\":\"\",\"freq\":0,\"passno\":0}" | jget uuid)"
[ -n "$mntentref" ] || { echo "provision-omv: FsTab.set returned no uuid, the pool is not registered with OMV" >&2; exit 1; }

echo "== provision-omv: shared folder on the pool =="
sfref="$(rpc "ShareMgmt" "set" "{\"uuid\":\"$NEW_UUID\",\"name\":\"compose\",\"reldirpath\":\"compose/\",\"comment\":\"\",\"mntentref\":\"$mntentref\"}" | jget uuid)"
[ -n "$sfref" ] || { echo "provision-omv: ShareMgmt.set returned no uuid, no shared folder to hand the compose plugin" >&2; exit 1; }

echo "== provision-omv: pointing the compose plugin at it =="
# Compose.set validates the whole settings object, so the current settings
# are read back and only sharedfolderref is changed. Writing a hand-built
# object would either be rejected or silently reset every other setting.
compose_settings="$(rpc "Compose" "get" '{}' | python3 -c '
import json, sys
d = json.load(sys.stdin)
d["sharedfolderref"] = sys.argv[1]
d.pop("files", None)
json.dump(d, sys.stdout)
' "$sfref")"
rpc "Compose" "set" "$compose_settings" >/dev/null

echo "== provision-omv: materialising a real compose stack =="
# The plugin has no "path" property and omv-confdbadm create takes an id and
# nothing else, so the shape used here before could never have worked. The
# real model, from the plugin's datamodel and its 10compose.sls: a file is a
# named body in the configuration database, and salt derives the layout
#   <shared folder>/<name>/<name>.yml
#   <shared folder>/<name>/compose.yml -> <shared folder>/<name>/<name>.yml
# with an ABSOLUTE target. That absolute symlink is the entire reason this
# variant exists: every hand-written fixture used a relative one, which is
# why the compose-root defect survived them all. createsymlinks defaults to
# 1, so the symlink needs no further setting.
rpc "Compose" "setFile" '{"name":"e2e-probe","description":"compose-root detection probe","body":"services:\n  probe:\n    image: busybox\n    command: [\"true\"]\n","showenv":false,"env":"","showoverride":false,"override":""}' >/dev/null
omv-salt deploy run compose

echo "== provision-omv: proving the layout is what the test expects =="
# Asserted here, at image-build time, rather than trusting the deploy's exit
# code: every state above can report success and still produce no stack,
# which is precisely how the previous image passed while containing nothing.
# A base image that silently lacks the one artifact it exists to carry would
# send every later run hunting the wrong bug.
link="/tank/compose/e2e-probe/compose.yml"
target="$(readlink "$link" 2>/dev/null || true)"
case "$target" in
  /*) echo "provision-omv: compose.yml -> $target" ;;
  "") echo "provision-omv: $link does not exist, OMV created no stack" >&2; exit 1 ;;
  *)  echo "provision-omv: $link points at a relative target ($target), which is not the shape this image exists to reproduce" >&2; exit 1 ;;
esac

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
# OpenMediaVault replaces /etc/ssh/sshd_config with its own template, which
# carries an unconditional "AllowGroups root _ssh". Membership in sudo is
# irrelevant to that list, so every new login for this account is refused
# before any key or password is examined, reported as the misleading
# "Permission denied (publickey,password)". It goes unnoticed during
# provisioning because this whole script runs inside one session opened
# before OMV was installed; the first connection to meet the new config is
# the shutdown that ends the build, which then hangs until the job timeout.
# A drop-in cannot fix this: OMV places its Include of sshd_config.d last,
# where the already-set value wins.
usermod -aG _ssh e2e
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
