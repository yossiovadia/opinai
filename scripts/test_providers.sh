#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# Protocol compliance test suite for LLM API servers
# Tests each provider endpoint for spec-correct request/response behavior
# ---------------------------------------------------------------------------

PASS=0
FAIL=0
SKIP=0
RESULTS_FILE="${RESULTS_DIR}/results.jsonl"

# ---------------------------------------------------------------------------
# Helpers
# ---------------------------------------------------------------------------

record() {
  local test_name="$1" provider="$2" status="$3" details="$4"
  local request="${5:-}" response="${6:-}"
  # Escape quotes and newlines for JSON-safe output
  local safe_req safe_resp
  safe_req="$(echo "$request" | tr '"' "'" | tr '\n' ' ' | cut -c1-500)"
  safe_resp="$(echo "$response" | tr '"' "'" | tr '\n' ' ' | cut -c1-500)"
  printf '{"test":"%s","provider":"%s","status":"%s","details":"%s","request":"%s","response":"%s"}\n' \
    "$test_name" "$provider" "$status" "$details" "$safe_req" "$safe_resp" \
    >> "$RESULTS_FILE"

  case "$status" in
    pass) (( PASS++ )) || true ;;
    fail) (( FAIL++ )) || true ;;
    skip) (( SKIP++ )) || true ;;
  esac
}

should_test_provider() {
  local provider="$1"
  if [[ -z "${PROVIDER_FILTER:-}" ]]; then
    return 0
  fi
  echo ",$PROVIDER_FILTER," | grep -qi ",$provider," || return 1
}

# ---------------------------------------------------------------------------
# Common tests
# ---------------------------------------------------------------------------

test_health() {
  local resp
  resp="$(curl -s -o /dev/null -w '%{http_code}' "${SERVER_URL}/health" 2>&1)" || resp="000"
  if [[ "$resp" == "200" ]]; then
    record "Health endpoint" "common" "pass" "Returns 200"
  else
    record "Health endpoint" "common" "fail" "Expected 200, got $resp"
  fi
}

test_health

# ---------------------------------------------------------------------------
# OpenAI tests
# ---------------------------------------------------------------------------

if should_test_provider "openai"; then
  OPENAI_URL="${SERVER_URL}/v1/chat/completions"
  OPENAI_BODY='{"model":"test","messages":[{"role":"user","content":"hello"}]}'

  # --- Basic completion ---
  resp="$(curl -s --max-time 10 -X POST "$OPENAI_URL" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer test-key" \
    -d "$OPENAI_BODY" 2>&1)" || resp=""

  if [[ -n "$resp" ]] && echo "$resp" | jq -e '.' >/dev/null 2>&1; then
    missing=""
    for field in id object choices usage; do
      if ! echo "$resp" | jq -e ".$field" >/dev/null 2>&1; then
        missing="${missing:+$missing, }$field"
      fi
    done
    obj="$(echo "$resp" | jq -r '.object // ""')"
    if [[ -z "$missing" && "$obj" == "chat.completion" ]]; then
      record "Response schema" "openai" "pass" "All required fields present" \
        "POST /v1/chat/completions" "$resp"
    else
      record "Response schema" "openai" "fail" "Missing: ${missing:-object=$obj}" \
        "POST /v1/chat/completions" "$resp"
    fi
  else
    record "Response schema" "openai" "fail" "No valid JSON response" \
      "POST /v1/chat/completions"
  fi

  # --- Streaming ---
  if [[ "$TESTS_STREAMING" == "true" ]]; then
    stream_body='{"model":"test","messages":[{"role":"user","content":"hello"}],"stream":true}'
    stream_resp="$(curl -s --max-time 10 -X POST "$OPENAI_URL" \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer test-key" \
      -d "$stream_body" 2>&1)" || stream_resp=""

    if echo "$stream_resp" | grep -q 'data: \[DONE\]'; then
      record "Streaming SSE" "openai" "pass" "Stream ends with data: [DONE]" \
        "POST /v1/chat/completions {stream:true}"
    else
      record "Streaming SSE" "openai" "fail" "Missing data: [DONE] terminator" \
        "POST /v1/chat/completions {stream:true}"
    fi
  fi

  # --- Auth required ---
  if [[ "$TESTS_AUTH" == "true" ]]; then
    auth_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "$OPENAI_URL" \
      -H "Content-Type: application/json" \
      -d "$OPENAI_BODY" 2>&1)" || auth_code="000"

    if [[ "$auth_code" == "401" ]]; then
      record "Auth required" "openai" "pass" "401 returned without Authorization header"
    else
      record "Auth required" "openai" "fail" "Expected 401 without auth, got $auth_code"
    fi
  fi

  # --- Invalid JSON ---
  bad_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "$OPENAI_URL" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer test-key" \
    -d '{invalid json' 2>&1)" || bad_code="000"

  if [[ "$bad_code" =~ ^4[0-9][0-9]$ ]]; then
    record "Invalid JSON" "openai" "pass" "Returns ${bad_code} for malformed JSON"
  else
    record "Invalid JSON" "openai" "fail" "Expected 4xx for bad JSON, got $bad_code"
  fi

  # --- Content-Type header ---
  ct_headers="$(curl -s -D - -o /dev/null --max-time 10 -X POST "$OPENAI_URL" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer test-key" \
    -d "$OPENAI_BODY" 2>&1)" || ct_headers=""
  ct="$(echo "$ct_headers" | grep -i '^content-type:' | head -1)" || ct=""

  if echo "$ct" | grep -qi 'application/json'; then
    record "Content-Type header" "openai" "pass" "Response has application/json content-type"
  else
    record "Content-Type header" "openai" "fail" "Expected application/json, got: $ct"
  fi
