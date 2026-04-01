"""OpinAI Sandbox Manager — creates and manages temporary K8s namespaces for deployment testing."""

import json
import logging
import time

import yaml
from kubernetes import client
from kubernetes.client.rest import ApiException

log = logging.getLogger("opinai-sandbox")

SANDBOX_PREFIX = "opinai-sandbox-"
MANAGED_LABEL_KEY = "opinai.dev/managed"
MAX_AGE_SECONDS = 1800  # 30 minutes

# Allowlisted resource kinds that _apply_manifest can create
_ALLOWED_KINDS = {
    "Deployment", "StatefulSet", "Service", "ConfigMap", "Secret",
    "ServiceAccount", "PersistentVolumeClaim", "Job",
}


def validate_sandbox_name(namespace: str) -> bool:
    """Return True only if namespace starts with the sandbox prefix."""
    return namespace.startswith(SANDBOX_PREFIX)


def create_sandbox(repo: str, issue: int) -> str:
    """Create an isolated sandbox namespace with quotas and network policy."""
    repo_short = repo.split("/")[-1][:20].lower().replace(".", "-")
    ts = str(int(time.time()))[-6:]
    namespace = f"{SANDBOX_PREFIX}{repo_short}-{issue}-{ts}"
    # K8s namespace max 63 chars
    namespace = namespace[:63].rstrip("-")

    core_api = client.CoreV1Api()

    # Create namespace
    ns_body = client.V1Namespace(
        metadata=client.V1ObjectMeta(
            name=namespace,
            labels={
                MANAGED_LABEL_KEY: "true",
                "opinai.dev/repo": repo.replace("/", "-"),
                "opinai.dev/issue": str(issue),
            },
            annotations={
                "opinai.dev/created-at": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
                "opinai.dev/repo-full": repo,
            },
        )
    )
    core_api.create_namespace(body=ns_body)
    log.info("Created sandbox namespace: %s", namespace)

    # ResourceQuota
    quota = client.V1ResourceQuota(
        metadata=client.V1ObjectMeta(
            name="opinai-quota",
            namespace=namespace,
            labels={MANAGED_LABEL_KEY: "true"},
        ),
        spec=client.V1ResourceQuotaSpec(
            hard={
                "requests.cpu": "1",
                "requests.memory": "1Gi",
                "limits.cpu": "2",
                "limits.memory": "2Gi",
                "pods": "10",
                "services": "5",
                "persistentvolumeclaims": "3",
            }
        ),
    )
    core_api.create_namespaced_resource_quota(namespace=namespace, body=quota)

    # NetworkPolicy — isolate sandbox, allow traffic from opinai namespace + DNS
    net_api = client.NetworkingV1Api()
    netpol = {
        "apiVersion": "networking.k8s.io/v1",
        "kind": "NetworkPolicy",
        "metadata": {
            "name": "opinai-sandbox-policy",
            "namespace": namespace,
            "labels": {MANAGED_LABEL_KEY: "true"},
        },
        "spec": {
            "podSelector": {},
            "policyTypes": ["Ingress", "Egress"],
            "ingress": [
                {
                    "from": [
                        {"namespaceSelector": {"matchLabels": {"kubernetes.io/metadata.name": "opinai"}}},
                        {"podSelector": {}},
                    ]
                }
            ],
            "egress": [
                {"to": [{"podSelector": {}}]},
                {"ports": [{"port": 53, "protocol": "UDP"}, {"port": 53, "protocol": "TCP"}]},
            ],
        },
    }
    net_api.create_namespaced_network_policy(
        namespace=namespace,
        body=netpol,
    )

    log.info("Sandbox %s ready (quota + network policy applied)", namespace)
    return namespace


def deploy_in_sandbox(namespace: str, steps: list[dict]) -> dict:
    """Execute deployment steps in a sandbox namespace.

    Each step: {"type": "manifest"|"shell"|"wait", "content": "...", "required": bool, "description": "..."}
    Returns: {"success": bool, "steps_completed": int, "steps_total": int, "errors": [str], "endpoints": {}}
    """
    if not validate_sandbox_name(namespace):
        raise ValueError(f"Invalid sandbox namespace: {namespace}")

    # Verify managed label
    core_api = client.CoreV1Api()
    ns = core_api.read_namespace(name=namespace)
    labels = ns.metadata.labels or {}
    if labels.get(MANAGED_LABEL_KEY) != "true":
        raise ValueError(f"Namespace {namespace} is not managed by OpinAI")

    result = {"success": True, "steps_completed": 0, "steps_total": len(steps), "errors": [], "endpoints": {}}

    for i, step in enumerate(steps):
        step_type = step.get("type", "")
        content = step.get("content", "")
        required = step.get("required", True)
        desc = step.get("description", f"Step {i + 1}")

        try:
            if step_type == "manifest":
                _apply_manifest(namespace, content)
                log.info("Step %d/%d: Applied manifest — %s", i + 1, len(steps), desc)

            elif step_type == "wait":
                timeout = step.get("timeout_seconds", 120)
                if not _wait_for_ready(namespace, content, timeout):
                    raise RuntimeError(f"Timeout waiting for {content}")
                log.info("Step %d/%d: Ready — %s", i + 1, len(steps), desc)

            elif step_type == "shell":
                log.info("Step %d/%d: Shell — %s (skipped in v1)", i + 1, len(steps), desc)

            result["steps_completed"] = i + 1

        except Exception as exc:
            error_msg = f"Step {i + 1} ({desc}): {exc}"
            log.error("Deployment step failed: %s", error_msg)
            result["errors"].append(error_msg)
            if required:
                result["success"] = False
                break

    # Collect endpoints
    result["endpoints"] = get_sandbox_endpoints(namespace)
    return result


