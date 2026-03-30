"""OpinAI Dashboard — web UI served alongside the controller."""

import json
import logging
import os
import ssl
import subprocess
import sys
import threading
import time

from flask import Flask, jsonify, request, send_from_directory

log = logging.getLogger("opinai-dashboard")

CERT_DIR = "/tmp/opinai-certs"
STATIC_DIR = os.path.join(os.path.dirname(__file__), "static")

# In-memory log buffer for admin page
_log_buffer = []
_log_buffer_lock = threading.Lock()
_LOG_BUFFER_MAX = 200


class DashboardLogHandler(logging.Handler):
    """Captures log lines into a buffer for the admin page."""

    def emit(self, record):
        line = self.format(record)
        with _log_buffer_lock:
            _log_buffer.append(line)
            if len(_log_buffer) > _LOG_BUFFER_MAX:
                del _log_buffer[: len(_log_buffer) - _LOG_BUFFER_MAX]


def install_log_handler():
    """Install the buffer handler on the root logger."""
    handler = DashboardLogHandler()
    handler.setFormatter(logging.Formatter(
        "%(asctime)s [%(levelname)s] %(name)s: %(message)s",
        datefmt="%Y-%m-%dT%H:%M:%S",
    ))
    logging.getLogger().addHandler(handler)

# Shared state — written by the controller, read by the dashboard
_state = {
    "start_time": time.time(),
    "last_poll": None,
    "poll_count": 0,
    "repos": {},       # repo -> {pending: int, processed: int, last_check: str}
    "runs": [],        # [{repo, issue, title, verdict, ai, duration, timestamp, report}]
    "check_now": False,
    "check_now_result": None,  # {total: int} set by controller after check completes
}

# Callback set by the controller for creating reproduction jobs
_reproduce_callback = None
_state_lock = threading.Lock()


def get_state():
    with _state_lock:
        return _state.copy()


def update_state(key, value):
    with _state_lock:
        _state[key] = value


def append_run(run: dict):
    with _state_lock:
        _state["runs"].insert(0, run)
        if len(_state["runs"]) > 200:
            _state["runs"] = _state["runs"][:200]


def update_repo_stats(repo: str, pending: int, processed: int):
    with _state_lock:
        _state["repos"][repo] = {
            "pending": pending,
            "processed": processed,
            "last_check": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        }


def set_check_result(total_new: int):
    with _state_lock:
        _state["check_now_result"] = {"total": total_new}


def should_check_now() -> bool:
    with _state_lock:
        if _state["check_now"]:
            _state["check_now"] = False
            return True
        return False


def set_reproduce_callback(callback):
    """Register the controller's create_job function for dashboard use."""
    global _reproduce_callback
    _reproduce_callback = callback


def _generate_certs():
    """Generate self-signed TLS cert if not present."""
    os.makedirs(CERT_DIR, exist_ok=True)
    cert_path = os.path.join(CERT_DIR, "cert.pem")
    key_path = os.path.join(CERT_DIR, "key.pem")
    if not os.path.exists(cert_path):
        log.info("Generating self-signed TLS certificate")
        subprocess.run(
            [
                "openssl", "req", "-x509", "-newkey", "rsa:2048",
                "-keyout", key_path,
                "-out", cert_path,
                "-days", "365", "-nodes",
                "-subj", "/CN=opinai-controller",
            ],
            check=True,
            capture_output=True,
        )
    return cert_path, key_path


