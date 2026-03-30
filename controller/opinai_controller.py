"""OpinAI Controller — watches GitHub repos for new issues and creates reproduction Jobs."""

import logging
import os
import signal
import sys
import time

import requests
from kubernetes import client, config
from kubernetes.client.rest import ApiException

from dashboard import (
    append_run,
    set_check_result,
    set_reproduce_callback,
    should_check_now,
    start_dashboard,
    update_repo_stats,
    update_state,
)

logging.basicConfig(
    level=logging.INFO,
    format="%(asctime)s [%(levelname)s] %(message)s",
    datefmt="%Y-%m-%dT%H:%M:%S",
)
log = logging.getLogger("opinai-controller")

# ---------------------------------------------------------------------------
# Config from env
# ---------------------------------------------------------------------------

REPOS = [r.strip() for r in os.environ.get("REPOS", "").split(",") if r.strip()]
POLL_INTERVAL = int(os.environ.get("POLL_INTERVAL_MINUTES", "60")) * 60
DONE_LABEL = os.environ.get("DONE_LABEL", "opinai-done")
NAMESPACE = os.environ.get("NAMESPACE", "opinai")
GITHUB_TOKEN = os.environ.get("GITHUB_TOKEN", "")

_shutdown = False


def _handle_signal(signum, _frame):
    global _shutdown
    log.info("Received signal %s — shutting down", signum)
    _shutdown = True


signal.signal(signal.SIGTERM, _handle_signal)
signal.signal(signal.SIGINT, _handle_signal)


# ---------------------------------------------------------------------------
# GitHub helpers
# ---------------------------------------------------------------------------

GH_API = "https://api.github.com"


def gh_headers():
    return {
        "Accept": "application/vnd.github+json",
        "Authorization": f"Bearer {GITHUB_TOKEN}",
        "X-GitHub-Api-Version": "2022-11-28",
    }


def _count_processed(repo: str) -> int:
    """Count issues with the DONE_LABEL (already processed)."""
    url = f"{GH_API}/repos/{repo}/issues"
    params = {"state": "all", "labels": DONE_LABEL, "per_page": 1}
    try:
        resp = requests.get(url, headers=gh_headers(), params=params, timeout=15)
        resp.raise_for_status()
        # GitHub returns total_count in link header; use len as approximation
        return len(resp.json())
    except requests.RequestException:
        return 0


def fetch_open_issues(repo: str) -> list[dict]:
    """Return open issues (not PRs) that don't have the DONE_LABEL."""
    url = f"{GH_API}/repos/{repo}/issues"
    params = {"state": "open", "per_page": 100}
    try:
        resp = requests.get(url, headers=gh_headers(), params=params, timeout=30)
        resp.raise_for_status()
    except requests.RequestException as exc:
        log.error("Failed to fetch issues for %s: %s", repo, exc)
        return []

    issues = []
    for issue in resp.json():
        # Skip pull requests (they have a pull_request key)
        if "pull_request" in issue:
            continue
        labels = {lbl["name"] for lbl in issue.get("labels", [])}
        if DONE_LABEL in labels:
            continue
        issues.append(issue)
    return issues


# ---------------------------------------------------------------------------
# Kubernetes helpers
# ---------------------------------------------------------------------------


def load_k8s():
    """Load in-cluster config, fall back to kubeconfig for local dev."""
    try:
        config.load_incluster_config()
        log.info("Loaded in-cluster Kubernetes config")
    except config.ConfigException:
        config.load_kube_config()
        log.info("Loaded kubeconfig (local dev mode)")


def job_name(repo: str, issue_number: int) -> str:
    repo_safe = repo.replace("/", "-").lower()
    return f"opinai-{repo_safe}-{issue_number}"


def job_exists(batch_api: client.BatchV1Api, name: str) -> bool:
    try:
        batch_api.read_namespaced_job(name=name, namespace=NAMESPACE)
        return True
    except ApiException as exc:
        if exc.status == 404:
            return False
        raise


def _all_repo_profile_env_vars() -> list:
    """Read all REPO_PROFILE_* entries from ConfigMap and return as env vars."""
    try:
        v1 = client.CoreV1Api()
        cm = v1.read_namespaced_config_map("opinai-config", NAMESPACE)
        data = cm.data or {}
        return [
            client.V1EnvVar(name=k, value=v)
            for k, v in data.items()
            if k.startswith("REPO_PROFILE_")
        ]
    except ApiException:
        # Fallback to env vars
        return [
            client.V1EnvVar(name=k, value=v)
            for k, v in os.environ.items()
            if k.startswith("REPO_PROFILE_")
        ]


# Track Jobs we've already recorded as runs so we don't duplicate
_recorded_jobs: set[str] = set()