def get_sandbox_endpoints(namespace: str) -> dict[str, str]:
    """List services in the sandbox and return name→FQDN map."""
    if not validate_sandbox_name(namespace):
        return {}
    try:
        core_api = client.CoreV1Api()
        services = core_api.list_namespaced_service(namespace=namespace)
        return {
            svc.metadata.name: f"{svc.metadata.name}.{namespace}.svc.cluster.local"
            for svc in services.items
        }
    except Exception:
        return {}


def teardown_sandbox(namespace: str) -> bool:
    """Delete a sandbox namespace. Only works if prefix and managed label match."""
    if not validate_sandbox_name(namespace):
        log.warning("Refusing to teardown: invalid prefix %s", namespace)
        return False

    core_api = client.CoreV1Api()
    try:
        ns = core_api.read_namespace(name=namespace)
    except ApiException as exc:
        if exc.status == 404:
            return True  # already gone
        raise

    labels = ns.metadata.labels or {}
    if labels.get(MANAGED_LABEL_KEY) != "true":
        log.warning("Refusing to teardown: missing managed label on %s", namespace)
        return False

    core_api.delete_namespace(name=namespace)
    log.info("Torn down sandbox: %s", namespace)
    return True


def list_sandboxes() -> list[dict]:
    """List active sandbox namespaces."""
    try:
        core_api = client.CoreV1Api()
        namespaces = core_api.list_namespace(
            label_selector=f"{MANAGED_LABEL_KEY}=true"
        )
        result = []
        for ns in namespaces.items:
            name = ns.metadata.name
            if not validate_sandbox_name(name):
                continue
            annotations = ns.metadata.annotations or {}
            labels = ns.metadata.labels or {}
            created_str = annotations.get("opinai.dev/created-at", "")
            age = 0
            if created_str:
                try:
                    created_ts = time.mktime(time.strptime(created_str, "%Y-%m-%dT%H:%M:%SZ"))
                    age = int(time.time() - created_ts)
                except ValueError:
                    pass
            result.append({
                "namespace": name,
                "repo": annotations.get("opinai.dev/repo-full", labels.get("opinai.dev/repo", "")),
                "issue": labels.get("opinai.dev/issue", ""),
                "created_at": created_str,
                "age_seconds": age,
            })
        return result
    except Exception as exc:
        log.error("Failed to list sandboxes: %s", exc)
        return []


def auto_cleanup() -> int:
    """Delete sandboxes older than MAX_AGE_SECONDS. Returns count deleted."""
    sandboxes = list_sandboxes()
    count = 0
    for sb in sandboxes:
        if sb["age_seconds"] > MAX_AGE_SECONDS:
            if teardown_sandbox(sb["namespace"]):
                count += 1
    if count:
        log.info("Auto-cleaned %d sandbox(es)", count)
    return count


def _apply_manifest(namespace: str, manifest_yaml: str):
    """Parse and apply a single K8s manifest YAML to a namespace."""
    doc = yaml.safe_load(manifest_yaml)
    if not doc or not isinstance(doc, dict):
        raise ValueError("Invalid manifest YAML")

    kind = doc.get("kind", "")
    if kind not in _ALLOWED_KINDS:
        raise ValueError(f"Resource kind '{kind}' is not allowed in sandbox")

    # Force namespace and managed label
    meta = doc.setdefault("metadata", {})
    meta["namespace"] = namespace
    labels = meta.setdefault("labels", {})
    labels[MANAGED_LABEL_KEY] = "true"

    if kind == "Deployment":
        api = client.AppsV1Api()
        api.create_namespaced_deployment(namespace=namespace, body=doc)
    elif kind == "StatefulSet":
        api = client.AppsV1Api()
        api.create_namespaced_stateful_set(namespace=namespace, body=doc)
    elif kind == "Service":
        core = client.CoreV1Api()
        core.create_namespaced_service(namespace=namespace, body=doc)
    elif kind == "ConfigMap":
        core = client.CoreV1Api()
        core.create_namespaced_config_map(namespace=namespace, body=doc)
    elif kind == "Secret":
        core = client.CoreV1Api()
        core.create_namespaced_secret(namespace=namespace, body=doc)
    elif kind == "ServiceAccount":
        core = client.CoreV1Api()
        core.create_namespaced_service_account(namespace=namespace, body=doc)
    elif kind == "PersistentVolumeClaim":
        core = client.CoreV1Api()
        core.create_namespaced_persistent_volume_claim(namespace=namespace, body=doc)
    elif kind == "Job":
        batch = client.BatchV1Api()
        batch.create_namespaced_job(namespace=namespace, body=doc)


def _wait_for_ready(namespace: str, resource_ref: str, timeout: int = 120) -> bool:
    """Wait for a resource to become ready. resource_ref format: 'deployment/name' or 'pod/name'."""
    parts = resource_ref.strip().split("/", 1)
    if len(parts) != 2:
        log.warning("Invalid resource ref: %s", resource_ref)
        return False

    kind, name = parts[0].lower(), parts[1]
    deadline = time.time() + timeout

    while time.time() < deadline:
        try:
            if kind == "deployment":
                api = client.AppsV1Api()
                dep = api.read_namespaced_deployment(name=name, namespace=namespace)
                desired = dep.spec.replicas or 1
                ready = dep.status.ready_replicas or 0
                if ready >= desired:
                    return True
            elif kind == "pod":
                core = client.CoreV1Api()
                pod = core.read_namespaced_pod(name=name, namespace=namespace)
                if pod.status.phase == "Running":
                    return True
        except ApiException:
            pass
        time.sleep(5)

    return False