def _create_app() -> Flask:
    app = Flask(__name__, static_folder=STATIC_DIR)

    @app.route("/")
    def index():
        return send_from_directory(STATIC_DIR, "index.html")

    @app.route("/style.css")
    def style():
        return send_from_directory(STATIC_DIR, "style.css")

    @app.route("/health")
    def health():
        return "ok"

    @app.route("/api/status")
    def api_status():
        state = get_state()
        uptime = time.time() - state["start_time"]
        return jsonify({
            "uptime_seconds": int(uptime),
            "uptime_human": _format_duration(uptime),
            "last_poll": state["last_poll"],
            "poll_count": state["poll_count"],
            "repos_count": len(state["repos"]),
        })

    @app.route("/api/repos")
    def api_repos():
        state = get_state()
        repos = []
        for name, stats in state["repos"].items():
            repos.append({"name": name, **stats})
        return jsonify(repos)

    @app.route("/api/runs")
    def api_runs():
        state = get_state()
        limit = request.args.get("limit", 50, type=int)
        return jsonify(state["runs"][:limit])

    @app.route("/api/jobs")
    def api_jobs():
        jobs = _get_k8s_jobs()
        return jsonify(jobs)

    @app.route("/api/check-now", methods=["POST"])
    def api_check_now():
        # Clear any previous result, set flag
        with _state_lock:
            _state["check_now"] = True
            _state["check_now_result"] = None
        return jsonify({"status": "triggered"})

    @app.route("/api/check-now/result", methods=["GET"])
    def api_check_now_result():
        """Poll for check-now result after triggering."""
        with _state_lock:
            result = _state.get("check_now_result")
        if result is not None:
            return jsonify({"status": "done", **result})
        return jsonify({"status": "pending"})

    @app.route("/api/rerun/<path:repo>/<int:issue>", methods=["POST"])
    def api_rerun(repo, issue):
        result = _rerun_issue(repo, issue)
        return jsonify(result)

    @app.route("/api/reproduce", methods=["POST"])
    def api_reproduce():
        data = request.get_json()
        repo = data.get("repo", "")
        issue_number = data.get("issue_number")
        if not repo or not issue_number:
            return jsonify({"status": "error", "message": "repo and issue_number required"}), 400
        return jsonify(_reproduce_issue(repo, int(issue_number)))

    @app.route("/api/chat", methods=["POST"])
    def api_chat():
        data = request.get_json()
        message = data.get("message", "")
        context = data.get("context", {})
        if not message.strip():
            return jsonify({"reply": "Please send a message."}), 400
        reply = _handle_chat(message, context)
        return jsonify({"reply": reply})

    # ----- Admin routes -----

    @app.route("/admin")
    def admin_page():
        return send_from_directory(STATIC_DIR, "admin.html")

    @app.route("/api/admin/repos", methods=["GET"])
    def admin_repos_get():
        repos, profiles = _admin_read_config()
        result = []
        for r in repos:
            key = r.replace("/", "_").replace("-", "_").replace(".", "_")
            profile = profiles.get(f"REPO_PROFILE_{key}")
            parsed = {}
            if profile:
                try:
                    parsed = json.loads(profile)
                except json.JSONDecodeError:
                    pass
            result.append({"name": r, "profile": parsed})
        return jsonify(result)

    @app.route("/api/admin/repos", methods=["POST"])
    def admin_repos_add():
        data = request.get_json()
        repo = data.get("name", "").strip()
        profile = data.get("profile", {})
        if not repo:
            return jsonify({"error": "name required"}), 400
        _admin_update_repo(repo, profile, delete=False)
        return jsonify({"status": "added", "name": repo})

    @app.route("/api/admin/repos", methods=["PUT"])
    def admin_repos_update():
        data = request.get_json()
        repo = data.get("name", "").strip()
        profile = data.get("profile", {})
        if not repo:
            return jsonify({"error": "name required"}), 400
        _admin_update_repo(repo, profile, delete=False)
        return jsonify({"status": "updated", "name": repo})

    @app.route("/api/admin/repos", methods=["DELETE"])
    def admin_repos_delete():
        data = request.get_json()
        repo = data.get("name", "").strip()
        if not repo:
            return jsonify({"error": "name required"}), 400
        _admin_update_repo(repo, {}, delete=True)
        return jsonify({"status": "deleted", "name": repo})

    @app.route("/api/admin/settings", methods=["GET"])
    def admin_settings_get():
        return jsonify({
            "poll_interval_minutes": os.environ.get("POLL_INTERVAL_MINUTES", "60"),
            "done_label": os.environ.get("DONE_LABEL", "opinai-done"),
            "ai_provider": os.environ.get("AI_PROVIDER", ""),
            "ai_model": os.environ.get("AI_MODEL", ""),
            "ai_region": os.environ.get("AI_REGION", ""),
            "ai_project": os.environ.get("AI_PROJECT", ""),
            "namespace": os.environ.get("NAMESPACE", "opinai"),
        })

    @app.route("/api/admin/settings", methods=["PUT"])
    def admin_settings_put():
        data = request.get_json()
        _admin_update_settings(data)
        return jsonify({"status": "updated"})

    @app.route("/api/admin/test-ai", methods=["POST"])
    def admin_test_ai():
        try:
            reply = _call_ai_chat("You are OpinAI. Respond with exactly: OpinAI is online.", "Say hi")
            return jsonify({"status": "ok", "reply": reply})
        except Exception as exc:
            return jsonify({"status": "error", "message": str(exc)})

    @app.route("/api/admin/test-github", methods=["POST"])
    def admin_test_github():
        import requests as req
        gh_token = os.environ.get("GITHUB_TOKEN", "")
        headers = {
            "Accept": "application/vnd.github+json",
            "Authorization": f"Bearer {gh_token}",
            "X-GitHub-Api-Version": "2022-11-28",
        }
        try:
            resp = req.get("https://api.github.com/user", headers=headers, timeout=10)
            if resp.ok:
                user = resp.json()
                return jsonify({"status": "ok", "login": user.get("login", "?")})
            return jsonify({"status": "error", "message": f"HTTP {resp.status_code}"})
        except Exception as exc:
            return jsonify({"status": "error", "message": str(exc)})

    @app.route("/api/admin/logs", methods=["GET"])
    def admin_logs():
        count = request.args.get("count", 50, type=int)
        with _log_buffer_lock:
            lines = _log_buffer[-count:]
        return jsonify(lines)

    @app.route("/api/admin/system", methods=["GET"])
    def admin_system():
        state = get_state()
        uptime = time.time() - state["start_time"]
        return jsonify({
            "namespace": os.environ.get("NAMESPACE", "opinai"),
            "pod_name": os.environ.get("HOSTNAME", "unknown"),
            "uptime_human": _format_duration(uptime),
            "uptime_seconds": int(uptime),
            "image": "opinai-controller:latest",
            "python": sys.version.split()[0] if "sys" in dir() else "?",
        })

    return app


