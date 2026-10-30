#!/usr/bin/env bash
# scripts/release.sh — tag and push miner + browser images to ghcr.io.
#
# Usage:
#   GITHUB_REPOSITORY_OWNER=al1sse scripts/release.sh v0.1.0
#   GITHUB_REPOSITORY_OWNER=al1sse scripts/release.sh v0.1.0 --build-only
#
# Prereqs:
#   Set GITHUB_REPOSITORY_OWNER to the GHCR owner and run docker login ghcr.io

set -euo pipefail

TAG="${1:-}"
MODE="${2:-push}"

if [[ -z "$TAG" ]]; then
  echo "usage: $0 <tag> [--build-only]" >&2
  exit 2
fi

REGISTRY_OWNER="${GITHUB_REPOSITORY_OWNER:-}"
if [[ -z "$REGISTRY_OWNER" ]]; then
  echo "GITHUB_REPOSITORY_OWNER must be set to the GHCR owner." >&2
  exit 2
fi
REGISTRY="ghcr.io/$REGISTRY_OWNER"
GRUB_IMAGE="$REGISTRY/grubdrops"
BROWSER_IMAGE="$REGISTRY/grubdrops-browser"

REPO_ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_ROOT"

echo "=== Building $GRUB_IMAGE:$TAG ==="
docker build -f deploy/Dockerfile.miner -t "$GRUB_IMAGE:$TAG" -t "$GRUB_IMAGE:latest" .

echo "=== Building $BROWSER_IMAGE:$TAG ==="
docker build -f deploy/Dockerfile.browser -t "$BROWSER_IMAGE:$TAG" -t "$BROWSER_IMAGE:latest" .

if [[ "$MODE" == "--build-only" ]]; then
  echo "Build-only mode; not pushing."
  exit 0
fi

echo "=== Pushing miner ==="
docker push "$GRUB_IMAGE:$TAG"
docker push "$GRUB_IMAGE:latest"

echo "=== Pushing browser ==="
docker push "$BROWSER_IMAGE:$TAG"
docker push "$BROWSER_IMAGE:latest"

echo
echo "Released:"
echo "  $GRUB_IMAGE:$TAG"
echo "  $BROWSER_IMAGE:$TAG"
echo
echo "Next: update your homelab compose.yml image tags, commit, deploy."
