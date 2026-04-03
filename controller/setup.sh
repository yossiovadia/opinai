#!/bin/bash
set -eo pipefail

echo ""
echo "🎳 OpinAI Controller Setup"
echo "=========================="
echo ""

# Step 1: Prerequisites
echo "📋 Step 1: Prerequisites"
echo ""

for cmd in oc gh curl jq; do
  if command -v "$cmd" &>/dev/null; then
    echo "  ✅ $cmd found"
  else
    echo "  ❌ $cmd not found — please install it first"
    exit 1
  fi
done

# Check cluster connection
echo ""
CLUSTER=$(oc whoami --show-server 2>/dev/null || echo "")
if [ -z "$CLUSTER" ]; then
  echo "  ❌ Not logged into an OpenShift cluster"
  echo "  Run: oc login <cluster-url> --username=<user> --password=<pass>"
  exit 1
fi
OC_USER=$(oc whoami 2>/dev/null)
echo "  ✅ Cluster: $CLUSTER (user: $OC_USER)"

# Check gh auth
GH_USER=$(gh auth status 2>&1 | grep "Logged in" | head -1 || echo "")
if [ -z "$GH_USER" ]; then
  echo "  ❌ Not logged into GitHub CLI"
  echo "  Run: gh auth login"
  exit 1
fi
echo "  ✅ GitHub CLI authenticated"
echo ""

# Step 2: AI Provider
echo "🧠 Step 2: AI Provider"
echo ""
echo "  Which AI provider for bug analysis?"
echo "  1) Anthropic (API key)"
echo "  2) OpenAI (API key)"
echo "  3) Google Vertex AI (ADC — same auth as Claude Code)"
echo "  4) Custom OpenAI-compatible endpoint"
echo ""
read -rp "  Choose [1-4]: " provider_choice

AI_PROVIDER=""
AI_API_KEY=""
AI_PROJECT=""
AI_REGION=""
AI_MODEL=""
AI_BASE_URL=""
GCP_CREDS_PATH=""

case $provider_choice in
  1)
    AI_PROVIDER="anthropic"
    read -rp "  Anthropic API key: " -s AI_API_KEY; echo
    AI_MODEL="claude-sonnet-4-20250514"
    read -rp "  Model [$AI_MODEL]: " custom_model
    AI_MODEL="${custom_model:-$AI_MODEL}"

    echo "  Testing Anthropic connection..."
    TEST=$(curl -s -w "%{http_code}" -o /dev/null "https://api.anthropic.com/v1/messages" \
      -H "x-api-key: $AI_API_KEY" \
      -H "anthropic-version: 2023-06-01" \
      -H "content-type: application/json" \
      -d "{\"model\":\"$AI_MODEL\",\"max_tokens\":5,\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}]}" 2>/dev/null)
    if [ "$TEST" = "200" ]; then
      echo "  ✅ Anthropic API working"
    else
      echo "  ⚠️  Got HTTP $TEST — check your API key. Continuing anyway."
    fi
    ;;
  2)
    AI_PROVIDER="openai"
    read -rp "  OpenAI API key: " -s AI_API_KEY; echo
    AI_MODEL="gpt-4o"
    read -rp "  Model [$AI_MODEL]: " custom_model
    AI_MODEL="${custom_model:-$AI_MODEL}"
    AI_BASE_URL="https://api.openai.com"
    ;;
  3)
    AI_PROVIDER="vertex"
    DEFAULT_PROJECT="${ANTHROPIC_VERTEX_PROJECT_ID:-}"
    DEFAULT_REGION="${CLOUD_ML_REGION:-us-east5}"

    read -rp "  Vertex project ID [$DEFAULT_PROJECT]: " AI_PROJECT
    AI_PROJECT="${AI_PROJECT:-$DEFAULT_PROJECT}"

    read -rp "  Vertex region [$DEFAULT_REGION]: " AI_REGION
    AI_REGION="${AI_REGION:-$DEFAULT_REGION}"

    AI_MODEL="claude-opus-4-6"
    read -rp "  Model [$AI_MODEL]: " custom_model
    AI_MODEL="${custom_model:-$AI_MODEL}"

    DEFAULT_CREDS="$HOME/.config/gcloud/application_default_credentials.json"
    if [ -f "$DEFAULT_CREDS" ]; then
      echo "  Found ADC at $DEFAULT_CREDS"
      GCP_CREDS_PATH="$DEFAULT_CREDS"
    else
      read -rp "  Path to GCP credentials JSON: " GCP_CREDS_PATH
    fi

    if [ ! -f "$GCP_CREDS_PATH" ]; then
      echo "  ❌ Credentials file not found. Run: gcloud auth application-default login"
      exit 1
    fi

    echo "  Testing Vertex connection..."
    TOKEN=$(gcloud auth application-default print-access-token 2>/dev/null || echo "")
    if [ -n "$TOKEN" ]; then
      TEST=$(curl -s -w "%{http_code}" -o /dev/null \
        "https://${AI_REGION}-aiplatform.googleapis.com/v1/projects/${AI_PROJECT}/locations/${AI_REGION}/publishers/anthropic/models/${AI_MODEL}:rawPredict" \
        -H "Authorization: Bearer $TOKEN" \
        -H "Content-Type: application/json" \
        -d "{\"anthropic_version\":\"vertex-2023-10-16\",\"messages\":[{\"role\":\"user\",\"content\":\"hi\"}],\"max_tokens\":5}" 2>/dev/null)
      if [ "$TEST" = "200" ]; then
        echo "  ✅ Vertex AI working ($AI_MODEL)"
      else
        echo "  ⚠️  Got HTTP $TEST — check project/region/credentials. Continuing anyway."
      fi
    else
      echo "  ⚠️  Could not get access token. gcloud may not be installed."
    fi
    ;;
  4)
    AI_PROVIDER="custom"
    read -rp "  API base URL: " AI_BASE_URL
    read -rp "  API key: " -s AI_API_KEY; echo
    read -rp "  Model name: " AI_MODEL
    ;;
  *)
    echo "  Invalid choice"
    exit 1
    ;;
