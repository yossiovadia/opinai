# Changelog

## v0.2.0 — Sandbox Deployment

Sandbox deployment for complex K8s projects. OpinAI can now build, deploy, and test
multi-component projects (operators, proxies, API+DB stacks) in isolated namespaces.

### Sandbox Infrastructure
- **OpenShift BuildConfig**: Build container images from Dockerfiles inside the cluster via `build` step type
- **Dynamic CRD client**: Deploy operator-managed resources (EnvoyFilter, Gateway, AuthPolicy, etc.) via `k8s.io/client-go/dynamic`
- **Cluster-scoped RBAC**: ClusterRole/ClusterRoleBinding manifests are created with sandbox-prefixed names and auto-cleaned on teardown
- **Helm RBAC cleanup**: Helm-managed cluster RBAC (identified by `meta.helm.sh/release-namespace` annotation) is cleaned up on sandbox teardown
- **Configurable quotas**: Resource quotas and sandbox timeout are configurable from the deployment plan
- **LimitRange defaults**: Sandbox namespaces get default container resource requests/limits so build pods pass quota validation
- **WaitForAllHealthy**: After deployment steps complete, waits for all Deployments, StatefulSets, and Services to be ready with exponential backoff

### Deployment Planning
- **Follow project docs**: AI reads README, CONTRIBUTING.md, install docs, and Makefile targets to follow the project's own install instructions instead of generating manifests from scratch
- **Rendered manifests**: Helm charts and kustomize overlays are pre-rendered and included in the AI prompt as source of truth for RBAC, ports, and probes
- **Rich analysis context**: Agent-discovered project knowledge (endpoints, architecture, RBAC needs) is fed to the deployment planner
- **IMAGE_PLACEHOLDER**: Auto-replaced with built image URL; for Helm `--set key.image=IMAGE_PLACEHOLDER`, auto-split into `.image.registry`, `.image.repository`, `.image.tag`
- **NAMESPACE_PLACEHOLDER**: Auto-replaced with sandbox namespace in all step content

### Runner Integration
- **test_endpoint**: Deployment plans specify which service is the test entry point; runner constructs serverURL from service FQDN instead of picking randomly
- **all_endpoints context**: Full service topology is passed to the agent for multi-service architecture awareness
- **K8s auto-detection**: Repos analyzed by the agent automatically get sandbox deployment if `needs_cluster` is set in repo_memory

### Operational
- **Auto-cleanup in poller**: Orphaned sandboxes are cleaned up every poll cycle
- **Namespace uniqueness**: Random suffix prevents collisions between concurrent sandbox attempts
- **Clone cleanup**: Stale `/tmp` clones are cleaned between deployment retry attempts
- **Manifest propagation delay**: 2-second pause between manifest and command steps for API server consistency
- **Namespace injection**: `injectNamespace` replaces env var references (`$GATEWAY_NAMESPACE`) and hardcoded namespaces with the sandbox NS
- **Setup script** (`scripts/setup.sh`): Complete RBAC setup including OpenShift Build/Image APIs, controller-runtime leases, and CRD discovery

## v0.1.0 — Initial Release

Kubernetes-native Go controller for automated GitHub issue reproduction using AI.

### Core
- Polls GitHub repos for new issues, spawns K8s Jobs for reproduction
- AI-powered categorization (bug/feature/question) and verdict (confirmed/not reproducible)
- Agent-based investigation loop with tool use (read files, grep, run tests, HTTP requests)
- SQLite database with WAL mode for runs, repo memory, deployment plans, chat history

### Dashboard
- REST API (30+ endpoints) with SSE streaming and WebSocket push
- Bearer token authentication (`OPINAI_API_TOKEN`)
- CORS lockdown (`OPINAI_ALLOWED_ORIGINS`)
- Rate limiting (general + AI-specific tiers)
- Graceful shutdown on SIGTERM/SIGINT

### Persistence
- Monitored repos survive controller restarts (stored in SQLite)
- Repo memory accumulates across runs (install commands, test strategies, deployment types)

### Multi-Provider AI
- Anthropic (default), OpenAI, Vertex AI
- Streaming + single-turn modes
