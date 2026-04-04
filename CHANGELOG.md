# Changelog

## v0.2.0 (2026-04-04) — Sandbox Deployment

### Highlights
- **Build from source**: OpenShift BuildConfig builds container images from any project's Dockerfile
- **Follow the docs**: AI reads project README/install docs and follows them exactly (no more guessing)
- **Helm chart support**: IMAGE_PLACEHOLDER auto-splits into registry/repository/tag sub-values
- **Full E2E tested**: BBR (Go K8s operator) built, deployed via Helm, and investigated in sandbox

### Sandbox Infrastructure
- Dynamic K8s client for CRD resources (EnvoyFilter, Gateway, etc.)
- Cluster-scoped RBAC support (ClusterRole/ClusterRoleBinding with sandbox-prefixed names)
- NAMESPACE_PLACEHOLDER and IMAGE_PLACEHOLDER auto-replacement in steps
- LimitRange for build pods (quota validation compatibility)
- WaitForAllHealthy checks all Deployments/StatefulSets after deploy
- Auto-cleanup: sandbox namespaces, labeled ClusterRoles, Helm-managed RBAC
- Configurable timeout from deployment plan

### Deployment Planner
- Reads project install docs (README, CONTRIBUTING, Makefile targets)
- Renders Helm templates and includes them as context
- Feeds rich agent analysis to planner (endpoints, RBAC needs, tech stack)
- Tool-aware prompts (only kubectl/helm/oc/curl available in sandbox)
- Single recommended option + code-review fallback

### Container Tooling
- kubectl, helm, oc (real binary with gcompat) in controller image
- `oc start-build` for reliable build triggering
- Build timeout: 30 min (Go builds on single-node clusters)

### Bug Fixes
- Namespace injection replaces AI-hardcoded namespaces (not just skips)
- Clone cleanup between retry options (no stale /tmp clones)
- Auto-detect K8s repos from rich_analysis `needs_cluster`
- Helm stale ClusterRole cleanup on sandbox teardown

## v0.1.0 (2026-04-03) — Initial Release
- Agentic bug reproduction with tool-use investigation loop
- Deep repo analysis (endpoints, auth, error handling, config)
- Code-review mode for K8s/cluster projects
- Multi-provider AI support (Anthropic, OpenAI, Vertex AI)
- Serial job queue with direct result callback
- Live dashboard with SSE streaming
- Persistent monitored repos
- Security: race condition fixes, XSS protection, optional API auth, CORS
