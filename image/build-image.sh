#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
ROOT_DIR="$(cd "$SCRIPT_DIR/.." && pwd)"

# Determine target architecture.
if [ -z "${ARCH:-}" ]; then
    HOST_ARCH="$(uname -m)"
    case "$HOST_ARCH" in
        aarch64|arm64) ARCH="arm64" ;;
        x86_64|amd64)  ARCH="amd64" ;;
        *) echo "Unsupported host architecture: $HOST_ARCH" >&2; exit 1 ;;
    esac
fi

case "$ARCH" in
    arm64) DOCKER_PLATFORM="linux/arm64" ;;
    amd64) DOCKER_PLATFORM="linux/amd64" ;;
    *) echo "Unsupported ARCH: $ARCH (use arm64 or amd64)" >&2; exit 1 ;;
esac

DIST_DIR="$ROOT_DIR/dist/$ARCH"
mkdir -p "$DIST_DIR"

echo "==> Building for $ARCH (platform: $DOCKER_PLATFORM)"

echo "==> Cross-compiling runner for linux/$ARCH..."
cd "$ROOT_DIR"
GOARCH="$ARCH" GOOS=linux CGO_ENABLED=0 go build -o "$DIST_DIR/kvarn-runner" ./cmd/runner

cleanup() {
    rm -f "$DIST_DIR/kvarn-runner"
}
trap cleanup EXIT

echo "==> Building image in Docker..."
docker run --rm --privileged --platform "$DOCKER_PLATFORM" \
    -v "$DIST_DIR:/dist" \
    -v "$SCRIPT_DIR:/scripts:ro" \
    -v "$SCRIPT_DIR/overlay:/overlay:ro" \
    -e ARCH="$ARCH" \
    debian:trixie /scripts/customize.sh
