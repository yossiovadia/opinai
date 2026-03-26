#!/usr/bin/env bash
set -euo pipefail

# ---------------------------------------------------------------------------
# Formats test results and posts a GitHub issue comment
# ---------------------------------------------------------------------------

PASS="$(cat "${RESULTS_DIR}/pass_count")"
FAIL="$(cat "${RESULTS_DIR}/fail_count")"
TOTAL="$(cat "${RESULTS_DIR}/total_count")"
RESULTS_FILE="${RESULTS_DIR}/results.jsonl"
TIMESTAMP="$(date -u +%Y-%m-%dT%H:%M:%SZ)"

# ---------------------------------------------------------------------------
# Build the results table
# ---------------------------------------------------------------------------

TABLE_ROWS=""
while IFS= read -r line; do
  test_name="$(echo "$line" | jq -r '.test')"
  provider="$(echo "$line" | jq -r '.provider')"
  status="$(echo "$line" | jq -r '.status')"
  details="$(echo "$line" | jq -r '.details')"

  case "$status" in
    pass) icon="✅ Pass" ;;
    fail) icon="🔴 Fail" ;;
    skip) icon="⏭️ Skip" ;;
    *)    icon="❓ $status" ;;
  esac

  TABLE_ROWS="${TABLE_ROWS}| ${test_name} | ${provider} | ${icon} | ${details} |
"
done < "$RESULTS_FILE"

# ---------------------------------------------------------------------------
# Build the evidence section for failures
# ---------------------------------------------------------------------------

EVIDENCE=""
while IFS= read -r line; do
  status="$(echo "$line" | jq -r '.status')"
  if [[ "$status" != "fail" ]]; then
    continue
  fi

  test_name="$(echo "$line" | jq -r '.test')"
  provider="$(echo "$line" | jq -r '.provider')"
  details="$(echo "$line" | jq -r '.details')"
  request="$(echo "$line" | jq -r '.request')"
  response="$(echo "$line" | jq -r '.response')"

  EVIDENCE="${EVIDENCE}
**${test_name} (${provider}):**
- **Details:** ${details}
- **Request:** \`${request}\`
- **Response:** \`${response:-(empty)}\`
"
done < "$RESULTS_FILE"

# ---------------------------------------------------------------------------
# Verdict
# ---------------------------------------------------------------------------

if (( FAIL == 0 )); then
  VERDICT="✅ **All tests passed** — ${PASS} of ${TOTAL} tests passed"
else
  VERDICT="🔴 **Bug Confirmed** — ${FAIL} of ${TOTAL} tests failed"
fi

# ---------------------------------------------------------------------------
# Compose the comment
# ---------------------------------------------------------------------------

COMMENT="## 🎳 OpinAI — Bug Reproduction Report

**Issue:** #${ISSUE_NUMBER}
**Server:** llm-katan (echo mode)
**Timestamp:** ${TIMESTAMP}

### Results

| Test | Provider | Status | Details |
|------|----------|--------|---------|
${TABLE_ROWS}
### Verdict
${VERDICT}
"

if [[ -n "$EVIDENCE" ]]; then
  COMMENT="${COMMENT}
### Evidence
<details><summary>Failing test details</summary>

${EVIDENCE}
</details>
"
fi

COMMENT="${COMMENT}
---
*\"That's just, like, your opinion, man.\" — [OpinAI](https://github.com/yossiovadia/opinai)*"

# ---------------------------------------------------------------------------
# Post the comment
# ---------------------------------------------------------------------------

echo "::group::Posting report to issue #${ISSUE_NUMBER}"
echo "$COMMENT"

gh api "repos/${GITHUB_REPOSITORY}/issues/${ISSUE_NUMBER}/comments" \
  -f body="$COMMENT" \
  --silent || echo "::warning::Failed to post comment (token may lack permissions)"

echo "::endgroup::"

# Fail the action if any test failed
if (( FAIL > 0 )); then
  echo "::error::${FAIL} test(s) failed"
  exit 1
fi
