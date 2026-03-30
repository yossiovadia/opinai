#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# AI-powered issue analysis — generates a custom test script from the issue
# Outputs the path to the generated script on stdout
# ---------------------------------------------------------------------------

if [[ -z "${AI_API_KEY:-}" ]]; then
  echo "::error::AI_API_KEY is required for AI analysis" >&2
  exit 1
fi

AI_MODEL="${AI_MODEL:-claude-sonnet-4-20250514}"
AI_BASE_URL="${AI_BASE_URL:-https://api.anthropic.com}"

GENERATED_SCRIPT="$(mktemp "${RESULTS_DIR}/ai_test_XXXXXX.sh")"

# Build the prompt — credentials are NEVER included
PROMPT="$(cat <<PROMPT_EOF
You are OpinAI, an automated bug reproduction system. A user filed this bug report:

Title: ${ISSUE_TITLE}
Body: ${ISSUE_BODY}

The server is running at ${SERVER_URL}.

Your task:
1. Analyze what the bug claims
2. Write a bash test script that would prove or disprove this bug
3. The script should use curl to test the server and output results as JSON files in ${RESULTS_DIR}
4. Each result file should have: {"test": "name", "provider": "...", "status": "pass|fail", "details": "..."}

Output ONLY the bash script, no explanation. The script will be executed directly.
PROMPT_EOF
)"

# JSON-encode the prompt safely
PROMPT_JSON="$(printf '%s' "$PROMPT" | jq -Rsa .)"

# ---------------------------------------------------------------------------
# Call the AI API
# ---------------------------------------------------------------------------

if echo "$AI_BASE_URL" | grep -qi "openai"; then
  # OpenAI-compatible API
  set +x
  response=$(curl -s --max-time 60 "${AI_BASE_URL}/v1/chat/completions" \
    -H "Authorization: Bearer ${AI_API_KEY}" \
    -H "content-type: application/json" \
    -d "{
      \"model\": \"${AI_MODEL}\",
      \"max_tokens\": 4096,
      \"messages\": [{\"role\": \"user\", \"content\": ${PROMPT_JSON}}]
    }" 2>&1) || true
  set -x

  # Extract content from OpenAI response
  script_content="$(printf '%s' "$response" | jq -r '.choices[0].message.content // empty' 2>/dev/null)" || true
else
  # Anthropic Messages API (default)
  set +x
  response=$(curl -s --max-time 60 "${AI_BASE_URL}/v1/messages" \
    -H "x-api-key: ${AI_API_KEY}" \
    -H "anthropic-version: 2023-06-01" \
    -H "content-type: application/json" \
    -d "{
      \"model\": \"${AI_MODEL}\",
      \"max_tokens\": 4096,
      \"messages\": [{\"role\": \"user\", \"content\": ${PROMPT_JSON}}]
    }" 2>&1) || true
  set -x

  # Extract content from Anthropic response
  script_content="$(printf '%s' "$response" | jq -r '.content[0].text // empty' 2>/dev/null)" || true
fi

if [[ -z "${script_content:-}" ]]; then
  echo "::warning::AI returned empty response" >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Sanitize: strip markdown code fences and any leaked credentials
# ---------------------------------------------------------------------------

# Remove ```bash / ``` fences
script_content="$(printf '%s' "$script_content" | sed -E '/^```(bash|sh)?$/d')"

# Strip any accidental API key leaks from the generated script
if [[ -n "${AI_API_KEY:-}" ]]; then
  set +x
  script_content="$(printf '%s' "$script_content" | sed "s/${AI_API_KEY}/REDACTED/g")"
  set -x
fi

printf '%s\n' "$script_content" > "$GENERATED_SCRIPT"
chmod +x "$GENERATED_SCRIPT"

# Output the path (stdout is captured by caller)
echo "$GENERATED_SCRIPT"
