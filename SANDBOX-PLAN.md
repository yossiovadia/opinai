# Sandbox Deployment for Complex K8s Projects — Implementation Plan

## Target Projects
- **BBR (Blazing-BBR)**: Envoy ext_proc filter. Components: Envoy proxy + BBR ext_proc gRPC service. Tests require sending HTTP through Envoy and observing ext_proc behavior.
- **MaaS (Multicloud API as a Service)**: Controller + REST API + PostgreSQL + potential operator dependencies. Tests require API availability with database.

---

## 1. What Infrastructure Already Exists

### Sandbox Manager (`internal/sandbox/manager.go`)
- **CreateSandbox(repo, issue)**: Creates isolated namespace with ResourceQuota (1 CPU/1Gi req, 2 CPU/2Gi limit, 10 pods, 5 services, 3 PVCs) and NetworkPolicy (ingress from controller namespace + intra-namespace, egress DNS only + intra-namespace).
- **DeployInSandbox(ns, steps)**: Executes deployment steps sequentially. Supports step types: `manifest` (K8s YAML/JSON), `wait` (poll deployment/pod readiness), `shell`/`command` (exec in cloned repo). Clones the repo once for command steps. Auto-injects namespace into oc/kubectl commands. Auto-runs `helm dependency build` before helm install/upgrade.
- **AllowedKinds**: Deployment, StatefulSet, Service, ConfigMap, Secret, ServiceAccount, PVC, Job, CronJob, Role, RoleBinding, NetworkPolicy, Ingress, HPA, Route. Does NOT include CRDs or CustomResources.
- **SkippedKinds**: Namespace, ClusterRole, ClusterRoleBinding (cluster-scoped — correctly skipped).
- **GetEndpoints(ns)**: Returns service name -> FQDN map (`svc.ns.svc.cluster.local`).
- **RBAC pre-check**: SelfSubjectAccessReview before creating sensitive resources.
- **waitForReady**: Polls Deployment readyReplicas or Pod phase. Only supports `deployment/name` and `pod/name`.
- **TeardownSandbox**: Deletes namespace (checks prefix + managed label).
- **AutoCleanup**: Deletes sandboxes older than 30 min.

### Job Manager (`internal/controller/jobs.go`)
- **trySandboxDeploy**: Called during job creation. Checks if repo profile has `k8s: true` AND a deployment plan exists. Orders options (previously-working first, then recommended, then rest). Tries each option: CreateSandbox -> DeployInSandbox -> on success returns (sandboxNS, endpointsJSON, planJSON). On failure, tears down and tries next option. Saves `working_deploy_option` and per-option errors to repo_memory.
- **Environment passing**: Sandbox NS, endpoints JSON, and deployment plan JSON are passed to the runner pod as env vars (`OPINAI_SANDBOX_NAMESPACE`, `OPINAI_SANDBOX_ENDPOINTS`, `OPINAI_DEPLOYMENT_PLAN`).
- **Post-job cleanup**: After harvesting, tears down sandbox namespace via annotation `opinai/sandbox-namespace`.

### Runner (`internal/runner/runner.go`)
- **Sandbox path** (line 70-80): If `OPINAI_SANDBOX_NAMESPACE` is set, reads `OPINAI_SANDBOX_ENDPOINTS`, picks the first endpoint as `serverURL`. This is the critical gap — it picks ONE endpoint blindly.
- **Deploy-from-plan path** (line 82-88): If `OPINAI_DEPLOYMENT_PLAN` is set but no sandbox, calls `deployFromPlan` which asks AI to select an option, then falls back to `startServer()` (local pip install). Cannot actually deploy K8s resources because the runner pod lacks RBAC.
- **Agent investigation** (line 194): Passes `serverURL` to the agent. The agent uses `server_request` tool to hit endpoints and `run_test` to run Python scripts.

### Deployment Plan Analysis (`internal/ai/analyze.go` + `analyze_deployment.txt`)
- AI generates 3 deployment options, each with steps (manifest/wait/shell). Also generates project metadata: project_type, dependencies, resource_requirements, install_command, run_command, health_endpoint.
- Stored in `deployment_plans` table as JSON.

### Agent Analysis (`internal/agent/analyze.go`)
- Structured output: RepoAnalysis with APISurface (endpoints list), Deployment info, BugHints (test_strategy).
- Stored as `rich_analysis` in repo_memory.

---

## 2. What Is Missing to Wire It End-to-End