esac
echo ""

# Step 3: GitHub token
echo "🔑 Step 3: GitHub Token"
echo ""
GH_TOKEN=$(gh auth token 2>/dev/null || echo "")
if [ -n "$GH_TOKEN" ]; then
  echo "  Using existing gh CLI token"
else
  read -rp "  GitHub token (needs repo + issues permissions): " -s GH_TOKEN; echo
fi
echo "  ✅ GitHub token ready"
echo ""

# Step 4: Repos
echo "📦 Step 4: Repositories to Monitor"
echo ""
echo "  Enter repos to monitor (comma-separated, e.g. owner/repo1,owner/repo2)"
echo "  The AI will automatically analyze each repo's README, dependencies,"
echo "  and deployment files — no manual configuration needed."
echo ""
read -rp "  Repos: " REPOS
echo ""

# Step 5: Namespace and polling
read -rp "📁 Namespace to deploy in [opinai]: " NAMESPACE
NAMESPACE="${NAMESPACE:-opinai}"

read -rp "⏰ Polling interval in minutes [60]: " INTERVAL
INTERVAL="${INTERVAL:-60}"

echo ""
echo "🚀 Step 5: Deploying to $CLUSTER"
echo ""

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Create namespace
oc create namespace "$NAMESPACE" 2>/dev/null || echo "  Namespace $NAMESPACE already exists"
echo "  ✅ Namespace ready"

# Create secrets
if [ "$AI_PROVIDER" = "vertex" ] && [ -n "$GCP_CREDS_PATH" ]; then
  oc create secret generic opinai-gcp-credentials -n "$NAMESPACE" \
    --from-file=credentials.json="$GCP_CREDS_PATH" \
    --dry-run=client -o yaml | oc apply -f - >/dev/null 2>&1
  echo "  ✅ GCP credentials stored"
fi

oc create secret generic opinai-credentials -n "$NAMESPACE" \
  --from-literal=GITHUB_TOKEN="$GH_TOKEN" \
  --from-literal=AI_API_KEY="${AI_API_KEY:-}" \
  --from-literal=AI_PROVIDER="$AI_PROVIDER" \
  --from-literal=AI_PROJECT="${AI_PROJECT:-}" \
  --from-literal=AI_REGION="${AI_REGION:-}" \
  --from-literal=AI_MODEL="$AI_MODEL" \
  --from-literal=AI_BASE_URL="${AI_BASE_URL:-}" \
  --dry-run=client -o yaml | oc apply -f - >/dev/null 2>&1
