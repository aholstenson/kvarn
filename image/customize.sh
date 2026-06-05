#!/usr/bin/env bash
set -euo pipefail

# Build a customized Debian genericcloud disk image.
# Runs inside a privileged Docker container.
#
# Expected mounts:
#   /dist      — output directory (rw)
#   /scripts   — this script directory (ro)
#   /overlay   — overlay files to copy into rootfs (ro)
#
# Expected env:
#   ARCH       — target architecture (arm64 or amd64)

export DEBIAN_FRONTEND=noninteractive

DEBIAN_VERSION="trixie"

# Pinned Debian genericcloud snapshot. The dated directory is immutable, so
# pinning it makes image builds reproducible and turns a base-OS update into an
# explicit PR that bumps this snapshot plus the recorded checksums below — the
# "base image updated" signal the image release stream records. When bumping,
# copy the new digests from the snapshot's published SHA512SUMS.
DEBIAN_SNAPSHOT="20260518-2482"
BASE_URL="https://cloud.debian.org/images/cloud/${DEBIAN_VERSION}/${DEBIAN_SNAPSHOT}"
IMAGE_NAME="debian-13-genericcloud-${ARCH}-${DEBIAN_SNAPSHOT}.qcow2"

case "$ARCH" in
    amd64) IMAGE_SHA512="7752ad2adce1bc49dd964dae8300ed7a239d0bf3c13112f55953b111447fe642d2cc01afeead234aa6ebe3605513f2e7c0e7c56785d675c38ff40110d5c8332b" ;;
    arm64) IMAGE_SHA512="80a45cbd5bd74258b818f09ad0c5a004ae6030bc980fd3540ca372002257bdca92f4917f13edf1ac70cca6cfa1f534e6d22161c4bdd09ecac5684ec1a6987e3f" ;;
    *) echo "No recorded checksum for ARCH: $ARCH" >&2; exit 1 ;;
esac

# Pinned Nix release. releases.nixos.org publishes per-version, immutable
# install scripts; the unpinned https://nixos.org/nix/install URL is a 302 to
# whatever is current, which we don't want sneaking into a reproducible image.
# The script self-verifies the per-arch tarball it downloads, so a single pin
# on the script bytes covers every arch. When bumping, copy the published
# digest:
#   curl -fsSL https://releases.nixos.org/nix/nix-<version>/install.sha256
NIX_VERSION="2.34.7"
NIX_INSTALL_SHA256="e9d447ce3d2ff62d7ff9cb6ef401de6fa8acb148839dd00f7271945d7b638b14"

ROOTFS="/mnt/rootfs"

echo "==> Installing build dependencies..."
apt-get update -qq
apt-get install -y -qq qemu-utils fdisk e2fsprogs curl >/dev/null 2>&1

echo "==> Downloading Debian ${DEBIAN_VERSION} genericcloud snapshot ${DEBIAN_SNAPSHOT}..."
curl -fSL -o "/tmp/${IMAGE_NAME}" "${BASE_URL}/${IMAGE_NAME}"

echo "==> Verifying base image checksum..."
echo "${IMAGE_SHA512}  /tmp/${IMAGE_NAME}" | sha512sum -c -

echo "==> Converting qcow2 to raw..."
qemu-img convert -f qcow2 -O raw "/tmp/${IMAGE_NAME}" /dist/disk.img
rm -f "/tmp/${IMAGE_NAME}"

echo "==> Finding rootfs partition..."
PART_INFO=$(fdisk -l /dist/disk.img | grep -E "Linux (root|filesystem)" | head -1)
PART_START=$(echo "$PART_INFO" | awk '{print $2}')
PART_SECTORS=$(echo "$PART_INFO" | awk '{print $4}')
OFFSET=$((PART_START * 512))
SIZE=$((PART_SECTORS * 512))
echo "  Partition offset: $OFFSET bytes (start sector: $PART_START)"

echo "==> Mounting rootfs..."
mkdir -p "$ROOTFS"