### Gap A: Runner doesn't know which endpoint to test
**Current**: Picks first endpoint from the map. For BBR this is wrong — Envoy listens on one port, ext_proc on another, and tests need to hit Envoy's HTTP port specifically.
**Fix needed**: The deployment plan options should specify which service is the "test target" (the entry point for test traffic). The runner should use that, not the first random service.

### Gap B: No CRD/CustomResource support in sandbox
**Current**: `AllowedKinds` only includes built-in K8s kinds. CRDs like `EnvoyFilter`, `Gateway`, `AuthPolicy` etc. are silently skipped.
**Fix needed**: Use dynamic client (`k8s.io/client-go/dynamic`) to apply arbitrary resource kinds. This is essential for operator-managed projects.

### Gap C: No dependency ordering / readiness orchestration
**Current**: Steps execute sequentially, which is correct. But `waitForReady` only supports Deployment and Pod. For complex stacks, you need to wait for StatefulSet, or for a CRD to reach a `.status.conditions` ready state, or for a database to accept connections.
**Fix needed**: Extend `waitForReady` to support StatefulSet, Service (has endpoints), and arbitrary `kind/name` with status condition checking.

### Gap D: Network policy blocks external egress
**Current**: Egress only allows intra-namespace + DNS. If a component needs to pull images from external registries, or if init containers need to reach the internet (e.g., database migrations downloading schemas), they will fail.
**Fix needed**: Optionally allow broader egress (e.g., to internal registry, or to specific CIDRs). The deployment plan should flag if external egress is needed.

### Gap E: Multi-endpoint awareness in agent investigation
**Current**: Agent's `server_request` tool takes a path and prepends `ServerURL` (one URL). For multi-component projects, the agent needs to know about ALL endpoints and their purposes.
**Fix needed**: Pass the full endpoints map + service descriptions to the agent context. The agent should be able to target different services.

### Gap F: Resource quotas too tight for complex stacks
**Current**: 1 CPU / 1Gi request, 10 pods max. A stack with Envoy + ext_proc + DB = 3 pods minimum, but with sidecars or init containers could exceed limits.
**Fix needed**: Make quotas configurable from the deployment plan's `resource_requirements`.

### Gap G: No database bootstrapping
**Current**: Can deploy a PostgreSQL StatefulSet, but no mechanism to run schema migrations or seed data.
**Fix needed**: Support `init` step type that runs a K8s Job in the sandbox (e.g., schema migration) and waits for completion.

### Gap H: Health detection only checks one endpoint
**Current**: Runner checks one health URL. Complex stacks need all components healthy before testing.
**Fix needed**: Aggregate health check — wait for all services in the sandbox to have ready endpoints.

---

## 3. CRD/Operator Dependencies

