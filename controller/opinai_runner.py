"""OpinAI Runner — runs inside Job pods to reproduce a single issue."""

import json
import logging
import os
import shutil
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


# ---------------------------------------------------------------------------
# Knowledge output — written to stdout for the controller to parse and store
# ---------------------------------------------------------------------------


def _emit_repo_memory(data: dict):
    """Print repo memory as delimited JSON for the controller to extract."""
    print("--- OPINAI REPO MEMORY ---")
    print(json.dumps(data))
    print("--- END REPO MEMORY ---")


def _load_repo_context() -> str:
    """Build context from REPO_MEMORY env var (injected by controller via ConfigMap)."""
    # The controller passes previous knowledge as OPINAI_REPO_CONTEXT env var
    ctx = os.environ.get("OPINAI_REPO_CONTEXT", "")
    return ctx


def _analyze_repo_readme(readme_text: str):
    """Ask AI to analyze the repo README and emit learnings to stdout."""
    if not _ai_available():
        return

    # Check env — controller sets this if knowledge already exists
    if os.environ.get("OPINAI_HAS_KNOWLEDGE", "") == "true":
        log.info("Repo knowledge already exists — skipping README analysis")
        return

    log.info("First run for %s — analyzing README...", REPO)
    prompt = (
        f"Analyze this project README from {REPO}.\n\n"
        f"{readme_text[:3000]}\n\n"
        "Provide a brief JSON summary (no markdown fences, just raw JSON):\n"
        "{\n"
        '  "description": "what this project does in 1-2 sentences",\n'
        '  "tech_stack": "languages and frameworks used",\n'
        '  "how_to_test": "how to test/validate bugs in this project",\n'
        '  "deployment_needs": "what infrastructure is needed to run it"\n'
        "}"
    )

    try:
        content = _call_ai(prompt)
    except Exception as exc:
        log.warning("README analysis failed: %s", exc)
        return

    if not content:
        return

    content = content.strip()
    if content.startswith("```"):
        lines = content.splitlines()
        content = "\n".join(l for l in lines if not l.strip().startswith("```"))

    try:
        data = json.loads(content)
        _emit_repo_memory(data)
        log.info("Emitted repo knowledge for %s", REPO)
    except json.JSONDecodeError:
        _emit_repo_memory({"description": content[:500]})


def _save_run_learnings(verdict: str, confidence: str, category: str):
    """Emit run learnings to stdout for the controller to store."""
    learnings = {
        "last_analyzed_issue": str(ISSUE_NUMBER),
        "last_verdict": verdict,
    }
    if confidence:
        learnings["last_confidence"] = confidence
    _emit_repo_memory(learnings)


def _ai_available() -> bool:
    """Check if any AI provider is configured."""
    if AI_PROVIDER.lower() == "vertex":
        return bool(AI_PROJECT and AI_REGION)
    return bool(AI_API_KEY)


def ai_categorize(title: str, body: str) -> str:
    """Ask the AI to categorize the issue. Returns BUG/FEATURE/QUESTION/DOCS."""
    if not _ai_available():
        return "BUG"  # default to BUG if no AI

    prompt = (
        "You are OpinAI. Categorize this GitHub issue:\n\n"
        f"Title: {title}\n"
        f"Body: {body}\n\n"
        "Categorize this issue: BUG (defect in existing behavior), "
        "FEATURE (request for new functionality), QUESTION (asking for help/clarification), "
        "or DOCS (documentation issue).\n\n"
        "Respond with ONLY one line in this exact format:\n"
        "Category: BUG\n"
        "(or FEATURE, QUESTION, DOCS)"
    )

    try:
        content = _call_ai(prompt)
    except Exception as exc:
        log.error("AI categorization failed: %s", exc)
        return "BUG"

    if not content:
        return "BUG"

    # Parse category from response
    for line in content.upper().splitlines():
        if "CATEGORY:" in line:
            for cat in ("BUG", "FEATURE", "QUESTION", "DOCS"):
                if cat in line:
                    return cat

    # Fallback: check if the response contains the category anywhere
    upper = content.upper()
    for cat in ("FEATURE", "QUESTION", "DOCS"):
        if cat in upper:
            return cat
    return "BUG"