def _format_duration(seconds: float) -> str:
    s = int(seconds)
    if s < 60:
        return f"{s}s"
    if s < 3600:
        return f"{s // 60}m {s % 60}s"
    h = s // 3600
    m = (s % 3600) // 60
    return f"{h}h {m}m"


def _get_k8s_jobs() -> list[dict]:
    """Fetch opinai-runner jobs from Kubernetes."""
    try:
        from kubernetes import client
        batch_api = client.BatchV1Api()
        namespace = os.environ.get("NAMESPACE", "opinai")
        jobs = batch_api.list_namespaced_job(
            namespace=namespace, label_selector="app=opinai-runner"
        )
        result = []
        for job in jobs.items:
            name = job.metadata.name
            created = job.metadata.creation_timestamp
            status = "Running"
            if job.status.succeeded:
                status = "Completed"
            elif job.status.failed:
                status = "Failed"

            duration = ""
            if job.status.start_time and job.status.completion_time:
                delta = (job.status.completion_time - job.status.start_time).total_seconds()
                duration = _format_duration(delta)
            elif job.status.start_time:
                delta = (time.time() - job.status.start_time.timestamp())
                duration = _format_duration(delta)

            # Extract repo + issue from labels
            labels = job.metadata.labels or {}
            repo_label = labels.get("opinai/repo", "")
            issue_label = labels.get("opinai/issue", "")

            # Get pod logs (last 20 lines)
            pod_logs = ""
            try:
                core_api = client.CoreV1Api()
                pods = core_api.list_namespaced_pod(
                    namespace=namespace,
                    label_selector=f"job-name={name}",
                )
                if pods.items:
                    pod_name = pods.items[0].metadata.name
                    pod_logs = core_api.read_namespaced_pod_log(
                        name=pod_name,
                        namespace=namespace,
                        tail_lines=20,
                    )
            except Exception:
                pod_logs = "(logs unavailable)"

            result.append({
                "name": name,
                "status": status,
                "repo": repo_label,
                "issue": issue_label,
                "created": created.isoformat() if created else "",
                "duration": duration,
                "logs": pod_logs,
            })
        return result
    except Exception as exc:
        log.error("Failed to fetch jobs: %s", exc)
        return []


