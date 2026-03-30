"""OpinAI Runner — runs inside Job pods to reproduce a single issue."""

import json
import logging
import os
import subprocess
import sys
import tempfile
import time

import requests

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%Y-%m-%dT%H:%M:%S",
)
log = logging.getLogger("opinai-runner")

# ---------------------------------------------------------------------------
# Config from env
# ---------------------------------------------------------------------------

REPO = os.environ.get("REPO", "")
ISSUE_NUMBER = os.environ.get("ISSUE_NUMBER", "")
GITHUB_TOKEN = os.environ.get("GITHUB_TOKEN", "")
AI_API_KEY = os.environ.get("AI_API_KEY", "")
AI_MODEL = os.environ.get("AI_MODEL", "claude-sonnet-4-20250514")
AI_BASE_URL = os.environ.get("AI_BASE_URL", "https://api.anthropic.com")
AI_PROVIDER = os.environ.get("AI_PROVIDER", "")
AI_PROJECT = os.environ.get("AI_PROJECT", "")
AI_REGION = os.environ.get("AI_REGION", "")
SERVER_URL = os.environ.get("SERVER_URL", "")
DONE_LABEL = os.environ.get("DONE_LABEL", "opinai-done")

GH_API = "https://api.github.com"


def load_repo_profile() -> dict | None:
    """Load the repo profile from the REPO_PROFILE_<key> env var."""
    repo_key = REPO.replace("/", "_").replace("-", "_").replace(".", "_")
    raw = os.environ.get(f"REPO_PROFILE_{repo_key}", "")
    if not raw.strip():
        return None
    try:
        return json.loads(raw.strip())
    except json.JSONDecodeError:
        log.warning("Failed to parse repo profile for %s", REPO)
        return None


def format_profile_context(profile: dict) -> str:
    """Format the repo profile as context for the AI prompt."""
    gpu = "Yes" if profile.get("gpu") else "No"
    k8s = "Yes" if profile.get("k8s") else "No"
    return (
        "\nProject Profile:\n"
        f"- Type: {profile.get('type', 'unknown')}\n"
        f"- Install: {profile.get('build', 'unknown')}\n"
        f"- Run: {profile.get('run', 'unknown')}\n"
        f"- Health check: {profile.get('health', 'unknown')}\n"
        f"- Needs GPU: {gpu}\n"
        f"- Needs Kubernetes: {k8s}\n"
        f"- Dependencies: {profile.get('deps', 'none')}\n"
        "\nUse this to properly install and start the server before testing. "
        "If the project needs Kubernetes, use kubectl/oc commands. "
        "If it is an API server, start it and use curl.\n"
    )


def gh_headers():
    return {
        "Accept": "application/vnd.github+json",
        "Authorization": f"Bearer {GITHUB_TOKEN}",
        "X-GitHub-Api-Version": "2022-11-28",
    }


# ---------------------------------------------------------------------------
# Step 1 — Fetch issue
# ---------------------------------------------------------------------------


def fetch_issue() -> dict:
    url = f"{GH_API}/repos/{REPO}/issues/{ISSUE_NUMBER}"
    resp = requests.get(url, headers=gh_headers(), timeout=30)
    resp.raise_for_status()
    return resp.json()


# ---------------------------------------------------------------------------
# Step 2 — AI analysis: generate test script
# ---------------------------------------------------------------------------


def _ai_available() -> bool:
    """Check if any AI provider is configured."""
    if AI_PROVIDER.lower() == "vertex":
        return bool(AI_PROJECT and AI_REGION)
    return bool(AI_API_KEY)


def ai_generate_tests(title: str, body: str, profile: dict | None = None) -> str | None:
    """Ask the AI to generate a bash test script for the issue. Returns script text."""
    if not _ai_available():
        log.warning("No AI credentials configured — skipping AI analysis")
        return None

    server_context = f"\nThe server is running at {SERVER_URL}." if SERVER_URL else ""
    profile_context = format_profile_context(profile) if profile else ""

    prompt = (
        "You are OpinAI, an automated bug reproduction system. "
        "A user filed this bug report:\n\n"
        f"Title: {title}\n"
        f"Body: {body}\n"
        f"{server_context}"
        f"{profile_context}\n\n"
        "Your task:\n"
        "1. Analyze what the bug claims\n"
        "2. Write a bash test script that would prove or disprove this bug\n"
        "3. The script should use curl to test endpoints and capture results\n"
        "4. Print each test result as a JSON line: "
        '{"test": "name", "status": "pass|fail", "details": "..."}\n\n'
        "Output ONLY the bash script, no explanation."
    )

    try:
        content = _call_ai(prompt)
    except Exception as exc:
        log.error("AI analysis failed: %s", exc)
        return None

    if not content:
        return None

    # Strip markdown code fences
    lines = content.splitlines()
    cleaned = []
    for line in lines:
        stripped = line.strip()
        if stripped.startswith("```"):
            continue
        cleaned.append(line)

    return "\n".join(cleaned)