fi

# ---------------------------------------------------------------------------
# Anthropic tests
# ---------------------------------------------------------------------------

if should_test_provider "anthropic"; then
  ANTHROPIC_URL="${SERVER_URL}/v1/messages"
  ANTHROPIC_BODY='{"model":"test","max_tokens":100,"messages":[{"role":"user","content":"hello"}]}'

  # --- Basic completion ---
  resp="$(curl -s --max-time 10 -X POST "$ANTHROPIC_URL" \
    -H "Content-Type: application/json" \
    -H "x-api-key: test-key" \
    -H "anthropic-version: 2023-06-01" \
    -d "$ANTHROPIC_BODY" 2>&1)" || resp=""

  if [[ -n "$resp" ]] && echo "$resp" | jq -e '.' >/dev/null 2>&1; then
    missing=""
    for field in id type content stop_reason usage; do
      if ! echo "$resp" | jq -e ".$field" >/dev/null 2>&1; then
        missing="${missing:+$missing, }$field"
      fi
    done
    rtype="$(echo "$resp" | jq -r '.type // ""')"
    if [[ -z "$missing" && "$rtype" == "message" ]]; then
      record "Response schema" "anthropic" "pass" "All required fields present" \
        "POST /v1/messages" "$resp"
    else
      record "Response schema" "anthropic" "fail" "Missing: ${missing:-type=$rtype}" \
        "POST /v1/messages" "$resp"
    fi
  else
    record "Response schema" "anthropic" "fail" "No valid JSON response" \
      "POST /v1/messages"
  fi

  # --- Requires x-api-key ---
  if [[ "$TESTS_AUTH" == "true" ]]; then
    auth_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "$ANTHROPIC_URL" \
      -H "Content-Type: application/json" \
      -H "anthropic-version: 2023-06-01" \
      -d "$ANTHROPIC_BODY" 2>&1)" || auth_code="000"

    if [[ "$auth_code" == "401" ]]; then
      record "x-api-key required" "anthropic" "pass" "401 returned without x-api-key"
    else
      record "x-api-key required" "anthropic" "fail" "Expected 401, got $auth_code"
    fi
  fi

  # --- Streaming ---
  if [[ "$TESTS_STREAMING" == "true" ]]; then
    stream_body='{"model":"test","max_tokens":100,"messages":[{"role":"user","content":"hello"}],"stream":true}'
    stream_resp="$(curl -s --max-time 10 -X POST "$ANTHROPIC_URL" \
      -H "Content-Type: application/json" \
      -H "x-api-key: test-key" \
      -H "anthropic-version: 2023-06-01" \
      -d "$stream_body" 2>&1)" || stream_resp=""

    if echo "$stream_resp" | grep -q 'event: message_stop'; then
      record "Streaming SSE" "anthropic" "pass" "Stream contains message_stop event" \
        "POST /v1/messages {stream:true}"
    else
      record "Streaming SSE" "anthropic" "fail" "Missing message_stop event" \
        "POST /v1/messages {stream:true}"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# Bedrock tests
