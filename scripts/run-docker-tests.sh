#!/bin/bash
# Run tests in Docker with erofs-utils built from source
# This ensures we use the same erofs-utils version (v1.8.10) as specified in build-erofs-utils.sh
#
# Usage:
#   ./scripts/run-docker-tests.sh [options]
#
# Options:
#   --build    Force rebuild of the Docker image
#   --shell    Start an interactive shell instead of running tests
#   -h, --help Show this help message

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT_DIR="$(cd "${SCRIPT_DIR}/.." && pwd)"
IMAGE_NAME="nexuserofs-test"
FORCE_BUILD=false
INTERACTIVE=false

# Parse arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --build)
            FORCE_BUILD=true
            shift
            ;;
        --shell)
            INTERACTIVE=true
            shift
            ;;
        -h|--help)
            head -20 "$0" | tail -n +2 | sed 's/^# //' | sed 's/^#//'
            exit 0
            ;;
        *)
            echo "Unknown option: $1"
            exit 1
            ;;
    esac
done

# Check if image exists
image_exists() {
    docker image inspect "${IMAGE_NAME}" &>/dev/null
}

# Build the Docker image
build_image() {
    echo "==> Building Docker image: ${IMAGE_NAME}"
    docker build -t "${IMAGE_NAME}" -f "${ROOT_DIR}/Dockerfile.test" "${ROOT_DIR}"
}

# Build image if needed
if [[ "${FORCE_BUILD}" == "true" ]] || ! image_exists; then
    build_image
else
    echo "==> Using existing Docker image: ${IMAGE_NAME}"
    echo "    (use --build to force rebuild)"
fi

echo "==> Running tests in Docker container"
echo "    Project root: ${ROOT_DIR}"

# Determine Go cache directories
GO_MOD_CACHE="${GOMODCACHE:-${GOPATH:-$HOME/go}/pkg/mod}"
GO_BUILD_CACHE="${GOCACHE:-$HOME/.cache/go-build}"

# Ensure cache directories exist
mkdir -p "${GO_MOD_CACHE}" "${GO_BUILD_CACHE}"

# Common docker run options:
# --privileged: Required for mount operations and loop devices
# --rm: Remove container after exit
# -v /dev:/dev: Mount host /dev for loop device access
# -v /tmp: Bind mount /tmp to avoid overlay filesystem issues
# -v workspace: Mount project directory
# -v go caches: Reuse host Go module and build caches
DOCKER_OPTS=(
    --privileged
    --rm
    -v /dev:/dev
    -v /tmp:/tmp
    -v "${ROOT_DIR}:/workspace"
    -v "${GO_MOD_CACHE}:/go/pkg/mod"
    -v "${GO_BUILD_CACHE}:/root/.cache/go-build"
    -w /workspace
)

if [[ "${INTERACTIVE}" == "true" ]]; then
    echo "==> Starting interactive shell..."
    docker run -it "${DOCKER_OPTS[@]}" "${IMAGE_NAME}" /bin/bash
else
    docker run "${DOCKER_OPTS[@]}" "${IMAGE_NAME}"
fi