echo "  ✅ Credentials stored"

# Create configmap
oc create configmap opinai-config -n "$NAMESPACE" \
  --from-literal=REPOS="$REPOS" \
  --from-literal=POLL_INTERVAL_MINUTES="$INTERVAL" \
  --dry-run=client -o yaml | oc apply -f - >/dev/null 2>&1
echo "  ✅ Configuration stored"

# Apply RBAC
for f in serviceaccount.yaml role.yaml rolebinding.yaml; do
  sed "s/namespace: opinai/namespace: $NAMESPACE/g" "$SCRIPT_DIR/manifests/$f" | oc apply -f - >/dev/null 2>&1
done

# Apply ClusterRole + ClusterRoleBinding for sandbox management
for f in clusterrole.yaml clusterrolebinding.yaml; do
  sed "s/namespace: opinai/namespace: $NAMESPACE/g" "$SCRIPT_DIR/manifests/$f" | oc apply -f - >/dev/null 2>&1
done
echo "  ✅ RBAC configured (namespace + cluster)"

# Create ImageStream + BuildConfig + build
oc create imagestream opinai-controller -n "$NAMESPACE" 2>/dev/null || true
cat << BCEOF | oc apply -f - >/dev/null 2>&1
apiVersion: build.openshift.io/v1
kind: BuildConfig
metadata:
  name: opinai-controller
  namespace: $NAMESPACE
spec:
  source:
    type: Git
    git:
      uri: https://github.com/yossiovadia/opinai.git
      ref: main
    contextDir: controller-go
  strategy:
    type: Docker
    dockerStrategy:
      dockerfilePath: Dockerfile
      noCache: true
  output:
    to:
      kind: ImageStreamTag
      name: opinai-controller:latest
BCEOF

echo "  🔨 Building controller image (this takes ~2 minutes)..."
oc start-build opinai-controller -n "$NAMESPACE" --follow 2>&1 | tail -1
echo "  ✅ Image built"

# Create persistent storage
sed "s/namespace: opinai/namespace: $NAMESPACE/g" "$SCRIPT_DIR/manifests/pvc.yaml" | oc apply -f - >/dev/null 2>&1
echo "  ✅ Persistent storage ready"

# Deploy controller
sed -e "s/namespace: opinai/namespace: $NAMESPACE/g" -e "s|svc:5000/opinai/|svc:5000/$NAMESPACE/|g" "$SCRIPT_DIR/manifests/deployment.yaml" | oc apply -f - >/dev/null 2>&1
echo "  ✅ Controller deployed"

# Apply Service + Route for dashboard
for f in service.yaml route.yaml; do
  sed "s/namespace: opinai/namespace: $NAMESPACE/g" "$SCRIPT_DIR/manifests/$f" | oc apply -f - >/dev/null 2>&1
done
echo "  ✅ Dashboard service + route created"

# Wait for pod
echo "  Waiting for controller to start..."
oc rollout status deployment/opinai-controller -n "$NAMESPACE" --timeout=60s 2>&1 | tail -1
echo ""

# Get dashboard URL
DASHBOARD_URL=$(oc get route opinai-dashboard -n "$NAMESPACE" -o jsonpath='{.spec.host}' 2>/dev/null || echo "")

echo "=============================="
echo "🎳 OpinAI is running!"
echo "=============================="
echo ""
echo "  Cluster:    $CLUSTER"
echo "  Namespace:  $NAMESPACE"
echo "  Monitoring: $REPOS"
echo "  AI:         $AI_PROVIDER ($AI_MODEL)"
echo "  Polling:    every ${INTERVAL}m"
if [ -n "$DASHBOARD_URL" ]; then
  echo ""
  echo "  Dashboard:  https://$DASHBOARD_URL"
fi
echo ""
echo "  View pods:    oc get pods -n $NAMESPACE"
echo "  View logs:    oc logs deployment/opinai-controller -n $NAMESPACE"
echo "  View jobs:    oc get jobs -n $NAMESPACE"
echo ""
echo "  \"That's just, like, your opinion, man.\""
echo ""
