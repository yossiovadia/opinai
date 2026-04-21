#!/usr/bin/env bash
# Deploy OpinAI into kind-maas-local cluster.
# Backs up secrets from kind-opinai, builds images, loads into maas-local,
# creates namespace + RBAC, applies secrets and manifests.
#
# Usage: ./scripts/deploy-to-maas-cluster.sh
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

SOURCE_CONTEXT="kind-opinai"
TARGET_CONTEXT="kind-maas-local"
NAMESPACE="opinai"

echo "=== OpinAI → kind-maas-local deployment ==="

# Step 1: Back up secrets/config from source cluster
echo "[1/7] Backing up secrets from $SOURCE_CONTEXT..."
kubectl --context "$SOURCE_CONTEXT" get secret opinai-credentials -n "$NAMESPACE" -o yaml > /tmp/opinai-creds-backup.yaml 2>/dev/null || echo "  WARNING: opinai-credentials not found in $SOURCE_CONTEXT"
kubectl --context "$SOURCE_CONTEXT" get secret opinai-gcp-credentials -n "$NAMESPACE" -o yaml > /tmp/opinai-gcp-backup.yaml 2>/dev/null || echo "  WARNING: opinai-gcp-credentials not found in $SOURCE_CONTEXT"
kubectl --context "$SOURCE_CONTEXT" get configmap opinai-config -n "$NAMESPACE" -o yaml > /tmp/opinai-config-backup.yaml 2>/dev/null || echo "  WARNING: opinai-config not found in $SOURCE_CONTEXT"

# Step 2: Build images
echo "[2/7] Building OpinAI images..."
(cd "$PROJECT_ROOT" && docker build -t opinai-controller:latest . 2>&1 | tail -3)
(cd "$PROJECT_ROOT" && docker build -f Dockerfile.python -t opinai-runner-python:latest . 2>&1 | tail -3)

# Step 3: Load into target cluster
echo "[3/7] Loading images into $TARGET_CONTEXT..."
kind load docker-image opinai-controller:latest --name maas-local
kind load docker-image opinai-runner-python:latest --name maas-local

# Step 4: Create namespace
echo "[4/7] Creating namespace in $TARGET_CONTEXT..."
kubectl --context "$TARGET_CONTEXT" get namespace "$NAMESPACE" &>/dev/null || \
  kubectl --context "$TARGET_CONTEXT" create namespace "$NAMESPACE"

# Step 5: Set up RBAC
echo "[5/7] Setting up RBAC..."
kubectl --context "$TARGET_CONTEXT" config use-context "$TARGET_CONTEXT"
bash "$SCRIPT_DIR/setup.sh" "$NAMESPACE"

# Step 6: Apply backed-up secrets (strip metadata that prevents re-apply)
echo "[6/7] Applying secrets and config..."
strip_and_apply() {
  local file="$1"
  if [ ! -f "$file" ] || [ ! -s "$file" ]; then
    echo "  Skipping $file (not found or empty)"
    return
  fi
  # Remove resourceVersion, uid, creationTimestamp, namespace metadata
  sed -e '/resourceVersion:/d' \
      -e '/uid:/d' \
      -e '/creationTimestamp:/d' \
      -e "s/namespace: .*/namespace: $NAMESPACE/" \
      "$file" | kubectl --context "$TARGET_CONTEXT" apply -f -
}
strip_and_apply /tmp/opinai-creds-backup.yaml
strip_and_apply /tmp/opinai-gcp-backup.yaml
strip_and_apply /tmp/opinai-config-backup.yaml

# Step 7: Apply deployment manifests
echo "[7/7] Applying deployment manifests..."
kubectl --context "$TARGET_CONTEXT" apply -f "$PROJECT_ROOT/manifests/"

# Wait for rollout
echo "Waiting for rollout..."
kubectl --context "$TARGET_CONTEXT" -n "$NAMESPACE" rollout status deployment/opinai-controller --timeout=120s

echo ""
echo "=== OpinAI deployed to kind-maas-local ==="
echo "  Dashboard: kubectl --context $TARGET_CONTEXT -n $NAMESPACE port-forward svc/opinai-controller 8080:8080"
echo ""
echo "NOTE: kind-opinai cluster was NOT deleted. Remove it manually when ready:"
echo "  kind delete cluster --name opinai"
