#!/bin/sh
set -eu

# Stage the runner binary injected onto the cloud-init seed ISO. The
# orchestrator embeds and ships the exact runner it speaks to, so the image
# carries none at rest. The iso9660 module is loaded at boot; the device may
# take a moment to appear, so wait for it. Failures here are fatal so the
# unit's Restart=on-failure retries instead of ExecStart launching a missing
# binary.
SEED_DEV=/dev/disk/by-label/cidata
SEED_MNT=/run/kvarn-seed
RETRIES=30
while [ ! -e "$SEED_DEV" ] && [ "$RETRIES" -gt 0 ]; do
	sleep 1
	RETRIES=$((RETRIES - 1))
done

if [ ! -e "$SEED_DEV" ]; then
	echo "kvarn-runner-setup: seed device $SEED_DEV not found after timeout" >&2
	exit 1
fi

mkdir -p "$SEED_MNT"
umount "$SEED_MNT" 2>/dev/null || true
mount -o ro "$SEED_DEV" "$SEED_MNT"
cp "$SEED_MNT/kvarn-runner" /usr/local/bin/kvarn
chmod +x /usr/local/bin/kvarn
umount "$SEED_MNT"

# Wait for cloud-init to write the runner env file.
RETRIES=30
while [ ! -f /run/kvarn-runner.env ] && [ "$RETRIES" -gt 0 ]; do
	sleep 1
	RETRIES=$((RETRIES - 1))
done

if [ ! -f /run/kvarn-runner.env ]; then
	echo "kvarn-runner-setup: /run/kvarn-runner.env not found after timeout, using default" >&2
	printf 'KVARN_RUNNER_ARGS=--addr :9090\n' > /run/kvarn-runner.env
fi

# Create XDG_RUNTIME_DIR for rootless Podman (/run is tmpfs, empty at boot).
kvarn_uid=$(id -u kvarn)
mkdir -p "/run/user/${kvarn_uid}"
chown kvarn:kvarn "/run/user/${kvarn_uid}"
chmod 700 "/run/user/${kvarn_uid}"

# Create containers runroot directory.
mkdir -p /run/containers
chown kvarn:kvarn /run/containers

# Wait for the rootless Podman socket to be ready.
echo "kvarn-runner-setup: waiting for podman..."
RETRIES=30
while [ "$RETRIES" -gt 0 ]; do
	if su -l -s /bin/sh -c "podman info >/dev/null 2>&1" kvarn; then
		echo "kvarn-runner-setup: podman is ready"
		break
	fi
	sleep 1
	RETRIES=$((RETRIES - 1))
done

if [ "$RETRIES" -eq 0 ]; then
	echo "kvarn-runner-setup: podman not ready after timeout, continuing anyway" >&2
fi