# ---------------------------------------------------------------------------

if should_test_provider "bedrock"; then
  # --- Converse endpoint ---
  BEDROCK_CONVERSE_URL="${SERVER_URL}/model/test/converse"
  BEDROCK_CONVERSE_BODY='{"messages":[{"role":"user","content":[{"text":"hello"}]}]}'

  resp="$(curl -s --max-time 10 -X POST "$BEDROCK_CONVERSE_URL" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer test" \
    -d "$BEDROCK_CONVERSE_BODY" 2>&1)" || resp=""

  if [[ -n "$resp" ]] && echo "$resp" | jq -e '.' >/dev/null 2>&1; then
    has_output="$(echo "$resp" | jq -e '.output' >/dev/null 2>&1 && echo yes || echo no)"
    has_usage="$(echo "$resp" | jq -e '.usage' >/dev/null 2>&1 && echo yes || echo no)"
    if [[ "$has_output" == "yes" && "$has_usage" == "yes" ]]; then
      record "Converse schema" "bedrock" "pass" "output and usage present" \
        "POST /model/test/converse" "$resp"
    else
      record "Converse schema" "bedrock" "fail" "Missing output or usage" \
        "POST /model/test/converse" "$resp"
    fi
  else
    record "Converse schema" "bedrock" "fail" "No valid JSON response" \
      "POST /model/test/converse"
  fi

  # --- Invoke endpoint (Anthropic Claude family) ---
  BEDROCK_INVOKE_URL="${SERVER_URL}/model/anthropic.claude-v2/invoke"
  BEDROCK_INVOKE_BODY='{"messages":[{"role":"user","content":"hello"}],"max_tokens":100,"anthropic_version":"bedrock-2023-05-31"}'

  resp="$(curl -s --max-time 10 -X POST "$BEDROCK_INVOKE_URL" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer test" \
    -d "$BEDROCK_INVOKE_BODY" 2>&1)" || resp=""

  if [[ -n "$resp" ]] && echo "$resp" | jq -e '.' >/dev/null 2>&1; then
    if echo "$resp" | jq -e '.content' >/dev/null 2>&1 || echo "$resp" | jq -e '.completion' >/dev/null 2>&1; then
      record "Invoke schema (claude)" "bedrock" "pass" "Response has content/completion field" \
        "POST /model/anthropic.claude-v2/invoke" "$resp"
    else
      record "Invoke schema (claude)" "bedrock" "fail" "Missing content or completion field" \
        "POST /model/anthropic.claude-v2/invoke" "$resp"
    fi
  else
    record "Invoke schema (claude)" "bedrock" "fail" "No valid JSON response" \
      "POST /model/anthropic.claude-v2/invoke"
  fi

  # --- Auth required ---
  if [[ "$TESTS_AUTH" == "true" ]]; then
    auth_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "$BEDROCK_CONVERSE_URL" \
      -H "Content-Type: application/json" \
      -d "$BEDROCK_CONVERSE_BODY" 2>&1)" || auth_code="000"

    if [[ "$auth_code" == "401" ]]; then
      record "Auth required" "bedrock" "pass" "401 returned without Authorization header"
    else
      record "Auth required" "bedrock" "fail" "Expected 401, got $auth_code"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# Vertex AI tests
# ---------------------------------------------------------------------------