# Ensure loop device nodes exist (Docker containers may not populate them
# even with --privileged). Fail loudly if a node exists but isn't a block
# device — silently skipping would produce a broken image at mount time.
for i in $(seq 0 7); do
    [ -b "/dev/loop$i" ] || mknod "/dev/loop$i" b 7 "$i"
done

LOOP_DEV=$(losetup --find --show --offset="$OFFSET" --sizelimit="$SIZE" /dist/disk.img)
mount "$LOOP_DEV" "$ROOTFS"

cleanup() {
    echo "==> Cleaning up mounts..."
    umount "$ROOTFS/proc" 2>/dev/null || true
    umount -R "$ROOTFS/dev" 2>/dev/null || umount -lR "$ROOTFS/dev" 2>/dev/null || true
    umount "$ROOTFS/dev/pts" 2>/dev/null || true
    umount "$ROOTFS/sys" 2>/dev/null || true
    if [ -f "$ROOTFS/etc/resolv.conf.bak" ]; then
        mv "$ROOTFS/etc/resolv.conf.bak" "$ROOTFS/etc/resolv.conf"
    elif [ -f "$ROOTFS/etc/resolv.conf.bak.link" ]; then
        LINK_TARGET=$(cat "$ROOTFS/etc/resolv.conf.bak.link")
        rm -f "$ROOTFS/etc/resolv.conf" "$ROOTFS/etc/resolv.conf.bak.link"
        ln -s "$LINK_TARGET" "$ROOTFS/etc/resolv.conf"
    fi
    umount "$ROOTFS" 2>/dev/null || true
    losetup -d "$LOOP_DEV" 2>/dev/null || true
}
trap cleanup EXIT

mount -t proc proc "$ROOTFS/proc"
mkdir -p "$ROOTFS/dev/pts"
mount -t devpts devpts "$ROOTFS/dev/pts"
mount -t sysfs sysfs "$ROOTFS/sys"

# Override DNS for package installation.
if [ -L "$ROOTFS/etc/resolv.conf" ]; then
    RESOLV_LINK=$(readlink "$ROOTFS/etc/resolv.conf")
    rm "$ROOTFS/etc/resolv.conf"
    echo "$RESOLV_LINK" > "$ROOTFS/etc/resolv.conf.bak.link"
elif [ -f "$ROOTFS/etc/resolv.conf" ]; then
    cp -a "$ROOTFS/etc/resolv.conf" "$ROOTFS/etc/resolv.conf.bak"
fi
echo "nameserver 1.1.1.1" > "$ROOTFS/etc/resolv.conf"

echo "==> Installing packages..."
chroot "$ROOTFS" apt-get update -qq
chroot "$ROOTFS" apt-get install -y -qq --no-install-recommends \
    podman \
    podman-compose \
    crun \
    fuse-overlayfs \
    passt \
    uidmap \
    bash \
    curl \
    git \
    wget \
    jq \
    python3 \
    openssh-client \
    build-essential \
    ca-certificates \
    unzip \
    zip \
    findutils \
    coreutils \
    ripgrep \
    xz-utils \
    zstd \
    aardvark-dns \
    nftables \
    iptables \
    cloud-guest-utils \
    e2fsprogs \
    pkg-config \
    procps \
    locales \
    python3-venv \
    tar \
    >/dev/null 2>&1

echo "==> Removing unnecessary packages..."
chroot "$ROOTFS" apt-get purge -y -qq \
    unattended-upgrades \
    apparmor-profiles \
    >/dev/null 2>&1 || true
chroot "$ROOTFS" apt-get autoremove -y -qq >/dev/null 2>&1

# Generate UTF-8 locale to prevent encoding errors in git, Python, Node, etc.
sed -i 's/^# *en_US.UTF-8/en_US.UTF-8/' "$ROOTFS/etc/locale.gen"
chroot "$ROOTFS" locale-gen >/dev/null 2>&1
cat > "$ROOTFS/etc/default/locale" <<'LOCALE'
LANG=en_US.UTF-8
LC_ALL=en_US.UTF-8
LOCALE