def ai_select_deployment_option(title: str, body: str, options: list[dict]) -> dict | None:
    """Ask the AI to pick the best deployment option for this issue."""
    if not _ai_available() or not options:
        return None

    options_text = "\n".join(
        f"- {opt.get('id', '?')}: {opt.get('name', '?')} — {opt.get('description', '')} "
        f"(best for: {opt.get('best_for', 'general')})"
        for opt in options
    )

    prompt = (
        "You are OpinAI. A user filed this bug report:\n\n"
        f"Title: {title}\n"
        f"Body: {body}\n\n"
        "Available deployment options for reproducing this bug:\n"
        f"{options_text}\n\n"
        "Which deployment option is best for reproducing THIS specific bug? "
        "Consider what the bug affects — if it's a controller/API bug, a lightweight option may suffice. "
        "If it's an integration bug, a full deploy may be needed.\n\n"
        "Respond with EXACTLY this format:\n"
        "Selected: <option_id>\n"
        "Reason: <one sentence explaining why>\n"
    )

    try:
        content = _call_ai(prompt)
    except Exception as exc:
        log.error("AI deployment selection failed: %s", exc)
        return None

    if not content:
        return None

    # Parse selected option ID
    selected_id = None
    reason = ""
    for line in content.splitlines():
        if line.strip().lower().startswith("selected:"):
            selected_id = line.split(":", 1)[1].strip().lower()
        elif line.strip().lower().startswith("reason:"):
            reason = line.split(":", 1)[1].strip()

    if selected_id:
        for opt in options:
            if opt.get("id", "").lower() == selected_id:
                log.info("AI selected deployment option: %s — %s", selected_id, reason)
                print(f"--- OPINAI SELECTED DEPLOYMENT: {selected_id} ---")
                print(f"--- OPINAI DEPLOYMENT REASON: {reason} ---")
                return opt

    # Fallback: pick recommended or first
    for opt in options:
        if opt.get("recommended"):
            return opt
    return options[0] if options else None