# ---------------------------------------------------------------------------
# Step 4 — AI verdict on results
# ---------------------------------------------------------------------------


def ai_verdict(title: str, body: str, results: str) -> str | None:
    """Ask the AI to summarize the test results."""
    if not _ai_available():
        return None

    prompt = (
        "You are OpinAI. A user filed this bug report:\n\n"
        f"Title: {title}\n"
        f"Body: {body}\n\n"
        "Here are the test results:\n\n"
        f"{results}\n\n"
        "Give a brief verdict:\n"
        "1. Is the bug confirmed, not reproduced, or inconclusive?\n"
        "2. One-paragraph summary of what the tests showed.\n"
        "Keep it concise."
    )

    try:
        return _call_ai(prompt)
    except Exception as exc:
        log.error("AI verdict failed: %s", exc)
        return None


# ---------------------------------------------------------------------------
# AI API call (shared)
# ---------------------------------------------------------------------------


def _get_vertex_access_token() -> str:
    """Get a Google access token from ADC for Vertex AI. Never log the token."""
    import google.auth
    import google.auth.transport.requests as google_requests

    scopes = ["https://www.googleapis.com/auth/cloud-platform"]
    credentials, _ = google.auth.default(scopes=scopes)
    credentials.refresh(google_requests.Request())
    return credentials.token


def _call_ai(prompt: str) -> str | None:
    """Call the AI API and return the text response."""
    if AI_PROVIDER.lower() == "vertex":
        # Google Vertex AI — Claude via rawPredict
        access_token = _get_vertex_access_token()
        url = (
            f"https://{AI_REGION}-aiplatform.googleapis.com/v1/"
            f"projects/{AI_PROJECT}/locations/{AI_REGION}/"
            f"publishers/anthropic/models/{AI_MODEL}:rawPredict"
        )
        # Credentials are in headers — never log
        headers = {
            "Authorization": f"Bearer {access_token}",
            "Content-Type": "application/json",
        }
        payload = {
            "anthropic_version": "vertex-2023-10-16",
            "messages": [{"role": "user", "content": prompt}],
            "max_tokens": 4096,
        }
        resp = requests.post(url, headers=headers, json=payload, timeout=120)
        resp.raise_for_status()
        data = resp.json()
        content_blocks = data.get("content", [])
        if content_blocks:
            return content_blocks[0].get("text")
        return None

    if "openai" in AI_BASE_URL.lower():
        url = f"{AI_BASE_URL}/v1/chat/completions"
        headers = {
            "Authorization": f"Bearer {AI_API_KEY}",
            "Content-Type": "application/json",
        }
        payload = {
            "model": AI_MODEL,
            "max_tokens": 4096,
            "messages": [{"role": "user", "content": prompt}],
        }
    else:
        # Anthropic Messages API
        url = f"{AI_BASE_URL}/v1/messages"
        headers = {
            "x-api-key": AI_API_KEY,
            "anthropic-version": "2023-06-01",
            "Content-Type": "application/json",
        }
        payload = {
            "model": AI_MODEL,
            "max_tokens": 4096,
            "messages": [{"role": "user", "content": prompt}],
        }

    # Credentials are in headers — never log the request
    resp = requests.post(url, headers=headers, json=payload, timeout=120)
    resp.raise_for_status()
    data = resp.json()

    if "openai" in AI_BASE_URL.lower():
        return data.get("choices", [{}])[0].get("message", {}).get("content")
    else:
        content_blocks = data.get("content", [])
        if content_blocks:
            return content_blocks[0].get("text")
    return None


# ---------------------------------------------------------------------------
# Step 3 — Execute test script
# ---------------------------------------------------------------------------


def run_tests(script_text: str) -> str:
    """Write script to a temp file, execute it, return stdout."""
    with tempfile.NamedTemporaryFile(
        mode="w", suffix=".sh", delete=False, prefix="opinai_test_"
    ) as f:
        f.write("#!/usr/bin/env bash\nset -euo pipefail\n\n")
        f.write(script_text)
        script_path = f.name

    os.chmod(script_path, 0o755)

    try:
        result = subprocess.run(
            ["/bin/bash", script_path],
            capture_output=True,
            text=True,
            timeout=300,
        )
        output = result.stdout
        if result.returncode != 0:
            output += f"\n[script exited with code {result.returncode}]\n"
            if result.stderr:
                output += f"[stderr: {result.stderr[:1000]}]\n"
        return output
    except subprocess.TimeoutExpired:
        return "[ERROR] Test script timed out after 300s"
    finally:
        os.unlink(script_path)


# ---------------------------------------------------------------------------
# Step 5 — Post report as GitHub comment
# ---------------------------------------------------------------------------