# On arm64, Go requires the gold linker for external linking (issues #15696,
# #22040). It validates the linker identity so a symlink to BFD won't work.
if [ "$ARCH" = "arm64" ]; then
    chroot "$ROOTFS" apt-get install -y -qq --no-install-recommends \
        binutils-gold >/dev/null 2>&1
fi

echo "==> Configuring system..."

# Create kvarn user with subuid/subgid mappings.
chroot "$ROOTFS" groupadd --system kvarn 2>/dev/null || true
chroot "$ROOTFS" useradd --system --gid kvarn --home-dir /home/kvarn --shell /bin/sh kvarn 2>/dev/null || true
chroot "$ROOTFS" mkdir -p /home/kvarn/workspace
chroot "$ROOTFS" chown -R kvarn:kvarn /home/kvarn
echo "kvarn:100000:65536" >> "$ROOTFS/etc/subuid"
echo "kvarn:100000:65536" >> "$ROOTFS/etc/subgid"

# Enable systemd lingering so the kvarn user's systemd instance starts at
# boot (needed for the rootless Podman socket).
mkdir -p "$ROOTFS/var/lib/systemd/linger"
touch "$ROOTFS/var/lib/systemd/linger/kvarn"

# Enable rootless Podman socket for the kvarn user.
mkdir -p "$ROOTFS/home/kvarn/.config/systemd/user/sockets.target.wants"
ln -sf /usr/lib/systemd/user/podman.socket \
    "$ROOTFS/home/kvarn/.config/systemd/user/sockets.target.wants/podman.socket"
chroot "$ROOTFS" chown -R kvarn:kvarn /home/kvarn/.config

# Podman rootless storage config.
mkdir -p "$ROOTFS/etc/containers"
cat > "$ROOTFS/etc/containers/storage.conf" <<'STORAGE'
[storage]
driver = "overlay"
runroot = "/run/containers"
graphroot = "/var/lib/kvarn/containers/storage"
[storage.options.overlay]
mount_program = "/usr/bin/fuse-overlayfs"
STORAGE

# Podman engine config: use cgroupfs and file-based logging so rootless
# containers work without a systemd user session.
cat > "$ROOTFS/etc/containers/containers.conf" <<'CONTAINERS'
[containers]
log_driver = "k8s-file"

# Trust the kvarn egress proxy CA inside every container.
# /etc/ssl/certs/ca-certificates.crt is the combined bundle produced by
# update-ca-certificates on first boot (cloud-init drops the kvarn cert at
# /usr/local/share/ca-certificates/kvarn-proxy.crt). NODE_EXTRA_CA_CERTS
# appends rather than replaces, so it gets the single-cert file.
env = [
  "SSL_CERT_FILE=/etc/ssl/certs/ca-certificates.crt",
  "GIT_SSL_CAINFO=/etc/ssl/certs/ca-certificates.crt",
  "REQUESTS_CA_BUNDLE=/etc/ssl/certs/ca-certificates.crt",
  "NODE_EXTRA_CA_CERTS=/etc/ssl/certs/kvarn-proxy.crt",
]
volumes = [
  "/etc/ssl/certs/ca-certificates.crt:/etc/ssl/certs/ca-certificates.crt:ro",
  "/usr/local/share/ca-certificates/kvarn-proxy.crt:/etc/ssl/certs/kvarn-proxy.crt:ro",
]

[engine]
cgroup_manager = "cgroupfs"
events_logger = "file"
CONTAINERS

# Container signature policy.
echo '{"default":[{"type":"insecureAcceptAnything"}]}' > "$ROOTFS/etc/containers/policy.json"

# Default to Docker Hub for unqualified image names so that
# "postgres" resolves to "docker.io/library/postgres" (matching Docker behavior).
mkdir -p "$ROOTFS/etc/containers/registries.conf.d"
cat > "$ROOTFS/etc/containers/registries.conf.d/00-kvarn.conf" <<'REGISTRIES'
unqualified-search-registries = ["docker.io"]
REGISTRIES

