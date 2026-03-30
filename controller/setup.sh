#!/bin/bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

echo "🎳 OpinAI Controller Setup"
echo "=========================="
echo ""
echo "Which AI provider do you want to use for bug analysis?"
echo ""
echo "  1) Anthropic (API key)"
echo "  2) OpenAI (API key)"
echo "  3) Google Vertex AI (service account)"
echo "  4) Custom OpenAI-compatible endpoint"
echo ""
read -rp "Choose [1-4]: " provider_choice

case $provider_choice in
  1)
    read -rp "Anthropic API key: " -s api_key; echo
    base_url="https://api.anthropic.com"
    model="claude-sonnet-4-20250514"
    ;;
  2)
    read -rp "OpenAI API key: " -s api_key; echo
    base_url="https://api.openai.com"
    model="gpt-4o"
    ;;
  3)
    read -rp "Vertex AI project ID: " project_id
    read -rp "Vertex AI region (e.g. us-east5): " region
    read -rp "Path to service account key JSON: " sa_key_path
    if [[ ! -f "$sa_key_path" ]]; then
      echo "Error: file not found: $sa_key_path"
      exit 1
    fi
    api_key=$(base64 < "$sa_key_path")
    base_url="https://${region}-aiplatform.googleapis.com"
    model="claude-opus-4-6"
    ;;
  4)
    read -rp "API base URL: " base_url
    read -rp "API key: " -s api_key; echo
    read -rp "Model name: " model
    ;;
  *)
    echo "Invalid choice"
    exit 1
    ;;
esac

read -rp "GitHub token (needs repo + issues permissions): " -s gh_token; echo

echo ""
echo "Which repos should OpinAI monitor? (comma-separated)"
echo "Example: owner/repo1,owner/repo2"
read -rp "Repos: " repos

# ---------------------------------------------------------------------------
# Collect project profiles for each repo
# ---------------------------------------------------------------------------
repo_profiles=""
IFS=',' read -ra repo_array <<< "$repos"
for repo in "${repo_array[@]}"; do
  repo="$(echo "$repo" | xargs)"  # trim whitespace
  echo ""
  echo "📚 Project Profile for ${repo}:"
  echo "   Help OpinAI understand your project so it can reproduce bugs better."
  echo ""
  read -rp "   What type of project? (api-server/cli/operator/library/other): " project_type
  read -rp "   How to install/build? (e.g. pip install -e ., make build, go build): " build_command
  read -rp "   How to run/start? (e.g. python -m myapp --port 8000, ./bin/server): " run_command
  read -rp "   Health check URL (if applicable, e.g. http://localhost:8000/health): " health_url
  read -rp "   Needs GPU? (y/n) [n]: " needs_gpu
  needs_gpu=${needs_gpu:-n}
  read -rp "   Needs OpenShift/K8s? (y/n) [n]: " needs_k8s
  needs_k8s=${needs_k8s:-n}
  read -rp "   Any special dependencies? (e.g. Redis, PostgreSQL, Ollama): " dependencies
  echo ""

  # Convert y/n to true/false
  [[ "$needs_gpu" =~ ^[Yy] ]] && gpu_bool="true" || gpu_bool="false"
  [[ "$needs_k8s" =~ ^[Yy] ]] && k8s_bool="true" || k8s_bool="false"

  # Build the config key: owner/repo -> owner_repo
  repo_key="$(echo "$repo" | tr '/' '_' | tr '-' '_')"

  # Escape double quotes in user input for JSON safety
  build_command="${build_command//\"/\\\"}"
  run_command="${run_command//\"/\\\"}"
  health_url="${health_url//\"/\\\"}"
  dependencies="${dependencies//\"/\\\"}"

  profile_json="{\"type\":\"${project_type}\",\"build\":\"${build_command}\",\"run\":\"${run_command}\",\"health\":\"${health_url}\",\"gpu\":${gpu_bool},\"k8s\":${k8s_bool},\"deps\":\"${dependencies}\"}"
  repo_profiles="${repo_profiles}  REPO_PROFILE_${repo_key}: |\n    ${profile_json}\n"
done

read -rp "Polling interval in minutes [60]: " interval
interval=${interval:-60}

read -rp "Namespace to deploy in [opinai]: " namespace
namespace=${namespace:-opinai}

echo ""
echo "Generating manifests..."

# Create the secret (credentials are written to a gitignored file)
set +x
cat > manifests/secret.yaml << SECRETEOF
apiVersion: v1
kind: Secret
metadata:
  name: opinai-credentials
  namespace: ${namespace}
type: Opaque
stringData:
  GITHUB_TOKEN: "${gh_token}"
  AI_API_KEY: "${api_key}"
  AI_BASE_URL: "${base_url}"
  AI_MODEL: "${model}"
SECRETEOF
set -x

# Update configmap with repos, interval, and repo profiles
cat > manifests/configmap.yaml << CMEOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: opinai-config
  namespace: ${namespace}
data:
  REPOS: "${repos}"
  POLL_INTERVAL_MINUTES: "${interval}"
  DONE_LABEL: "opinai-done"
$(echo -e "${repo_profiles}")CMEOF

# Update namespace in all manifests
if [[ "$namespace" != "opinai" ]]; then
  sed -i.bak "s/namespace: opinai/namespace: ${namespace}/g" manifests/*.yaml
  rm -f manifests/*.yaml.bak
fi

echo ""
echo "Setup complete!"
echo ""
echo "Deploy with:"
echo "  kubectl apply -f manifests/namespace.yaml"
echo "  kubectl apply -f manifests/"
echo ""
echo "WARNING: manifests/secret.yaml contains your credentials — do NOT commit it!"
echo "         It's already in .gitignore"