### Strategy: Pre-installed operators (recommended for v1)
Operators should be pre-installed in the cluster by the cluster admin. OpinAI's sandbox just creates instances of CRDs in the sandbox namespace. This is the right approach because:
- Installing operators requires cluster-admin RBAC (OpinAI shouldn't have this)
- Operators are shared across namespaces
- The AI analysis already detects required operators via `clusterState["operators"]`

### What's needed:
1. **Dynamic client in sandbox manager**: Use `k8s.io/client-go/dynamic` to apply any resource kind, not just the hardcoded AllowedKinds switch. This handles CRDs naturally — if the CRD exists in the cluster, the dynamic client can create instances.
2. **Operator availability check**: Before deploying, verify that required CRDs exist (`apiextensions.k8s.io/v1` CRD listing). If a required CRD is missing, fail fast with a clear message.
3. **`readClusterState` in admin.go**: Currently returns empty. Should actually query the cluster for installed CRDs, operators (OLM subscriptions), and namespaces.

### Files to change:
- `internal/sandbox/manager.go`: Add dynamic client, CRD existence check
- `internal/dashboard/admin.go`: Implement `readClusterState()` 

**Complexity: Medium** (dynamic client is straightforward, CRD check is a single API call)

---

## 4. Multi-Container Deployment Orchestration

### Current capability
The step-based system already handles this: a deployment plan can have multiple `manifest` steps followed by `wait` steps. The issue is that `waitForReady` is too limited.

### What's needed:
1. **Extend `waitForReady`** to support:
   - `statefulset/name` — check readyReplicas
   - `service/name` — check Endpoints object has addresses  
   - `job/name` — check succeeded count
   - Generic timeout with exponential backoff instead of fixed 5s sleep

2. **Add `waitForAllReady(ns, timeout)` function** that waits for ALL deployments+statefulsets in the namespace to be ready. Called automatically after all manifest steps complete.

3. **Deployment plan format enhancement** — add `depends_on` to steps so the AI can express: "deploy the database first, wait for it, then deploy the API server."

### Files to change:
- `internal/sandbox/manager.go`: Extend `waitForReady`, add `waitForAllReady`
- `internal/prompts/analyze_deployment.txt`: Update prompt to request depends_on ordering

**Complexity: Easy-Medium** (waitForReady extension is straightforward)

---

## 5. Test Traffic Routing

### Problem
For BBR: test traffic must go through Envoy (port 8080) which forwards to ext_proc (gRPC port 50051). The agent needs to know:
- Send HTTP to `envoy.sandbox-ns.svc.cluster.local:8080`
- Not directly to `bbr-ext-proc.sandbox-ns.svc.cluster.local:50051`

For MaaS: test traffic goes to the API server, which talks to Postgres internally. The agent needs:
- Send HTTP to `maas-api.sandbox-ns.svc.cluster.local:8080`
- Postgres is internal — agent doesn't hit it directly

### Solution: `test_endpoint` in deployment plan
Add a `test_endpoint` field to each deployment option:
```json
{
  "id": "envoy-setup",
  "test_endpoint": {
    "service": "envoy",
    "port": 8080,
    "protocol": "http",
    "health_path": "/health",
    "purpose": "Envoy proxy — all test requests go through here"
  },
  "all_endpoints": [
    {"service": "envoy", "port": 8080, "purpose": "HTTP proxy entry point"},
    {"service": "bbr-ext-proc", "port": 50051, "purpose": "gRPC ext_proc filter (internal)"}
  ]
}
```

### Runner changes:
- Instead of picking the first endpoint, read `test_endpoint` from the deployment plan option and construct the serverURL from it.
- Pass `all_endpoints` as context to the agent so it can reason about the architecture.

### Files to change:
- `internal/runner/runner.go`: Read `test_endpoint` from plan, construct serverURL properly
- `internal/prompts/analyze_deployment.txt`: Add `test_endpoint` and `all_endpoints` to option schema
- `internal/agent/agent.go`: Include endpoint map in agent context

**Complexity: Medium** (prompt changes + runner plumbing)

---

## 6. Health Detection for Complex Stacks

### Current
Runner checks one health URL (from profile or hardcoded `localhost:8000/health`). For sandboxed deployments, there's no health checking at all — the runner just takes the first endpoint and starts testing.

### Solution: Multi-service health aggregation
1. **In `DeployInSandbox`**: After all steps complete, call a new `WaitForAllHealthy(ns, timeout)` that:
   - Lists all services in the namespace
   - For each service, checks if it has ready endpoints (K8s Endpoints resource has addresses)
   - Optionally, if the deployment plan specifies health paths per service, HTTP-checks them
   - Returns only when all services are responding or timeout

2. **In runner (sandbox path)**: After setting `serverURL`, do a health check loop against the test_endpoint's health_path (if specified) before starting investigation.

### Files to change:
- `internal/sandbox/manager.go`: Add `WaitForAllHealthy`
- `internal/runner/runner.go`: Add health check loop for sandbox deployments

**Complexity: Easy-Medium**

---

## 7. Cleanup and Resource Management

### Current (already solid)
- Sandbox namespaces are torn down after job completion (in `harvestSingleJob`)
- AutoCleanup runs on admin endpoint, deletes sandboxes older than 30 min
- ResourceQuota limits per namespace prevent runaway resource usage

### Enhancements needed:
1. **Configurable quotas from deployment plan**: The AI's `resource_requirements` should feed into the ResourceQuota, not use fixed values. Some projects need more memory (e.g., Java apps need 1-2Gi).

2. **Auto-cleanup on poller cycle**: Currently AutoCleanup is only triggered via admin API. Should also run during each poll cycle to catch orphaned sandboxes (e.g., if the controller restarted mid-job).

3. **Sandbox timeout from deployment plan**: The 30-min max age is hardcoded. Complex deployments might need longer. Use `job_timeout_minutes` from the plan to set the sandbox TTL.

### Files to change:
- `internal/sandbox/manager.go`: Accept quota overrides in `CreateSandbox`
- `internal/controller/jobs.go`: Pass resource requirements to `CreateSandbox`
- `internal/controller/poller.go`: Call AutoCleanup each cycle

**Complexity: Easy**

---

## 8. Changes Needed Per File

| File | Changes | Complexity |
|------|---------|-----------|
| `internal/sandbox/manager.go` | Dynamic client for CRDs, extend `waitForReady` (StatefulSet/Service/Job), add `WaitForAllHealthy`, configurable quotas, CRD existence check | **Hard** |
| `internal/runner/runner.go` | Read `test_endpoint` from plan instead of first endpoint, add sandbox health check loop, pass all_endpoints to agent context | **Medium** |
| `internal/controller/jobs.go` | Pass resource requirements + timeout to `CreateSandbox`, pass `test_endpoint` info to runner env | **Easy** |
| `internal/prompts/analyze_deployment.txt` | Add `test_endpoint`, `all_endpoints`, `depends_on` to option schema | **Easy** |
| `internal/agent/agent.go` | Include endpoint map in agent system prompt, make `server_request` support targeting different services | **Medium** |
| `internal/agent/tools.go` | Optional: add `service_request` tool that targets a specific service FQDN | **Easy** |
| `internal/dashboard/admin.go` | Implement `readClusterState()` to query CRDs/operators/namespaces | **Medium** |
| `internal/controller/poller.go` | Add AutoCleanup call in poll loop | **Easy** |
| `internal/ai/analyze.go` | No changes needed | - |

---

## 9. Estimated Complexity Per Component

| Component | Complexity | Effort | Risk |
|-----------|-----------|--------|------|
| Dynamic client for CRDs | Medium | 2-3 hours | Low — well-documented K8s API |
| `waitForReady` extensions | Easy | 1 hour | Low |
| `WaitForAllHealthy` | Easy-Medium | 1-2 hours | Medium — timing sensitivity |
| `test_endpoint` plumbing | Medium | 2-3 hours | Low — straightforward data flow |
| Prompt updates | Easy | 30 min | Medium — AI output quality |
| Multi-endpoint agent context | Medium | 1-2 hours | Low |
| Configurable quotas | Easy | 30 min | Low |
| `readClusterState` real impl | Medium | 1-2 hours | Low |
| Auto-cleanup in poller | Easy | 15 min | Low |

---

## 10. Suggested Implementation Order

### Phase 1: Make what exists actually work (highest value, lowest risk)
1. **`test_endpoint` in deployment plan + runner** — Without this, sandbox deployments pick the wrong endpoint. This single change makes the existing sandbox flow useful.
2. **Extend `waitForReady` for StatefulSet/Service** — Enables database deployments to complete before the API server starts.
3. **Auto-cleanup in poller** — Prevents orphaned sandboxes from leaking resources.

### Phase 2: Enable complex projects (BBR, MaaS)
4. **Dynamic client for CRDs** — Unlocks operator-managed projects. Without this, any project using CRDs falls back to code review.
5. **`readClusterState` real implementation** — Feeds accurate cluster info to the AI, so it generates deployment plans that match the actual cluster.
6. **Configurable quotas from plan** — Some stacks won't fit in the default 1Gi quota.

### Phase 3: Better testing intelligence
7. **Multi-endpoint agent context** — The agent knows about all services, not just the entry point. Leads to smarter investigation.
8. **`WaitForAllHealthy`** — Prevents the agent from running tests against a half-deployed stack.
9. **Prompt updates** — Improve AI deployment plan quality with `test_endpoint` schema.

### Phase 4: Robustness
10. **Sandbox timeout from plan** — Complex deployments need more than 30 min.
11. **Network policy egress adjustments** — Some projects need to reach external services.

### What to skip (not worth the complexity yet)
- **`depends_on` in steps**: The sequential step execution already handles ordering. Dependencies add complexity for marginal benefit.
- **Database bootstrapping via init Jobs**: Can be handled by command steps + wait. A dedicated init step type is premature abstraction.
- **Operator auto-installation**: Too much RBAC risk, and operators should be managed by cluster admins.

---

## Key Insight

The biggest bang for the buck is **Phase 1, item 1**: making `test_endpoint` work. Right now the full sandbox pipeline exists (analysis -> plan -> create namespace -> deploy manifests -> run steps -> pass endpoints to runner -> agent investigates), but it breaks at the last mile because the runner doesn't know WHICH endpoint to test. Fix that one thing and BBR-style projects become testable with the existing infrastructure.