def sanitize_output(text: str) -> str:
    """Remove any accidental credential leaks from text before posting."""
    sanitized = text
    for secret in (AI_API_KEY, GITHUB_TOKEN):
        if secret and len(secret) > 8:
            sanitized = sanitized.replace(secret, "REDACTED")
    return sanitized


def post_comment(body: str):
    url = f"{GH_API}/repos/{REPO}/issues/{ISSUE_NUMBER}/comments"
    safe_body = sanitize_output(body)
    resp = requests.post(
        url, headers=gh_headers(), json={"body": safe_body}, timeout=30
    )
    if resp.ok:
        log.info("Posted comment to %s#%s", REPO, ISSUE_NUMBER)
    else:
        log.error("Failed to post comment: %s %s", resp.status_code, resp.text[:200])


def add_label():
    url = f"{GH_API}/repos/{REPO}/issues/{ISSUE_NUMBER}/labels"
    resp = requests.post(
        url, headers=gh_headers(), json={"labels": [DONE_LABEL]}, timeout=30
    )
    if resp.ok:
        log.info("Added label '%s' to %s#%s", DONE_LABEL, REPO, ISSUE_NUMBER)
    else:
        log.error("Failed to add label: %s %s", resp.status_code, resp.text[:200])


# ---------------------------------------------------------------------------
# Main
# ---------------------------------------------------------------------------


def main():
    if not REPO or not ISSUE_NUMBER:
        log.error("REPO and ISSUE_NUMBER env vars are required")
        sys.exit(1)
    if not GITHUB_TOKEN:
        log.error("GITHUB_TOKEN env var is required")
        sys.exit(1)

    log.info("Starting reproduction for %s#%s", REPO, ISSUE_NUMBER)

    # Step 1: Fetch issue
    try:
        issue = fetch_issue()
    except requests.RequestException as exc:
        log.error("Failed to fetch issue: %s", exc)
        sys.exit(1)

    title = issue.get("title", "")
    body = issue.get("body", "") or ""
    log.info("Issue: %s", title)

    # Load repo profile from env
    profile = load_repo_profile()
    if profile:
        log.info("Loaded repo profile: type=%s", profile.get("type", "?"))

    # Step 2: AI generates test script
    script_text = ai_generate_tests(title, body, profile=profile)
    if not script_text:
        comment = (
            "## OpinAI Bug Reproduction Report\n\n"
            f"**Issue:** #{ISSUE_NUMBER}\n"
            "**Analysis:** Skipped (no AI API key or AI analysis failed)\n\n"
            "Could not generate tests for this issue. "
            "Configure an AI API key for automated analysis.\n\n"
            "---\n"
            '*"That\'s just, like, your opinion, man." '
            "-- [OpinAI](https://github.com/yossiovadia/opinai)*"
        )
        post_comment(comment)
        add_label()
        return

    log.info("AI generated test script (%d bytes)", len(script_text))

    # Step 3: Execute tests
    log.info("Running AI-generated tests...")
    test_output = run_tests(script_text)
    log.info("Tests completed (%d bytes of output)", len(test_output))

    # Step 4: AI verdict
    verdict_text = ai_verdict(title, body, test_output)
    verdict_section = verdict_text if verdict_text else "AI verdict unavailable."

    # Step 5: Build and post report
    # Parse JSONL results from test output
    results_table = ""
    for line in test_output.splitlines():
        line = line.strip()
        if not line.startswith("{"):
            continue
        try:
            r = json.loads(line)
            status = r.get("status", "?")
            icon = {"pass": "PASS", "fail": "FAIL"}.get(status, status.upper())
            results_table += (
                f"| {r.get('test', '?')} | {icon} | {r.get('details', '')} |\n"
            )
        except json.JSONDecodeError:
            continue

    if not results_table:
        results_table = "| (no structured results) | - | - |\n"

    comment = (
        "## OpinAI Bug Reproduction Report\n\n"
        f"**Issue:** #{ISSUE_NUMBER}\n"
        f"**Analysis:** AI-powered (model: {AI_MODEL})\n\n"
        "### Results\n\n"
        "| Test | Status | Details |\n"
        "|------|--------|---------|\n"
        f"{results_table}\n"
        "### Verdict\n\n"
        f"{verdict_section}\n\n"
        "<details><summary>Raw test output</summary>\n\n"
        f"```\n{test_output[:5000]}\n```\n\n"
        "</details>\n\n"
        "---\n"
        '*"That\'s just, like, your opinion, man." '
        "-- [OpinAI](https://github.com/yossiovadia/opinai)*"
    )

    post_comment(comment)
    add_label()
    log.info("Done — reproduction complete for %s#%s", REPO, ISSUE_NUMBER)


if __name__ == "__main__":
    main()