def ai_generate_tests(title: str, body: str, profile: dict | None = None) -> str | None:
    """Ask the AI to generate a bash test script for the issue. Returns script text."""
    if not _ai_available():
        log.warning("No AI credentials configured — skipping AI analysis")
        return None

    # Re-read SERVER_URL — _start_server updates the global after import
    current_server_url = os.environ.get("SERVER_URL", "") or SERVER_URL
    server_context = f"\nThe server is already running at {current_server_url}. Do NOT start the server yourself — just test it with curl." if current_server_url else ""
    profile_context = format_profile_context(profile) if profile else ""
    repo_context = _load_repo_context()

    prompt = (
        "You are OpinAI, an automated bug reproduction system. "
        "A user filed this bug report:\n\n"
        f"Title: {title}\n"
        f"Body: {body}\n"
        f"{server_context}"
        f"{profile_context}"
        f"{repo_context}\n\n"
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


def ai_verdict(title: str, body: str, results: str) -> tuple[str | None, str, str]:
    """Ask the AI to summarize the test results. Returns (verdict_text, confidence, verdict_enum)."""
    if not _ai_available():
        return None, "LOW", "ERROR"

    prompt = (
        "You are OpinAI. A user filed this bug report:\n\n"
        f"Title: {title}\n"
        f"Body: {body}\n\n"
        "Here are the test results:\n\n"
        f"{results}\n\n"
        "Give a brief verdict. Use EXACTLY one of these verdicts:\n"
        "- BUG_CONFIRMED — tests prove the bug exists\n"
        "- NOT_A_BUG — tests prove behavior is correct\n"
        "- NOT_REPRODUCIBLE — tests ran but could not trigger the bug\n\n"
        "Include this exact line:\n"
        "Verdict: BUG_CONFIRMED\n"
        "(or NOT_A_BUG or NOT_REPRODUCIBLE)\n\n"
        "Then a one-paragraph summary of what the tests showed.\n\n"
        "Also rate your confidence: HIGH (strong evidence, "
        "clear pass/fail results), MEDIUM (some evidence but ambiguous), "
        "or LOW (mostly guessing, tests may not cover the actual bug).\n\n"
        "Include this exact line:\n"
        "Confidence: HIGH\n"
        "(or MEDIUM or LOW)\n\n"
        "Keep it concise."
    )

    try:
        content = _call_ai(prompt)
    except Exception as exc:
        log.error("AI verdict failed: %s", exc)
        return None, "LOW", "ERROR"

    if not content:
        return None, "LOW", "ERROR"

    # Parse confidence from response
    confidence = "MEDIUM"
    for line in content.upper().splitlines():
        if "CONFIDENCE:" in line:
            if "HIGH" in line:
                confidence = "HIGH"
            elif "LOW" in line:
                confidence = "LOW"
            else:
                confidence = "MEDIUM"
            break

    # Parse verdict enum from response
    verdict_enum = "NOT_REPRODUCIBLE"
    for line in content.upper().splitlines():
        if "VERDICT:" in line:
            if "BUG_CONFIRMED" in line:
                verdict_enum = "BUG_CONFIRMED"
            elif "NOT_A_BUG" in line:
                verdict_enum = "NOT_A_BUG"
            elif "NOT_REPRODUCIBLE" in line:
                verdict_enum = "NOT_REPRODUCIBLE"
            break
    else:
        # Fallback: scan content for keywords
        upper = content.upper()
        if "BUG_CONFIRMED" in upper or "BUG CONFIRMED" in upper:
            verdict_enum = "BUG_CONFIRMED"
        elif "NOT_A_BUG" in upper or "NOT A BUG" in upper:
            verdict_enum = "NOT_A_BUG"

    return content, confidence, verdict_enum


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
    safe_body = sanitize_output(body)
    auto_post = os.environ.get("OPINAI_AUTO_POST", "false").lower() in ("true", "1", "yes")

    # Always write suggested comment to file and stdout for dashboard visibility
    try:
        with open("/tmp/opinai-suggested-comment.md", "w") as f:
            f.write(safe_body)
    except OSError:
        pass

    print("--- OPINAI SUGGESTED COMMENT ---")
    print(safe_body)
    print("--- END SUGGESTED COMMENT ---")

    if not auto_post:
        log.info("Auto-post disabled — comment saved for review (%s#%s)", REPO, ISSUE_NUMBER)
        return

    url = f"{GH_API}/repos/{REPO}/issues/{ISSUE_NUMBER}/comments"
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


MAX_RETRIES = 3


def _try_common_fix(error_output: str, command: str) -> tuple[str | None, str]:
    """Try well-known fixes without AI. Returns (fixed_command, description) or (None, '')."""
    lower = error_output.lower()

    # Permission denied on pip → add --user
    if "permission denied" in lower and "pip install" in command and "--user" not in command:
        return command.replace("pip install", "pip install --user"), "added --user flag (permission denied)"

    # pip not found → use python3 -m pip
    if ("pip: command not found" in lower or "pip3: command not found" in lower) and "python3 -m pip" not in command:
        fixed = command.replace("pip install", "python3 -m pip install").replace("pip3 install", "python3 -m pip install")
        return fixed, "replaced pip with python3 -m pip (command not found)"

    # Command not found after install → PATH issue
    if "command not found" in lower and "pip" not in lower:
        return f"export PATH=/tmp/pip-user/bin:$PATH && {command}", "prepended pip bin to PATH"

    # No module named X → auto-install
    if "no module named" in lower:
        for line in error_output.splitlines():
            if "no module named" in line.lower():
                parts = line.split("'")
                if len(parts) >= 2:
                    module = parts[1]
                    if " " not in module and len(module) < 50:
                        return f"python3 -m pip install --user {module} && {command}", f"auto-installed missing module: {module}"

    return None, ""


def _ask_ai_for_fix(command: str, error_output: str) -> str | None:
    """Ask the AI to diagnose and fix a failed command."""
    if not _ai_available():
        return None

    err_trunc = error_output[-1500:] if len(error_output) > 1500 else error_output
    prompt = (
        f"The reproduction setup failed.\n\n"
        f"Command: {command}\n\n"
        f"Error output (last 1500 chars):\n{err_trunc}\n\n"
        "Diagnose the problem and provide a fixed command that will work in a minimal container "
        "(Debian-based, no root, limited PATH, pip may need --user flag, PYTHONUSERBASE=/tmp/pip-user).\n\n"
        "Respond with ONLY the fixed shell command on a single line. No explanation."
    )

    try:
        reply = _call_ai(prompt)
    except Exception:
        return None

    if not reply:
        return None

    for line in reply.strip().splitlines():
        line = line.strip()
        if line and not line.startswith("```") and not line.startswith("#") and len(line) > 5:
            return line
    return None


def _run_with_retry(command: str, cwd: str, env: dict) -> tuple:
    """Run a command with self-healing retries. Returns (result, retry_count)."""
    current_cmd = command

    for attempt in range(MAX_RETRIES + 1):
        result = subprocess.run(
            current_cmd,
            shell=True,
            cwd=cwd,
            env=env,
            capture_output=True,
            text=True,
            timeout=300,
        )

        if result.returncode == 0:
            return result, attempt

        if attempt == MAX_RETRIES:
            return result, attempt

        error_output = result.stdout + result.stderr
        log.warning("Command failed (attempt %d/%d), trying self-heal...", attempt + 1, MAX_RETRIES)

        # Try common fixes first
        fixed, desc = _try_common_fix(error_output, current_cmd)
        if fixed:
            log.info("Applying common fix: %s", desc)
            current_cmd = fixed
            continue

        # Ask AI
        ai_fix = _ask_ai_for_fix(current_cmd, error_output)
        if ai_fix and ai_fix != current_cmd:
            log.info("Applying AI fix: %s", ai_fix[:100])
            current_cmd = ai_fix
            continue

        # No fix found
        return result, attempt

    return result, MAX_RETRIES


def _start_server(profile: dict) -> subprocess.Popen | None:
    """Install and start the target server inside the pod using profile config."""
    global SERVER_URL

    build_cmd = profile.get("build", "")
    run_cmd = profile.get("run", "")

    # Clone the repo first
    log.info("Cloning %s...", REPO)
    clone_dir = "/tmp/opinai-repo"
    result = subprocess.run(
        ["git", "clone", "--depth=1", f"https://github.com/{REPO}.git", clone_dir],
        capture_output=True,
        text=True,
        timeout=120,
    )
    if result.returncode != 0:
        log.warning("Clone failed: %s", result.stderr[:500])
        return None

    # Read README and analyze on first run
    readme_path = os.path.join(clone_dir, "README.md")
    if not os.path.exists(readme_path):
        readme_path = os.path.join(clone_dir, "readme.md")
    if os.path.exists(readme_path):
        try:
            with open(readme_path) as f:
                readme_text = f.read()[:3000]
            _analyze_repo_readme(readme_text)
        except OSError:
            pass

    # Build env with pip bin dirs on PATH
    env = os.environ.copy()
    pip_bin = os.path.dirname(shutil.which("python3") or "/usr/local/bin/python3")
    env["PYTHONUSERBASE"] = "/tmp/pip-user"
    env["PATH"] = f"/tmp/pip-user/bin:/usr/local/bin:/root/.local/bin:{pip_bin}:{env.get('PATH', '')}"

    # Install / build with self-healing retries
    if build_cmd:
        resolved_build_cmd = build_cmd.replace("pip install", "python3 -m pip install --user")
        log.info("Installing: %s", resolved_build_cmd)
        result, retries = _run_with_retry(resolved_build_cmd, clone_dir, env)
        if result.returncode != 0:
            log.warning("Build failed after %d retries (exit %d): %s",
                        retries, result.returncode, result.stderr[:500])
        elif retries > 0:
            log.info("Build succeeded after %d retries", retries)

    if not run_cmd:
        return None

    # Start the server with retry on startup failures
    log.info("Starting server: %s", run_cmd)
    current_run_cmd = run_cmd
    server_proc = None
    for srv_attempt in range(MAX_RETRIES + 1):
        try:
            server_proc = subprocess.Popen(
                current_run_cmd,
                shell=True,
                cwd=clone_dir,
                env=env,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.PIPE,
            )
            # Give it a moment to crash
            time.sleep(1)
            if server_proc.poll() is not None:
                stderr_out = server_proc.stderr.read().decode("utf-8", errors="replace")[:500]
                raise RuntimeError(f"Server exited immediately: {stderr_out}")
            break  # server is running
        except Exception as exc:
            if srv_attempt == MAX_RETRIES:
                log.warning("Server start failed after %d retries: %s", MAX_RETRIES, exc)
                return None
            error_str = str(exc)
            fixed, desc = _try_common_fix(error_str, current_run_cmd)
            if fixed:
                log.info("Server fix: %s", desc)
                current_run_cmd = fixed
            elif "not found" in error_str.lower():
                current_run_cmd = f"export PATH=/tmp/pip-user/bin:$PATH && {run_cmd}"
                log.info("Server fix: prepended pip bin to PATH")
            else:
                log.warning("Server start failed, no fix found: %s", exc)
                return None

    # Derive SERVER_URL from health check URL
    health_url = profile.get("health", "")
    if health_url:
        # Strip path to get base URL (e.g. http://localhost:8000/health -> http://localhost:8000)
        parts = health_url.split("/")
        if len(parts) >= 3:
            SERVER_URL = "/".join(parts[:3])
        else:
            SERVER_URL = health_url
    else:
        health_url = "http://localhost:8000/health"
        SERVER_URL = "http://localhost:8000"

    os.environ["SERVER_URL"] = SERVER_URL

    # Wait for health check
    log.info("Waiting for server health at %s...", health_url)
    for i in range(30):
        try:
            r = requests.get(health_url, timeout=2)
            if r.status_code < 500:
                log.info("Server healthy after %ds", i)
                return server_proc
        except (requests.ConnectionError, requests.Timeout):
            pass
        if server_proc.poll() is not None:
            log.warning("Server process exited with code %d", server_proc.returncode)
            return None
        time.sleep(1)

    log.warning("Server did not become healthy within 30s — continuing anyway")
    return server_proc


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

    # Step 2: Check for sandbox or start server in pod
    global SERVER_URL
    SERVER_URL = os.environ.get("SERVER_URL", "")
    profile = load_repo_profile()
    server_proc = None
    sandbox_ns = os.environ.get("OPINAI_SANDBOX_NAMESPACE", "")
    sandbox_endpoints = os.environ.get("OPINAI_SANDBOX_ENDPOINTS", "")

    if sandbox_ns:
        # Sandbox is already deployed by the controller — use it
        log.info("Using sandbox deployment in namespace %s", sandbox_ns)
        if sandbox_endpoints:
            try:
                endpoints = json.loads(sandbox_endpoints)
                if endpoints:
                    first_svc = next(iter(endpoints.values()))
                    SERVER_URL = f"http://{first_svc}"
                    os.environ["SERVER_URL"] = SERVER_URL
                    log.info("Server URL from sandbox: %s", SERVER_URL)
            except (json.JSONDecodeError, StopIteration):
                pass
    elif profile:
        log.info("Loaded repo profile: type=%s", profile.get("type", "?"))
        server_proc = _start_server(profile)

    try:
        # Step 3: Categorize the issue
        category = ai_categorize(title, body)
        log.info("Category: %s", category)
        print(f"--- OPINAI CATEGORY: {category} ---")

        if category in ("FEATURE", "QUESTION", "DOCS"):
            verdict_enum = "FEATURE_REQUEST"
            print(f"--- OPINAI VERDICT: {verdict_enum} ---")
            cat_labels = {
                "FEATURE": "feature request",
                "QUESTION": "question / help request",
                "DOCS": "documentation issue",
            }
            comment = (
                "## OpinAI Bug Reproduction Report\n\n"
                f"**Issue:** #{ISSUE_NUMBER}\n"
                f"**Category:** {category}\n"
                f"**Verdict:** {verdict_enum}\n"
                f"**Analysis:** AI-powered (model: {AI_MODEL})\n\n"
                f"This appears to be a **{cat_labels[category]}**, "
                "not a reproducible bug. Skipping reproduction.\n\n"
                "---\n"
                '*"That\'s just, like, your opinion, man." '
                "-- [OpinAI](https://github.com/yossiovadia/opinai)*"
            )
            post_comment(comment)
            add_label()
            _save_run_learnings(verdict_enum, "", category)
            log.info("Skipped reproduction — verdict: %s", verdict_enum)
            return

        # Step 4: AI generates test script
        script_text = ai_generate_tests(title, body, profile=profile)
        if not script_text:
            print("--- OPINAI VERDICT: ERROR ---")
            comment = (
                "## OpinAI Bug Reproduction Report\n\n"
                f"**Issue:** #{ISSUE_NUMBER}\n"
                f"**Category:** {category}\n"
                "**Verdict:** ERROR\n"
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

        # Step 5: Execute tests
        log.info("Running AI-generated tests...")
        test_output = run_tests(script_text)
        log.info("Tests completed (%d bytes of output)", len(test_output))

        # Step 6: AI verdict with confidence
        verdict_text, confidence, verdict_enum = ai_verdict(title, body, test_output)
        verdict_section = verdict_text if verdict_text else "AI verdict unavailable."
        log.info("Verdict: %s, Confidence: %s", verdict_enum, confidence)
        print(f"--- OPINAI VERDICT: {verdict_enum} ---")
        print(f"--- OPINAI CONFIDENCE: {confidence} ---")

        # Step 7: Build and post report
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

        server_info = f"**Server:** `{SERVER_URL}`\n" if SERVER_URL else ""

        comment = (
            "## OpinAI Bug Reproduction Report\n\n"
            f"**Issue:** #{ISSUE_NUMBER}\n"
            f"**Category:** {category}\n"
            f"**Verdict:** {verdict_enum}\n"
            f"**Confidence:** {confidence}\n"
            f"{server_info}"
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
        _save_run_learnings(verdict_enum, confidence, category)
        log.info("Done — reproduction complete for %s#%s", REPO, ISSUE_NUMBER)
    finally:
        # Cleanup: kill server process
        if server_proc and server_proc.poll() is None:
            log.info("Terminating server process")
            server_proc.terminate()
            try:
                server_proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                server_proc.kill()


if __name__ == "__main__":
    main()
