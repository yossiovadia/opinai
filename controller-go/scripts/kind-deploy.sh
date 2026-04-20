#!/usr/bin/env bash
# OpinAI Kind deployment — builds and loads ALL required images.
# Usage: ./scripts/kind-deploy.sh [CLUSTER_NAME]
set -euo pipefail

CLUSTER_NAME="${1:-opinai}"
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

echo "Building OpinAI images for Kind cluster: $CLUSTER_NAME"

# Build controller
echo "  Building opinai-controller..."
(cd "$PROJECT_ROOT" && docker build -t opinai-controller:latest . 2>&1 | tail -3)

# Build runner (used for PR reviews and issue reproductions)
echo "  Building opinai-runner-python..."
(cd "$PROJECT_ROOT" && docker build -f Dockerfile.python -t opinai-runner-python:latest . 2>&1 | tail -3)

# Load into Kind
echo "  Loading images into Kind..."
kind load docker-image opinai-controller:latest --name "$CLUSTER_NAME"
kind load docker-image opinai-runner-python:latest --name "$CLUSTER_NAME"

echo ""
echo "✓ Both images loaded into kind-${CLUSTER_NAME}"
echo "  - opinai-controller:latest    (main controller + dashboard)"
echo "  - opinai-runner-python:latest  (PR review + issue reproduction runner)"
