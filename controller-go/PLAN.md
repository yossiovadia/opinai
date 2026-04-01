# OpinAI Go Rewrite Plan

See the full plan at: /Users/mooki/.claude/plans/typed-purring-reef.md

## Quick Reference

### Phase 1: Scaffold + Database — `go build` + health endpoint + SQLite
### Phase 2: Dashboard API — All REST endpoints, static file serving
### Phase 3: GitHub Integration — Polling, issue reading, Job creation
### Phase 4: AI Integration — Anthropic/OpenAI/Vertex client
### Phase 5: Job Reconciler — controller-runtime watch + log parsing
### Phase 6: SSE Streaming — 4 streaming endpoints
### Phase 7: Sandbox Manager — Namespace lifecycle
### Phase 8: Deployment Analysis — AI-powered repo analysis
### Phase 9: Chat — Streaming AI chat
### Phase 10: Runner — Reproduction in Job pods

### Key Libraries
- `sigs.k8s.io/controller-runtime` — K8s controller framework
- `github.com/go-chi/chi/v5` — HTTP router
- `modernc.org/sqlite` — Pure Go SQLite (no CGO)
- `golang.org/x/oauth2` — Google Cloud auth

### Run
```bash
go build ./cmd/opinai
./opinai --mode=controller  # default
./opinai --mode=runner      # in Job pods
```