# Symlink docker -> podman.
ln -sf /usr/bin/podman "$ROOTFS/usr/local/bin/docker"

# Pre-create rootless storage directory.
mkdir -p "$ROOTFS/var/lib/kvarn/containers"
chroot "$ROOTFS" chown -R kvarn:kvarn /var/lib/kvarn

# Serial console: add console=hvc0 to GRUB config.
# update-grub doesn't work reliably in chroot (no /dev), so patch both
# /etc/default/grub (for future kernel installs) and grub.cfg directly.
if [ -f "$ROOTFS/etc/default/grub" ]; then
    sed -i 's/^GRUB_CMDLINE_LINUX_DEFAULT=.*/GRUB_CMDLINE_LINUX_DEFAULT="console=hvc0"/' "$ROOTFS/etc/default/grub"
fi
if [ -f "$ROOTFS/boot/grub/grub.cfg" ]; then
    sed -i 's|\(linux\s\+/boot/vmlinuz[^ ]*\s\+root=[^ ]*\s\+ro\)|\1 console=hvc0|g' "$ROOTFS/boot/grub/grub.cfg"
fi

# Sysctl settings.
echo "net.ipv4.ip_unprivileged_port_start=0" > "$ROOTFS/etc/sysctl.d/99-kvarn.conf"
echo "net.core.bpf_jit_enable=0" >> "$ROOTFS/etc/sysctl.d/99-kvarn.conf"

# Load vsock and iso9660 modules at boot.
mkdir -p "$ROOTFS/etc/modules-load.d"
cat > "$ROOTFS/etc/modules-load.d/vsock.conf" <<'MODULES'
vsock
virtio_vsock
MODULES
cat > "$ROOTFS/etc/modules-load.d/virtiofs.conf" <<'MODULES'
virtiofs
MODULES
cat > "$ROOTFS/etc/modules-load.d/iso9660.conf" <<'MODULES'
iso9660
MODULES

# Configure cloud-init to use NoCloud datasource (seed disk with label "cidata").
mkdir -p "$ROOTFS/etc/cloud/cloud.cfg.d"
cat > "$ROOTFS/etc/cloud/cloud.cfg.d/90_kvarn.cfg" <<'CLOUDINIT'
datasource_list: [NoCloud, None]

growpart:
  mode: auto
  devices: ["/"]

resize_rootfs: true
CLOUDINIT

echo "==> Installing Nix ${NIX_VERSION} (single-user, kvarn-owned)..."
# Single-user install: ephemeral VMs are single-tenant, so the daemon mode
# adds nothing but conflicts with rootless-podman uid mappings.
chroot "$ROOTFS" install -d -o kvarn -g kvarn /nix

# Fetch + verify outside the chroot so a corrupted/tampered script never runs.
echo "==> Downloading Nix installer..."
curl -fsSL "https://releases.nixos.org/nix/nix-${NIX_VERSION}/install" \
    -o "$ROOTFS/tmp/nix-install.sh"
echo "==> Verifying Nix installer checksum..."
echo "${NIX_INSTALL_SHA256}  $ROOTFS/tmp/nix-install.sh" | sha256sum -c -
chroot "$ROOTFS" chown kvarn:kvarn /tmp/nix-install.sh

# `su -l` runs cloud-init's locale script which writes to /dev/null, and the
# Nix installer opens a pseudoterminal master at /dev/ptmx. The rootfs /dev
# is empty before boot, so recursively bind the host /dev (including the
# devpts submount it depends on) for the duration of the install.
mount --rbind /dev "$ROOTFS/dev"
mount --make-rslave "$ROOTFS/dev"
chroot "$ROOTFS" su -l kvarn -s /bin/sh -c '
  set -e
  sh /tmp/nix-install.sh --no-daemon --no-channel-add --yes
  rm /tmp/nix-install.sh
'
umount -R "$ROOTFS/dev" 2>/dev/null || umount -lR "$ROOTFS/dev"

