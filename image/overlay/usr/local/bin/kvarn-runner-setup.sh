#!/bin/sh
set -eu

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