def create_job(batch_api: client.BatchV1Api, repo: str, issue: dict):
    """Create a Kubernetes Job to reproduce the issue."""
    number = issue["number"]
    name = job_name(repo, number)

    if job_exists(batch_api, name):
        log.info("Job %s already exists — skipping", name)
        return

    repo_safe = repo.replace("/", "-").lower()

    # Volume + mount for GCP credentials (used when AI_PROVIDER=vertex)
    gcp_volume = client.V1Volume(
        name="gcp-credentials",
        secret=client.V1SecretVolumeSource(
            secret_name="opinai-gcp-credentials",
            optional=True,
        ),
    )
    gcp_mount = client.V1VolumeMount(
        name="gcp-credentials",
        mount_path="/var/run/secrets/gcp",
        read_only=True,
    )

    job_manifest = client.V1Job(
        api_version="batch/v1",
        kind="Job",
        metadata=client.V1ObjectMeta(
            name=name,
            namespace=NAMESPACE,
            labels={
                "app": "opinai-runner",
                "opinai/repo": repo_safe,
                "opinai/issue": str(number),
            },
            annotations={
                "opinai/title": issue.get("title", "")[:253],
                "opinai/repo-full": repo,
            },
        ),
        spec=client.V1JobSpec(
            backoff_limit=0,
            ttl_seconds_after_finished=3600,
            template=client.V1PodTemplateSpec(
                spec=client.V1PodSpec(
                    service_account_name="opinai-controller",
                    restart_policy="Never",
                    volumes=[gcp_volume],
                    containers=[
                        client.V1Container(
                            name="runner",
                            image="image-registry.openshift-image-registry.svc:5000/opinai/opinai-controller:latest",
                            image_pull_policy="Always",
                            command=["python", "opinai_runner.py"],
                            env=[
                                client.V1EnvVar(name="REPO", value=repo),
                                client.V1EnvVar(
                                    name="ISSUE_NUMBER", value=str(number)
                                ),
                                client.V1EnvVar(
                                    name="OPINAI_AUTO_POST",
                                    value="false",
                                ),
                                client.V1EnvVar(
                                    name="GOOGLE_APPLICATION_CREDENTIALS",
                                    value="/var/run/secrets/gcp/credentials.json",
                                ),
                                client.V1EnvVar(
                                    name="AI_PROVIDER",
                                    value_from=client.V1EnvVarSource(
                                        secret_key_ref=client.V1SecretKeySelector(
                                            name="opinai-credentials",
                                            key="AI_PROVIDER",
                                            optional=True,
                                        )
                                    ),
                                ),
                                client.V1EnvVar(
                                    name="AI_PROJECT",
                                    value_from=client.V1EnvVarSource(
                                        secret_key_ref=client.V1SecretKeySelector(
                                            name="opinai-credentials",
                                            key="AI_PROJECT",
                                            optional=True,
                                        )
                                    ),
                                ),
                                client.V1EnvVar(
                                    name="AI_REGION",
                                    value_from=client.V1EnvVarSource(
                                        secret_key_ref=client.V1SecretKeySelector(
                                            name="opinai-credentials",
                                            key="AI_REGION",
                                            optional=True,
                                        )
                                    ),
                                ),
                                client.V1EnvVar(
                                    name="AI_MODEL",
                                    value_from=client.V1EnvVarSource(
                                        secret_key_ref=client.V1SecretKeySelector(
                                            name="opinai-credentials",
                                            key="AI_MODEL",
                                            optional=True,
                                        )
                                    ),
                                ),
                            ] + _all_repo_profile_env_vars(),
                            env_from=[
                                client.V1EnvFromSource(
                                    secret_ref=client.V1SecretEnvSource(
                                        name="opinai-credentials"
                                    )
                                ),
                                client.V1EnvFromSource(
                                    config_map_ref=client.V1ConfigMapEnvSource(
                                        name="opinai-config",
                                        optional=True,
                                    )
                                ),
                            ],
                            volume_mounts=[gcp_mount],
                            resources=client.V1ResourceRequirements(
                                requests={"cpu": "100m", "memory": "256Mi"},
                                limits={"cpu": "500m", "memory": "512Mi"},
                            ),
                        )
                    ],
                )
            ),
        ),
    )

    try:
        batch_api.create_namespaced_job(namespace=NAMESPACE, body=job_manifest)
        log.info("Created Job %s for %s#%d: %s", name, repo, number, issue["title"])
    except ApiException as exc:
        log.error("Failed to create Job %s: %s", name, exc.reason)