# Sandbox is off because the VM is already an isolation boundary; flake
# commands are required for the dependency-install path.
mkdir -p "$ROOTFS/etc/nix"
cat > "$ROOTFS/etc/nix/nix.conf" <<'NIXCONF'
experimental-features = nix-command flakes
sandbox = false
substituters = https://cache.nixos.org
trusted-public-keys = cache.nixos.org-1:6NCHdD59X431o0gWypbMrAURkbJ16ZPMQFGspcDShjY=
NIXCONF

# Expose the kvarn-user nix profile to system login shells (su -l, sh -l,
# podman exec ... sh -l) so dependency-installed binaries land on PATH.
ln -sf /home/kvarn/.nix-profile/etc/profile.d/nix.sh \
    "$ROOTFS/etc/profile.d/nix.sh"

echo "==> Copying overlay files..."
cp -a /overlay/. "$ROOTFS/"

echo "==> Making scripts executable..."
chmod +x "$ROOTFS/usr/local/bin/kvarn-runner-setup.sh"

echo "==> Enabling systemd services..."
mkdir -p "$ROOTFS/etc/systemd/system/cloud-init.target.wants"
for svc in cloud-init-local.service cloud-init-network.service cloud-init-main.service; do
    ln -sf "/lib/systemd/system/$svc" "$ROOTFS/etc/systemd/system/cloud-init.target.wants/$svc"
done
ln -sf /lib/systemd/system/cloud-init.target "$ROOTFS/etc/systemd/system/multi-user.target.wants/cloud-init.target"
chroot "$ROOTFS" systemctl enable kvarn-runner

echo "==> Cleaning up..."
chroot "$ROOTFS" apt-get autoremove -y
chroot "$ROOTFS" apt-get clean -y
rm -rf "$ROOTFS/var/lib/apt/lists/"*
rm -rf "$ROOTFS/var/cache/apt/"*
rm -rf "$ROOTFS/tmp/"*

# Remove documentation, man pages, and unused locale data.
rm -rf "$ROOTFS/usr/share/doc/"*
rm -rf "$ROOTFS/usr/share/man/"*
find "$ROOTFS/usr/share/locale" -mindepth 1 -maxdepth 1 ! -name 'en_US' -exec rm -rf {} +

# Remove kernel headers and module build symlinks.
rm -rf "$ROOTFS/usr/src/"*
rm -rf "$ROOTFS/lib/modules/"*/build

# Truncate log files from package installation.
find "$ROOTFS/var/log" -type f -exec truncate -s 0 {} +

# Restore resolv.conf before zero-fill.
if [ -f "$ROOTFS/etc/resolv.conf.bak.link" ]; then
    LINK_TARGET=$(cat "$ROOTFS/etc/resolv.conf.bak.link")
    rm -f "$ROOTFS/etc/resolv.conf" "$ROOTFS/etc/resolv.conf.bak.link"
    ln -s "$LINK_TARGET" "$ROOTFS/etc/resolv.conf"
elif [ -f "$ROOTFS/etc/resolv.conf.bak" ]; then
    mv "$ROOTFS/etc/resolv.conf.bak" "$ROOTFS/etc/resolv.conf"
fi

echo "==> Zero-filling free space for compression..."
dd if=/dev/zero of="$ROOTFS/zero" bs=1M 2>/dev/null || true
rm -f "$ROOTFS/zero"

# Unmount chroot filesystems before unmounting rootfs.
umount "$ROOTFS/proc" 2>/dev/null || true
umount "$ROOTFS/dev/pts" 2>/dev/null || true
umount "$ROOTFS/sys" 2>/dev/null || true
umount "$ROOTFS"
losetup -d "$LOOP_DEV" 2>/dev/null || true
trap - EXIT

echo "==> Converting to qcow2..."
qemu-img convert -f raw -O qcow2 -c /dist/disk.img /dist/disk.qcow2
rm -f /dist/disk.img

echo "==> Image customization complete"
ls -lh /dist/disk.qcow2
