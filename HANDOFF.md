# OpinAI Handoff — Full Context for New Claude Code Sessions

## Project Overview
OpinAI is an AI-powered bug reproduction system. It monitors GitHub repos, deploys projects on OpenShift, generates AI test scripts, and validates whether reported bugs are real.

## Architecture

### Go Packages (controller-go/)
- `cmd/opinai/` — main entrypoint
- `internal/runner/` — **THE CORE**: clones repos, analyzes README, installs, starts servers, generates tests, runs them, produces verdicts
- `internal/ai/` — AI client (Vertex AI Claude), analyze, categorize, test generation, verdict
- `internal/controller/` — GitHub poller, job creation, pod harvesting
- `internal/dashboard/` — HTTP server, API endpoints, WebSocket, static files (embedded via go:embed)
- `internal/database/` — SQLite wrapper (runs, repo_memory, deployment_plans, processed_issues, chat)
- `internal/sandbox/` — K8s deployment manager (kustomize/helm apply)
- `internal/prompts/` — **All AI prompts as .txt files** (analyze_readme.txt, selfheal_install.txt, etc.) loaded via go:embed

### Key Files
- `internal/dashboard/static/index.html` — entire dashboard frontend (single HTML file with embedded JS/CSS)
- `internal/dashboard/static/style.css` — dashboard styles  
- `internal/runner/runner.go` — ~1000 lines, the brain of OpinAI
- `internal/prompts/*.txt` — all AI prompt templates with {{.Variable}} placeholders
- `Dockerfile` — Go 1.25 multi-stage build, alpine base with python3

### Database Schema (SQLite at /data/opinai.db)
- `runs` — id, repo, issue, title, category, verdict, confidence, report, posted, ai_powered, duration, created_at, suggested_questions, **repro_details** (JSON string)
- `repo_memory` — repo, key, value (stores: working_install_command, install_command, description, tech_stack, deployment_needs, monitored_since, etc.)
- `deployment_plans` — id, repo, plan_json (massive JSON with deployment options, steps, requirements), status, created_at, commit_sha
- `processed_issues` — repo, issue (tracks what's been checked)
- `chat_history` — repo, issue, role, content, created_at

## Build & Deploy

### BuildConfig
- Source: `https://github.com/yossiovadia/opinai.git` branch `main`
- Context dir: `controller-go` 
- Dockerfile: `Dockerfile` (relative to context dir)
- Image: `image-registry.openshift-image-registry.svc:5000/opinai/opinai-controller:latest`

### Commands
```bash
cd /Users/mooki/code/opinai/controller-go
go build -o /dev/null ./cmd/opinai    # local build check
go test ./... -count=1                 # run all tests

# JS syntax check (catches dangling braces that killed dashboard for hours)
sed -n '/<script>/,/<\/script>/p' internal/dashboard/static/index.html | sed '1d;$d' > /tmp/check.js && node --check /tmp/check.js

cd /Users/mooki/code/opinai
git add -A && git commit -m "msg" && git push
oc start-build opinai-controller -n opinai --follow
oc rollout restart deployment/opinai-controller -n opinai
```

### CRITICAL: After any HTML/JS change, run the node --check! A single dangling } killed the entire dashboard.

## Current State (April 3, 2026)

### What Works
- ✅ Deployment plan analysis for all 3 repos (llm-katan, MaaS, BBR) — incredibly detailed
- ✅ llm-katan issues: server deploys, tests run, bugs confirmed (#12 streaming bug = BUG_CONFIRMED HIGH)
- ✅ BBR #54: code review confirms header leak bug (BUG_CONFIRMED HIGH)
- ✅ AI chat per issue (🎳 button)
- ✅ Prompts extracted to .txt files
- ✅ Working install command only saved after server health confirmed
- ✅ Dashboard: WebSocket + HTTP fallback, hero stats, batch rerun, share button
- ✅ Subtitle: "That's just, like, your opinion, man."
- ✅ 50+ Go tests across all packages + JS syntax test + CI workflow

### What's Broken / Needs Fixing

#### 1. 🔧 Reproduction Details not visible in expanded rows
- `repro_details` field IS in the API response (JSON string with method, build_command, server_started, etc.)
- `renderReproDetails()` function exists and works when called manually
- `mergeIssues()` was JUST fixed to copy `repro_details` (commit 434af14)
- **VERIFY**: expand a row and confirm the 🔧 section appears
- The `deployment_option` field in repro_details is EMPTY — the runner builds the JSON but doesn't include the selected option name. Fix in runner.go where `OPINAI REPRODUCTION_DETAILS` is emitted.

#### 2. Install command self-heal overwriting good commands
- FIXED: `pendingInstallCmd` pattern — only saves after server health confirmed
- But the analyze_readme also generates `install_command` which may conflict
- The self-heal prompt was updated to NOT use --no-deps on lightweight deps

#### 3. llm-katan #11 NOT_REPRODUCIBLE
- Server starts fine, but the test doesn't properly reproduce the env var override bug
- The test needs to: stop server → restart with LLM_KATAN_MODEL=different → check API response
- Previous run (with old code) DID confirm this bug — the test script was better then

#### 4. MaaS issues — code review only
- MaaS is an operator project — needs full cluster deployment to properly test
- Current approach: code review (grep files) — works for script bugs (#658) but not runtime bugs
- The deployment plan exists with full kustomize steps but sandbox deployment not wired up yet

## Key Gotchas (learned the hard way)

1. **BuildConfig was pointing to Python Dockerfile** for the ENTIRE first day. Fixed to controller-go/Dockerfile with contextDir=controller-go
2. **ID type in deployment plan JSON is integer, not string** — use `any` type in Go struct
3. **`isK8sProject()` gate was blocking deployment plan usage** — REMOVED, plan drives everything now
4. **mergeIssues() must copy ALL fields** from run data — it was dropping repro_details, suggested_questions
5. **JS syntax errors kill the ENTIRE dashboard silently** — always run node --check
6. **Self-signed cert on passthrough route** — use edge TLS instead, WS works through edge
7. **go:embed embeds at build time** — strings command won't find embedded content
8. **OpenShift job pods use the SAME image as controller** — changes only take effect after build + rollout + new job

## Monitored Repos
1. `yossiovadia/llm-katan` — Python API server, echo mode, pip install
2. `opendatahub-io/models-as-a-service` — K8s operator, kustomize/script deploy
3. `opendatahub-io/ai-gateway-payload-processing` — Go ext-proc plugin, Helm deploy

## Open Tasks (Priority Order)
1. Fix repro_details visibility in dashboard (verify mergeIssues fix works)
2. Add deployment_option name to repro_details JSON in runner.go
3. Re-run llm-katan #11 — improve test to properly reproduce env var bug
4. Wire sandbox manager for K8s project deployment (MaaS)
5. Add more tests for every bug found
6. Consider: prompt tuning for better test generation quality
