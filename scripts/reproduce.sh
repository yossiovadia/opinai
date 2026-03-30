#!/usr/bin/env bash
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
RESULTS_DIR="$(mktemp -d)"
SERVER_PID=""

cleanup() {
  if [[ -n "${SERVER_PID:-}" ]] && kill -0 "$SERVER_PID" 2>/dev/null; then
    kill "$SERVER_PID" 2>/dev/null || true
    wait "$SERVER_PID" 2>/dev/null || true
  fi
  rm -rf "$RESULTS_DIR"
}
trap cleanup EXIT

# ---------------------------------------------------------------------------
# 1. Fetch the issue body
# ---------------------------------------------------------------------------
echo "::group::Fetching issue #${ISSUE_NUMBER}"
ISSUE_JSON="$(gh api "repos/${GITHUB_REPOSITORY}/issues/${ISSUE_NUMBER}" 2>&1)" || {
  echo "::error::Failed to fetch issue #${ISSUE_NUMBER}"
  exit 1
}
ISSUE_TITLE="$(echo "$ISSUE_JSON" | jq -r '.title')"
ISSUE_BODY="$(echo "$ISSUE_JSON" | jq -r '.body // ""')"
echo "Title: $ISSUE_TITLE"
echo "::endgroup::"

# ---------------------------------------------------------------------------
# 2. Analyze issue text to decide which tests to run
# ---------------------------------------------------------------------------
ISSUE_TEXT="$(echo "$ISSUE_TITLE $ISSUE_BODY" | tr '[:upper:]' '[:lower:]')"

# Determine test scope from issue keywords
TESTS_STREAMING=false
TESTS_AUTH=false
TESTS_TOOLS=false
PROVIDER_FILTER=""

if echo "$ISSUE_TEXT" | grep -qE 'stream|sse|server.sent'; then
  TESTS_STREAMING=true
fi
if echo "$ISSUE_TEXT" | grep -qE 'auth|api.key|token|401|unauthorized'; then
  TESTS_AUTH=true
fi
if echo "$ISSUE_TEXT" | grep -qE 'tool|function.call'; then
  TESTS_TOOLS=true
fi

# Check for specific provider mentions
for p in openai anthropic vertexai bedrock azure_openai; do
  if echo "$ISSUE_TEXT" | grep -qi "$p"; then
    PROVIDER_FILTER="${PROVIDER_FILTER:+$PROVIDER_FILTER,}$p"
  fi
done

# If nothing specific matched, run full suite
if ! $TESTS_STREAMING && ! $TESTS_AUTH && ! $TESTS_TOOLS && [[ -z "$PROVIDER_FILTER" ]]; then
  TESTS_STREAMING=true
  TESTS_AUTH=true
  TESTS_TOOLS=true
fi

echo "Test scope — streaming:$TESTS_STREAMING auth:$TESTS_AUTH tools:$TESTS_TOOLS providers:${PROVIDER_FILTER:-all}"

# ---------------------------------------------------------------------------
# 3. Start the server
# ---------------------------------------------------------------------------
echo "::group::Starting server"
if [[ -z "${SERVER_COMMAND:-}" ]]; then
  echo "No server_command provided — assuming server is already running"
  SERVER_PID=""
else
  eval "$SERVER_COMMAND" &
  SERVER_PID=$!
fi

echo "Waiting ${WAIT_SECONDS}s for server (pid $SERVER_PID)..."
ELAPSED=0
HEALTHY=false
while [ "$ELAPSED" -lt "$WAIT_SECONDS" ]; do
  if curl -sf "${SERVER_URL}/health" >/dev/null 2>&1 || curl -sf "${SERVER_URL}/" >/dev/null 2>&1; then
    echo "Server is healthy after ${ELAPSED}s"
    HEALTHY=true
    break
  fi
  sleep 1
  ELAPSED=$((ELAPSED + 1))
done

if [ "$HEALTHY" != "true" ] && ! curl -sf "${SERVER_URL}/health" >/dev/null 2>&1; then
  echo "::error::Server failed to become healthy within ${WAIT_SECONDS}s"
  exit 1
fi
echo "::endgroup::"

# ---------------------------------------------------------------------------
# 4. Run tests
# ---------------------------------------------------------------------------
export RESULTS_DIR SERVER_URL PROVIDER_FILTER TESTS_STREAMING TESTS_AUTH TESTS_TOOLS
export ISSUE_TITLE ISSUE_BODY

if [[ -n "${AI_API_KEY:-}" ]]; then
  echo "::group::AI-powered analysis"
  AI_SCRIPT="$("$SCRIPT_DIR/ai_analyze.sh")" || true
  if [[ -n "${AI_SCRIPT:-}" && -f "$AI_SCRIPT" ]]; then
    echo "AI generated custom test script"
    export AI_ANALYSIS_USED=true
    chmod +x "$AI_SCRIPT"
    "$AI_SCRIPT"
  else
    echo "::warning::AI analysis failed, falling back to deterministic tests"
    export AI_ANALYSIS_USED=false
    "$SCRIPT_DIR/test_providers.sh"
  fi
  echo "::endgroup::"
else
  export AI_ANALYSIS_USED=false
  if [[ -n "${TEST_SCRIPT:-}" && -f "$TEST_SCRIPT" ]]; then
    echo "::group::Running custom test script: $TEST_SCRIPT"
    chmod +x "$TEST_SCRIPT"
    "$TEST_SCRIPT"
    echo "::endgroup::"
  else
    "$SCRIPT_DIR/test_providers.sh"
  fi
fi

# ---------------------------------------------------------------------------
# 5. Post report
# ---------------------------------------------------------------------------
export ISSUE_NUMBER ISSUE_TITLE GITHUB_REPOSITORY
"$SCRIPT_DIR/report.sh"
