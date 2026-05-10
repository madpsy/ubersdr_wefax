#!/usr/bin/env bash
# docker.sh — build the ubersdr_wefax Docker image
#
# All binaries are built from source inside the Docker image.
# No host binaries are required.
#
# Usage:
#   ./docker.sh [build|push|run|arm64]
#
#   build  — build the image for linux/amd64 (default)
#   arm64  — build the image for linux/arm64 (Raspberry Pi, Apple Silicon, etc.)
#   push   — build then push to registry (set IMAGE env var)
#   run    — run the image locally (set env vars below)
#
# Environment variables (build):
#   IMAGE      Docker image name/tag   (default: madpsy/ubersdr_wefax:latest)
#   PLATFORM   Docker --platform flag  (default: linux/amd64)

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

IMAGE="${IMAGE:-madpsy/ubersdr_wefax:latest}"
PLATFORM="${PLATFORM:-linux/amd64}"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

die() { echo "error: $*" >&2; exit 1; }

check_deps() {
    command -v docker >/dev/null || die "docker not found in PATH"
}

build() {
    check_deps

    # Create a temporary build context from the source tree only
    TMPCTX="$(mktemp -d)"
    trap 'rm -rf "$TMPCTX"' EXIT

    echo "Staging build context in $TMPCTX..."

    # Copy source tree (excluding build artefacts and git history)
    rsync -a --exclude='.git' \
              --exclude='wefax-images' \
              --exclude='images' \
              --exclude='ubersdr_wefax' \
              "$SCRIPT_DIR/" "$TMPCTX/"

    echo "Building image $IMAGE (platform=$PLATFORM)..."
    docker build \
        --platform "$PLATFORM" \
        --tag "$IMAGE" \
        "$TMPCTX"

    echo "Built: $IMAGE"
}

push() {
    build
    echo "Pushing $IMAGE..."
    docker push "$IMAGE"
    echo "Committing and pushing git repository..."
    git add -A
    git diff --cached --quiet || git commit -m "Release $IMAGE"
    git push
}

run_image() {
    local args=()

    [[ -n "${UBERSDR_URL:-}"      ]] && args+=(-e "UBERSDR_URL=$UBERSDR_URL")
    [[ -n "${UBERSDR_CHANNELS:-}" ]] && args+=(-e "UBERSDR_CHANNELS=$UBERSDR_CHANNELS")
    [[ -n "${UBERSDR_PASS:-}"     ]] && args+=(-e "UBERSDR_PASS=$UBERSDR_PASS")
    [[ -n "${OUTPUT_DIR:-}"       ]] && args+=(-e "OUTPUT_DIR=$OUTPUT_DIR")
    [[ -n "${WEB_PORT:-}"         ]] && args+=(-e "WEB_PORT=$WEB_PORT")
    [[ -n "${LPM:-}"              ]] && args+=(-e "LPM=$LPM")
    [[ -n "${IMAGE_WIDTH:-}"      ]] && args+=(-e "IMAGE_WIDTH=$IMAGE_WIDTH")
    [[ "${NO_PHASING:-}"   = "1"  ]] && args+=(-e "NO_PHASING=1")
    [[ "${NO_AUTOSTOP:-}"  = "1"  ]] && args+=(-e "NO_AUTOSTOP=1")
    [[ "${NO_AUTOSTART:-}" = "1"  ]] && args+=(-e "NO_AUTOSTART=1")

    docker run --rm -it \
        --platform "$PLATFORM" \
        -p "${WEB_PORT:-6094}:${WEB_PORT:-6094}" \
        "${args[@]}" \
        "$IMAGE" \
        "$@"
}

# ---------------------------------------------------------------------------
# Environment variable reference (for docker run -e ...)
# ---------------------------------------------------------------------------
#
#   UBERSDR_URL       UberSDR WebSocket URL (default: ws://ubersdr:8080/ws)
#   UBERSDR_CHANNELS  Comma-separated freq:mode pairs, e.g. 7880000:usb,13882500:usb
#   UBERSDR_PASS      UberSDR bypass password (optional)
#   OUTPUT_DIR        Output directory for images (default: /data)
#   WEB_PORT          Web gallery port (default: 6094)
#   LPM               Lines per minute: 120 (default) or 60
#   IMAGE_WIDTH       Image width: 1809 = IOC-576 (default), 904 = IOC-288
#   NO_PHASING        Set to 1 to disable horizontal phasing sync
#   NO_AUTOSTOP       Set to 1 to disable auto-stop on STOP tone
#   NO_AUTOSTART      Set to 1 to disable auto-start on START tone

# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------

case "${1:-build}" in
    build) build ;;
    arm64) PLATFORM=linux/arm64 build ;;
    push)  push  ;;
    run)   shift; run_image "$@" ;;
    *)
        echo "Usage: $0 [build|arm64|push|run [args...]]" >&2
        exit 1
        ;;
esac