def _rerun_issue(repo: str, issue: int) -> dict:
    """Remove done label and delete existing job to allow re-run."""
    try:
        import requests as req
        from kubernetes import client
        from kubernetes.client.rest import ApiException

        gh_token = os.environ.get("GITHUB_TOKEN", "")
        done_label = os.environ.get("DONE_LABEL", "opinai-done")
        namespace = os.environ.get("NAMESPACE", "opinai")

        # Remove label from GitHub issue
        headers = {
            "Accept": "application/vnd.github+json",
            "Authorization": f"Bearer {gh_token}",
            "X-GitHub-Api-Version": "2022-11-28",
        }
        req.delete(
            f"https://api.github.com/repos/{repo}/issues/{issue}/labels/{done_label}",
            headers=headers,
            timeout=15,
        )

        # Delete existing job
        batch_api = client.BatchV1Api()
        repo_safe = repo.replace("/", "-").lower()
        name = f"opinai-{repo_safe}-{issue}"
        try:
            batch_api.delete_namespaced_job(
                name=name,
                namespace=namespace,
                propagation_policy="Background",
            )
        except ApiException as exc:
            if exc.status != 404:
                raise

        return {"status": "rerun_triggered", "repo": repo, "issue": issue}
    except Exception as exc:
        log.error("Rerun failed for %s#%d: %s", repo, issue, exc)
        return {"status": "error", "message": str(exc)}


def _fetch_issue(repo: str, issue_number: int) -> dict:
    """Fetch issue details from GitHub API."""
    import requests as req
    gh_token = os.environ.get("GITHUB_TOKEN", "")
    headers = {
        "Accept": "application/vnd.github+json",
        "Authorization": f"Bearer {gh_token}",
        "X-GitHub-Api-Version": "2022-11-28",
    }
    try:
        resp = req.get(
            f"https://api.github.com/repos/{repo}/issues/{issue_number}",
            headers=headers,
            timeout=15,
        )
        resp.raise_for_status()
        return resp.json()
    except Exception as exc:
        log.error("Failed to fetch issue %s#%d: %s", repo, issue_number, exc)
        return {}


def _get_repo_profile(repo: str) -> dict | None:
    """Load repo profile from env vars."""
    repo_key = repo.replace("/", "_").replace("-", "_").replace(".", "_")
    raw = os.environ.get(f"REPO_PROFILE_{repo_key}", "")
    if not raw.strip():
        return None
    try:
        return json.loads(raw.strip())
    except json.JSONDecodeError:
        return None