if should_test_provider "vertexai"; then
  VERTEX_URL="${SERVER_URL}/v1beta/models/test:generateContent"
  VERTEX_BODY='{"contents":[{"role":"user","parts":[{"text":"hello"}]}]}'

  resp="$(curl -s --max-time 10 -X POST "$VERTEX_URL" \
    -H "Content-Type: application/json" \
    -H "Authorization: Bearer test" \
    -d "$VERTEX_BODY" 2>&1)" || resp=""

  if [[ -n "$resp" ]] && echo "$resp" | jq -e '.' >/dev/null 2>&1; then
    has_candidates="$(echo "$resp" | jq -e '.candidates' >/dev/null 2>&1 && echo yes || echo no)"
    if [[ "$has_candidates" == "yes" ]]; then
      record "Response schema" "vertexai" "pass" "candidates field present" \
        "POST /v1beta/models/test:generateContent" "$resp"
    else
      record "Response schema" "vertexai" "fail" "Missing candidates field" \
        "POST /v1beta/models/test:generateContent" "$resp"
    fi
  else
    record "Response schema" "vertexai" "fail" "No valid JSON response" \
      "POST /v1beta/models/test:generateContent"
  fi

  # --- Streaming ---
  if [[ "$TESTS_STREAMING" == "true" ]]; then
    VERTEX_STREAM_URL="${SERVER_URL}/v1beta/models/test:streamGenerateContent"
    stream_resp="$(curl -s --max-time 10 -X POST "$VERTEX_STREAM_URL" \
      -H "Content-Type: application/json" \
      -H "Authorization: Bearer test" \
      -d "$VERTEX_BODY" 2>&1)" || stream_resp=""

    if echo "$stream_resp" | grep -q '"candidates"'; then
      record "Streaming" "vertexai" "pass" "Stream contains candidates" \
        "POST /v1beta/models/test:streamGenerateContent"
    else
      record "Streaming" "vertexai" "fail" "No candidates in stream response" \
        "POST /v1beta/models/test:streamGenerateContent"
    fi
  fi

  # --- Auth required ---
  if [[ "$TESTS_AUTH" == "true" ]]; then
    auth_code="$(curl -s -o /dev/null -w '%{http_code}' --max-time 10 -X POST "$VERTEX_URL" \
      -H "Content-Type: application/json" \
      -d "$VERTEX_BODY" 2>&1)" || auth_code="000"

    if [[ "$auth_code" == "401" ]]; then
      record "Auth required" "vertexai" "pass" "401 returned without Authorization header"
    else
      record "Auth required" "vertexai" "fail" "Expected 401, got $auth_code"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# Azure OpenAI tests
# ---------------------------------------------------------------------------

if should_test_provider "azure_openai"; then
  AZURE_URL="${SERVER_URL}/openai/deployments/test/chat/completions?api-version=2024-02-01"
  AZURE_BODY='{"messages":[{"role":"user","content":"hello"}]}'

  resp="$(curl -s --max-time 10 -X POST "$AZURE_URL" \
    -H "Content-Type: application/json" \
    -H "api-key: test-key" \
    -d "$AZURE_BODY" 2>&1)" || resp=""

  if [[ -n "$resp" ]] && echo "$resp" | jq -e '.' >/dev/null 2>&1; then
    missing=""
    for field in id object choices usage; do
      if ! echo "$resp" | jq -e ".$field" >/dev/null 2>&1; then
        missing="${missing:+$missing, }$field"
      fi
    done
    if [[ -z "$missing" ]]; then
      record "Response schema" "azure_openai" "pass" "All required fields present" \
        "POST /openai/deployments/test/chat/completions" "$resp"
    else
      record "Response schema" "azure_openai" "fail" "Missing: $missing" \
        "POST /openai/deployments/test/chat/completions" "$resp"
    fi
  else
    record "Response schema" "azure_openai" "fail" "No valid JSON response" \
      "POST /openai/deployments/test/chat/completions"
  fi

  # --- Streaming ---
  if [[ "$TESTS_STREAMING" == "true" ]]; then
    stream_body='{"messages":[{"role":"user","content":"hello"}],"stream":true}'
    stream_resp="$(curl -s --max-time 10 -X POST "$AZURE_URL" \
      -H "Content-Type: application/json" \
      -H "api-key: test-key" \
      -d "$stream_body" 2>&1)" || stream_resp=""

    if echo "$stream_resp" | grep -q 'data: \[DONE\]'; then
      record "Streaming SSE" "azure_openai" "pass" "Stream ends with data: [DONE]" \
        "POST /openai/deployments/test/chat/completions {stream:true}"
    else
      record "Streaming SSE" "azure_openai" "fail" "Missing data: [DONE] terminator" \
        "POST /openai/deployments/test/chat/completions {stream:true}"
    fi
  fi
fi

# ---------------------------------------------------------------------------
# Summary
# ---------------------------------------------------------------------------

TOTAL=$(( PASS + FAIL + SKIP ))
echo ""
echo "===== Test Summary ====="
echo "Total: $TOTAL  Pass: $PASS  Fail: $FAIL  Skip: $SKIP"
echo "========================"

# Export for report.sh
echo "$PASS" > "${RESULTS_DIR}/pass_count"
echo "$FAIL" > "${RESULTS_DIR}/fail_count"
echo "$TOTAL" > "${RESULTS_DIR}/total_count"
