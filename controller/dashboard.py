"""OpinAI Dashboard — web UI served alongside the controller."""

import json
import logging
import os
import ssl
import subprocess
import threading
import time

from flask import Flask, jsonify, request, send_from_directory

log = logging.getLogger("opinai-dashboard")

CERT_DIR = "/tmp/opinai-certs"
STATIC_DIR = os.path.join(os.path.dirname(__file__), "static")

# Shared state — written by the controller, read by the dashboard
_state = {
    "start_time": time.time(),
    "last_poll": None,
    "poll_count": 0,
    "repos": {},       # repo -> {pending: int, processed: int, last_check: str}
    "runs": [],        # [{repo, issue, title, verdict, ai, duration, timestamp, report}]
    "check_now": False,
}
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


def should_check_now() -> bool:
    with _state_lock:
        if _state["check_now"]:
            _state["check_now"] = False
            return True
        return False


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
        update_state("check_now", True)
        return jsonify({"status": "triggered"})

    @app.route("/api/rerun/<path:repo>/<int:issue>", methods=["POST"])
    def api_rerun(repo, issue):
        result = _rerun_issue(repo, issue)
        return jsonify(result)

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


def start_dashboard():
    """Start the dashboard HTTPS server in a background thread."""
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