def _call_ai_chat(system_context: str, user_message: str) -> str:
    """Call the AI API for chat. Reuses the same auth pattern as opinai_runner."""
    import requests as req

    ai_provider = os.environ.get("AI_PROVIDER", "").lower()
    ai_api_key = os.environ.get("AI_API_KEY", "")
    ai_model = os.environ.get("AI_MODEL", "claude-sonnet-4-20250514")
    ai_base_url = os.environ.get("AI_BASE_URL", "https://api.anthropic.com")
    ai_project = os.environ.get("AI_PROJECT", "")
    ai_region = os.environ.get("AI_REGION", "")

    messages = [
        {"role": "user", "content": f"{system_context}\n\nUser question: {user_message}"},
    ]

    try:
        if ai_provider == "vertex":
            import google.auth
            import google.auth.transport.requests as google_requests
            scopes = ["https://www.googleapis.com/auth/cloud-platform"]
            credentials, _ = google.auth.default(scopes=scopes)
            credentials.refresh(google_requests.Request())
            access_token = credentials.token

            url = (
                f"https://{ai_region}-aiplatform.googleapis.com/v1/"
                f"projects/{ai_project}/locations/{ai_region}/"
                f"publishers/anthropic/models/{ai_model}:rawPredict"
            )
            headers = {
                "Authorization": f"Bearer {access_token}",
                "Content-Type": "application/json",
            }
            payload = {
                "anthropic_version": "vertex-2023-10-16",
                "messages": messages,
                "max_tokens": 2048,
            }
        elif "openai" in ai_base_url.lower():
            url = f"{ai_base_url}/v1/chat/completions"
            headers = {
                "Authorization": f"Bearer {ai_api_key}",
                "Content-Type": "application/json",
            }
            payload = {
                "model": ai_model,
                "max_tokens": 2048,
                "messages": messages,
            }
        else:
            url = f"{ai_base_url}/v1/messages"
            headers = {
                "x-api-key": ai_api_key,
                "anthropic-version": "2023-06-01",
                "Content-Type": "application/json",
            }
            payload = {
                "model": ai_model,
                "max_tokens": 2048,
                "messages": messages,
            }

        # Credentials in headers — never log the request
        resp = req.post(url, headers=headers, json=payload, timeout=120)
        resp.raise_for_status()
        data = resp.json()

        if ai_provider == "vertex" or (ai_provider != "openai" and "openai" not in ai_base_url.lower()):
            blocks = data.get("content", [])
            return blocks[0].get("text", "") if blocks else "No response from AI."
        else:
            return data.get("choices", [{}])[0].get("message", {}).get("content", "No response from AI.")
    except Exception as exc:
        log.error("Chat AI call failed: %s", exc)
        return f"Sorry, I couldn't reach the AI service: {exc}"


def _handle_chat(message: str, context: dict) -> str:
    """Build context and call AI for the chat feature."""
    system_context = (
        "You are OpinAI, an AI bug reproduction assistant running on a Kubernetes cluster. "
        "You help developers understand bugs, analyze reproduction results, and suggest fixes. "
        "Be concise, technical, and helpful. Use markdown formatting.\n\n"
    )

    repo = context.get("repo")
    issue_number = context.get("issue_number")

    if repo and issue_number:
        issue = _fetch_issue(repo, int(issue_number))
        if issue:
            system_context += f"Current issue: {repo}#{issue_number}\n"
            system_context += f"Title: {issue.get('title', '')}\n"
            body = issue.get("body", "") or ""
            system_context += f"Body: {body[:2000]}\n\n"

        # Include previous reproduction results
        state = get_state()
        for run in state.get("runs", []):
            if str(run.get("repo")) == str(repo) and str(run.get("issue")) == str(issue_number):
                system_context += f"Previous reproduction result:\n{(run.get('report') or '')[:1000]}\n\n"
                break

        # Include repo profile
        profile = _get_repo_profile(repo)
        if profile:
            system_context += f"Project profile: {json.dumps(profile)}\n\n"
    else:
        # General context — include current dashboard state
        state = get_state()
        repos = list(state.get("repos", {}).keys())
        system_context += f"Monitored repos: {', '.join(repos) if repos else 'none configured'}\n"
        system_context += f"Total runs: {len(state.get('runs', []))}\n"
        pending = sum(r.get("pending", 0) for r in state.get("repos", {}).values())
        system_context += f"Pending issues: {pending}\n\n"

    # Sanitize AI response — strip any leaked credentials
    reply = _call_ai_chat(system_context, message)
    for secret_key in ("AI_API_KEY", "GITHUB_TOKEN"):
        secret = os.environ.get(secret_key, "")
        if secret and len(secret) > 8:
            reply = reply.replace(secret, "REDACTED")
    return reply