def check_completed_jobs(batch_api: client.BatchV1Api):
    """Scan completed Jobs, extract results, and add to dashboard runs."""
    try:
        jobs = batch_api.list_namespaced_job(
            namespace=NAMESPACE, label_selector="app=opinai-runner"
        )
    except ApiException as exc:
        log.error("Failed to list jobs: %s", exc.reason)
        return

    core_api = client.CoreV1Api()

    for job in jobs.items:
        name = job.metadata.name
        finished = bool(job.status.succeeded or job.status.failed)
        if not finished:
            continue
        if name in _recorded_jobs:
            continue

        _recorded_jobs.add(name)

        labels = job.metadata.labels or {}
        annotations = job.metadata.annotations or {}
        repo = annotations.get("opinai/repo-full", labels.get("opinai/repo", ""))
        issue = labels.get("opinai/issue", "")
        title = annotations.get("opinai/title", "")

        if job.status.succeeded:
            log.info("Job %s succeeded", name)
        else:
            log.warning("Job %s failed", name)

        # Compute duration
        duration = ""
        if job.status.start_time and job.status.completion_time:
            delta = (job.status.completion_time - job.status.start_time).total_seconds()
            mins = int(delta) // 60
            secs = int(delta) % 60
            duration = f"{mins}m {secs}s" if mins else f"{secs}s"

        # Read pod logs to extract verdict
        pod_logs = ""
        try:
            pods = core_api.list_namespaced_pod(
                namespace=NAMESPACE,
                label_selector=f"job-name={name}",
            )
            if pods.items:
                pod_logs = core_api.read_namespaced_pod_log(
                    name=pods.items[0].metadata.name,
                    namespace=NAMESPACE,
                    tail_lines=100,
                )
        except Exception:
            pass

        # Extract suggested comment from delimiters
        suggested_comment = ""
        start_marker = "--- OPINAI SUGGESTED COMMENT ---"
        end_marker = "--- END SUGGESTED COMMENT ---"
        if start_marker in pod_logs and end_marker in pod_logs:
            start = pod_logs.index(start_marker) + len(start_marker)
            end = pod_logs.index(end_marker)
            suggested_comment = pod_logs[start:end].strip()

        # Parse verdict from logs
        verdict = "inconclusive"
        check_text = (suggested_comment or pod_logs).lower()
        if "bug confirmed" in check_text or "tests failed" in check_text:
            verdict = "bug confirmed"
        elif "all tests passed" in check_text or "not a bug" in check_text:
            verdict = "not a bug"

        append_run({
            "repo": repo,
            "issue": issue,
            "title": title,
            "verdict": verdict,
            "ai": True,
            "duration": duration,
            "posted": False,
            "timestamp": (
                job.status.completion_time.isoformat()
                if job.status.completion_time
                else time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime())
            ),
            "report": suggested_comment or pod_logs[-3000:] or "(no logs)",
        })


# ---------------------------------------------------------------------------
# Main loop
# ---------------------------------------------------------------------------


def main():
    if not REPOS:
        log.error("REPOS env var is empty — nothing to watch")
        sys.exit(1)
    if not GITHUB_TOKEN:
        log.error("GITHUB_TOKEN env var is required")
        sys.exit(1)

    load_k8s()
    batch_api = client.BatchV1Api()

    # Start the web dashboard
    start_dashboard()

    # Register reproduce callback so the dashboard can create jobs
    def _reproduce_from_dashboard(repo: str, issue_number: int):
        """Fetch issue from GitHub and create a Job."""
        import requests as req
        url = f"{GH_API}/repos/{repo}/issues/{issue_number}"
        resp = req.get(url, headers=gh_headers(), timeout=30)
        resp.raise_for_status()
        issue = resp.json()
        create_job(batch_api, repo, issue)

    set_reproduce_callback(_reproduce_from_dashboard)

    log.info(
        "OpinAI Controller started — watching %s, polling every %ds",
        ", ".join(REPOS),
        POLL_INTERVAL,
    )

    poll_count = 0

    while not _shutdown:
        poll_count += 1
        update_state("poll_count", poll_count)
        update_state("last_poll", time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()))

        total_new = 0
        for repo in REPOS:
            log.info("Checking %s for new issues...", repo)
            issues = fetch_open_issues(repo)
            log.info("Found %d unprocessed issues in %s", len(issues), repo)
            total_new += len(issues)

            # Count processed issues (those with done label) for stats
            processed = _count_processed(repo)
            update_repo_stats(repo, pending=len(issues), processed=processed)

            for issue in issues:
                create_job(batch_api, repo, issue)

        check_completed_jobs(batch_api)
        set_check_result(total_new)

        # Sleep in small increments, but also check for "check now" requests
        elapsed = 0
        while elapsed < POLL_INTERVAL and not _shutdown:
            if should_check_now():
                log.info("Manual check triggered from dashboard")
                break
            time.sleep(min(5, POLL_INTERVAL - elapsed))
            elapsed += 5

    log.info("Controller shut down cleanly")


if __name__ == "__main__":
    main()
