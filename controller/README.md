# OpinAI Controller

Kubernetes-native controller that watches GitHub repos for new issues and orchestrates AI-powered bug reproduction Jobs. Built in Go, runs on OpenShift.

## How it works

1. **Controller** (`opinai-go`) runs as a Deployment, polling GitHub for open issues
2. For each new issue, it creates a Kubernetes **Job**
3. The **Runner** (same binary, `--mode=runner`) runs inside the Job pod — fetches the issue, calls AI to categorize and generate tests, reproduces the bug, and posts a structured report
4. Issues are labeled `opinai-done` after processing
5. Results are stored in SQLite and displayed on the web dashboard

Single binary (~20MB), ~41MB container image (with runtime deps).

## Quick start

```bash
# Interactive setup — configures secrets, repos, and deploys
./setup.sh

# Or build manually
docker build -t opinai-controller -f Dockerfile ..
```

## Architecture

```
controller-go/
├── cmd/opinai/main.go           # Entrypoint (controller + runner modes)
├── internal/
│   ├── ai/                      # Multi-provider AI client (Vertex/Anthropic/OpenAI)
│   ├── controller/              # GitHub polling, K8s Job management
│   ├── dashboard/               # HTTP/HTTPS server, REST API, SSE streaming
│   ├── database/                # SQLite persistence
│   ├── runner/                  # Bug reproduction flow (runs in Job pods)
│   └── sandbox/                 # Sandbox namespace lifecycle
├── Dockerfile
└── go.mod
```

## Dashboard

The controller serves a web dashboard on ports 8080 (HTTP) and 8443 (HTTPS):

- **Main dashboard** — unified issue view with verdicts, confidence, categories
- **Admin panel** — repo management, deployment analysis, AI knowledge, logs
- **AI Chat** — ask OpinAI about any issue with streaming responses
- **Post preview** — review AI-generated comments before posting to GitHub

## Configuration

### Environment variables (via ConfigMap)

| Variable | Description | Default |
|----------|-------------|---------|
| `REPOS` | Comma-separated repos to watch | (required) |
| `POLL_INTERVAL_MINUTES` | Polling frequency | `60` |
| `DONE_LABEL` | Label applied to processed issues | `opinai-done` |

### Credentials (via Secret)

| Variable | Description |
|----------|-------------|
| `GITHUB_TOKEN` | GitHub token with repo + issues permissions |
| `AI_API_KEY` | API key for AI analysis (Anthropic/OpenAI) |
| `AI_PROVIDER` | `vertex`, `anthropic`, or `openai` |
| `AI_MODEL` | Model name |
| `AI_PROJECT` | Google Cloud project (Vertex AI) |
| `AI_REGION` | Google Cloud region (Vertex AI) |

## Features

- [x] GitHub issue polling + K8s Job creation
- [x] AI-powered issue categorization (BUG/FEATURE/QUESTION/DOCS)
- [x] Automated test generation + execution
- [x] AI verdict with confidence scoring
- [x] Web dashboard with real-time updates
- [x] SSE streaming (analysis, reproduction, chat, check-now)
- [x] SQLite persistence (survives pod restarts via PVC)
- [x] Sandbox namespace management (isolated deployments)
- [x] Deployment analysis (AI generates deployment options)
- [x] Self-healing reproduction (auto-retry with fixes)
- [x] Repo memory (learns from previous runs)
- [x] Admin panel (repo management, settings, logs)
- [x] AI chat with issue context
- [x] Post preview before GitHub comment
- [x] Multi-provider AI (Anthropic, OpenAI, Vertex AI)

## Security

- Credentials stored in K8s Secrets (never logged)
- Sandbox namespaces isolated with NetworkPolicy + ResourceQuota
- API keys sanitized from all outputs
- Self-signed TLS for HTTPS