def _admin_read_config() -> tuple[list[str], dict]:
    """Read repos list and profiles from the Kubernetes ConfigMap."""
    try:
        from kubernetes import client
        v1 = client.CoreV1Api()
        namespace = os.environ.get("NAMESPACE", "opinai")
        cm = v1.read_namespaced_config_map("opinai-config", namespace)
        data = cm.data or {}
        repos_str = data.get("REPOS", "")
        repos = [r.strip() for r in repos_str.split(",") if r.strip()]
        profiles = {k: v for k, v in data.items() if k.startswith("REPO_PROFILE_")}
        return repos, profiles
    except Exception as exc:
        log.error("Failed to read ConfigMap: %s", exc)
        # Fall back to env vars
        repos_str = os.environ.get("REPOS", "")
        repos = [r.strip() for r in repos_str.split(",") if r.strip()]
        return repos, {}


def _admin_update_repo(repo: str, profile: dict, delete: bool = False):
    """Add, update, or delete a repo in the Kubernetes ConfigMap."""
    try:
        from kubernetes import client
        v1 = client.CoreV1Api()
        namespace = os.environ.get("NAMESPACE", "opinai")
        cm = v1.read_namespaced_config_map("opinai-config", namespace)
        data = cm.data or {}

        repos_str = data.get("REPOS", "")
        repos = [r.strip() for r in repos_str.split(",") if r.strip()]
        repo_key = f"REPO_PROFILE_{repo.replace('/', '_').replace('-', '_').replace('.', '_')}"

        if delete:
            repos = [r for r in repos if r != repo]
            data.pop(repo_key, None)
        else:
            if repo not in repos:
                repos.append(repo)
            data[repo_key] = json.dumps(profile)

        data["REPOS"] = ",".join(repos)
        cm.data = data
        v1.replace_namespaced_config_map("opinai-config", namespace, cm)

        # Update local env so the controller picks it up on next poll
        os.environ["REPOS"] = data["REPOS"]
        if delete:
            os.environ.pop(repo_key, None)
        else:
            os.environ[repo_key] = json.dumps(profile)

        log.info("ConfigMap updated: %s repo %s", "deleted" if delete else "upserted", repo)
    except Exception as exc:
        log.error("Failed to update ConfigMap: %s", exc)
        raise


def _admin_update_settings(settings: dict):
    """Update settings in the Kubernetes ConfigMap."""
    try:
        from kubernetes import client
        v1 = client.CoreV1Api()
        namespace = os.environ.get("NAMESPACE", "opinai")
        cm = v1.read_namespaced_config_map("opinai-config", namespace)
        data = cm.data or {}

        if "poll_interval_minutes" in settings:
            data["POLL_INTERVAL_MINUTES"] = str(settings["poll_interval_minutes"])
            os.environ["POLL_INTERVAL_MINUTES"] = data["POLL_INTERVAL_MINUTES"]
        if "done_label" in settings:
            data["DONE_LABEL"] = settings["done_label"]
            os.environ["DONE_LABEL"] = data["DONE_LABEL"]

        cm.data = data
        v1.replace_namespaced_config_map("opinai-config", namespace, cm)
        log.info("ConfigMap settings updated")
    except Exception as exc:
        log.error("Failed to update settings: %s", exc)
        raise


def _reproduce_issue(repo: str, issue_number: int) -> dict:
    """Create a K8s Job for a specific issue via the controller's callback."""
    if _reproduce_callback is None:
        return {"status": "error", "message": "Controller not ready"}
    try:
        _reproduce_callback(repo, issue_number)
        return {"status": "triggered", "repo": repo, "issue": issue_number}
    except Exception as exc:
        log.error("Reproduce failed for %s#%d: %s", repo, issue_number, exc)
        return {"status": "error", "message": str(exc)}


def start_dashboard():
    """Start the dashboard HTTPS server in a background thread."""
    install_log_handler()
    cert_path, key_path = _generate_certs()
    app = _create_app()

    ssl_ctx = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    ssl_ctx.load_cert_chain(cert_path, key_path)

    def _run():
        from werkzeug.serving import make_server
        server = make_server("0.0.0.0", 8443, app, ssl_context=ssl_ctx)
        log.info("Dashboard running on https://0.0.0.0:8443")
        server.serve_forever()

    thread = threading.Thread(target=_run, daemon=True)
    thread.start()
    return thread
