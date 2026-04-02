# OpinAI Go Controller — Implementation Complete

All 10 phases implemented. The Go binary is now the primary (and only) controller.

## Phases (all complete)

| Phase | Description | Status |
|-------|-------------|--------|
| 1 | Scaffold + Database + Health endpoint | Done |
| 2 | Full Dashboard API (34 endpoints) | Done |
| 3 | GitHub polling + K8s Job management | Done |
| 4 | AI integration (Vertex/Anthropic/OpenAI) | Done |
| 5 | SSE streaming (4 endpoints) | Done |
| 6 | Sandbox namespace management | Done |
| 7 | Deployment analysis integration | Done |
| 8 | Deployment analysis AI | Done |
| 9 | AI chat with context | Done |
| 10 | Runner (reproduction in Job pods) | Done |
| + | Self-healing retries + repo memory | Done |

## Run

```bash
# Controller mode (default)
./opinai-go

# Runner mode (inside Job pods)
./opinai-go --mode=runner

# Custom ports
./opinai-go --http=:8080 --https=:8443

# Custom DB path
./opinai-go --db=/data/opinai.db
```

## Key libraries
- `k8s.io/client-go` — Kubernetes API
- `github.com/go-chi/chi/v5` — HTTP router
- `modernc.org/sqlite` — Pure Go SQLite (no CGO)
- `golang.org/x/oauth2/google` — Vertex AI ADC auth
