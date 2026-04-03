# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Project Overview

OpinAI Controller is a Kubernetes-native Go service that automates GitHub issue reproduction and verification using AI. It polls GitHub repos for new issues, spawns K8s Jobs to reproduce them in isolated sandbox namespaces, uses AI (Anthropic/OpenAI/Vertex) to analyze results, and reports back on issues.

**Two runtime modes:** `controller` (orchestrates from a long-lived pod) and `runner` (short-lived per-issue pod created by the controller).

## Build & Development Commands

```bash
# Build
go build ./cmd/opinai

# Run tests
go test ./internal/...

# Run a single test
go test ./internal/database -run TestAddRun

# Lint
golangci-lint run

# Docker build (multi-stage, CGO disabled)
docker build -t opinai-controller .
```

Go version: 1.25. SQLite via `modernc.org/sqlite` (pure Go, no CGO needed).

## Architecture

### Entry Point & Modes
`cmd/opinai/main.go` — flag `--mode` selects `controller` or `runner`.

- **Controller mode**: Starts DB, job manager, poller, dashboard (HTTP :8080 / HTTPS :8443), sandbox manager. Uses in-cluster K8s config with kubeconfig fallback.
- **Runner mode**: Runs inside K8s Jobs. Fetches issue, reproduces it (sandbox/deployment-plan/standard), categorizes with AI, posts GitHub comment.

### Internal Packages

| Package | Role |
|---------|------|
| `internal/controller` | K8s Job creation/watching, GitHub API polling, issue fetching |
| `internal/runner` | In-pod reproduction orchestration, self-healing (AI-driven retry on failures) |
| `internal/dashboard` | chi-based HTTP server: REST API (34+ endpoints), SSE streaming, WebSocket hub, embedded frontend, rate limiting |
| `internal/database` | SQLite with WAL mode, write mutex serialization. Tables: `runs`, `repo_memory`, `processed_issues`, `deployment_plans`, `chat_history` |
| `internal/sandbox` | K8s namespace lifecycle: creation with network policies + resource quotas, resource deployment, 30-min auto-cleanup |
| `internal/ai` | Multi-provider client (Anthropic default, OpenAI, Vertex). Streaming + single-turn. Default model: `claude-sonnet-4-20250514` |
| `internal/prompts` | Embedded Go templates for AI prompts (categorize, verdict, test generation, self-heal, deployment analysis) |

### Key Data Flow
```
GitHub Issues → Poller → JobManager (max 3 concurrent) → K8s Jobs (runner pods)
    → Sandbox namespace created → Reproduce → AI categorize/verdict → DB + GitHub comment
```

### Adapters & Interfaces
The dashboard uses adapter structs (`hubAdapter`, `sandboxAdapter`) to bridge K8s/WebSocket components. Callback pattern for deferred binding of reproduce/verify-fix functions.

### Configuration (Environment Variables)
- **K8s**: `NAMESPACE`, `KUBECONFIG`
- **Repos**: `REPOS` (comma-separated), `POLL_INTERVAL_MINUTES` (default 60)
- **AI**: `AI_PROVIDER`, `AI_API_KEY`, `AI_MODEL`, `AI_BASE_URL`, `AI_PROJECT`, `AI_REGION`
- **GitHub**: `GITHUB_TOKEN`, `OPINAI_WEBHOOK_SECRET`
- **Runtime**: `OPINAI_IMAGE`, `OPINAI_DB_PATH` (default `/data/opinai.db`)
- **Runner-specific**: `REPO`, `ISSUE_NUMBER`, `SERVER_URL`, `OPINAI_SANDBOX_NAMESPACE`, `OPINAI_DEPLOYMENT_PLAN`, `OPINAI_VERIFY_FIX`

## Testing Patterns

- Table-driven tests throughout
- `database`: uses temp directories for SQLite
- `dashboard`: httptest servers, tests middleware (CORS, rate limiting), frontend JS syntax validation
- `controller`: tests GitHub response parsing, PR filtering, K8s resource extraction from YAML
- `runner`: tests container env setup, memory parsing, comment truncation
- `sandbox`: tests namespace creation and quota validation
- `ai`: tests provider selection and config loading

## Dashboard Rate Limiting

Two tiers: general paths (2 req/sec, burst 20) and AI paths (0.5 req/sec, burst 5). Token-bucket per IP.
