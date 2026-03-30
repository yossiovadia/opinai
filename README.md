# 🎳 OpinAI

**"That's just, like, your opinion, man."**

AI-powered infrastructure-level bug reproduction and validation. You *think* it's a bug? Let OpinAI verify it on real infrastructure.

<p align="center">
  <img src="assets/demo.svg" alt="OpinAI Demo" width="800">
</p>

[**→ Watch the Full Interactive Demo**](https://yossiovadia.github.io/opinai-demo/)

---

## Quick Start

Add this workflow to your repo at `.github/workflows/opinai.yml`:

```yaml
name: OpinAI Bug Reproduction
on:
  issues:
    types: [labeled]

jobs:
  reproduce:
    if: github.event.label.name == 'needs-reproduction'
    runs-on: ubuntu-latest
    permissions:
      issues: write
    steps:
      - uses: actions/checkout@v4
      - uses: yossiovadia/opinai@v1
        with:
          issue_number: ${{ github.event.issue.number }}
          install_command: 'pip install -e .'
          server_command: 'python -m myapp serve --port 8000'
          github_token: ${{ secrets.GITHUB_TOKEN }}
```

When someone labels an issue with `needs-reproduction`, OpinAI:

1. Installs your server
2. Starts it
3. Reads the issue to understand the bug
4. Runs targeted protocol compliance tests
5. Posts a structured report as a comment on the issue

**Zero cost. Zero config. Works with any HTTP server.**

## Inputs

| Input | Required | Default | Description |
|-------|----------|---------|-------------|
| `issue_number` | ✅ | — | GitHub issue number to investigate |
| `install_command` | ❌ | `''` | Command to install your server |
| `server_command` | ❌ | `''` | Command to start your server |
| `server_url` | ❌ | `http://localhost:8000` | Base URL of your server |
| `wait_seconds` | ❌ | `15` | Seconds to wait for server startup |
| `test_script` | ❌ | `''` | Path to custom test script (overrides built-in) |
| `github_token` | ✅ | — | GitHub token for posting comments |

## Examples

| Use Case | Example |
|----------|---------|
| Python FastAPI/Flask | [python-fastapi.yml](examples/python-fastapi.yml) |
| Go service | [go-service.yml](examples/go-service.yml) |
| Docker-based | [docker.yml](examples/docker.yml) |
| External/staging server | [external-server.yml](examples/external-server.yml) |
| Custom test script | [custom-tests.yml](examples/custom-tests.yml) |
| LLM API server (llm-katan) | [workflow.yml](examples/workflow.yml) |

## Report Format

OpinAI posts a structured comment on the issue:

```
## 🎳 OpinAI — Bug Reproduction Report

**Issue:** #42
**Timestamp:** 2026-03-26T12:00:00Z

### Results

| Test | Status | Details |
|------|--------|---------|
| Health endpoint | ✅ Pass | Returns 200 |
| Response schema | ✅ Pass | All required fields present |
| Streaming SSE | 🔴 Fail | Missing [DONE] terminator |

### Verdict
🔴 **Bug Confirmed** — 1 of 3 tests failed
```

## What is OpinAI?

OpinAI automatically reproduces bugs on real infrastructure. When someone files a bug report, OpinAI:

1. **Reads** the issue and understands what the bug is
2. **Checks feasibility** — can this bug be reproduced?
3. **Provisions** the test environment
4. **Reproduces** the bug with real requests and captures structured evidence
5. **Reports** back with proof: confirmed, not reproduced, or partially reproduced
6. **Tracks** the bug as a regression test — re-validates until the fix lands

No human debugging time wasted. No "works on my machine." Real evidence from real infrastructure.

## Why OpinAI?

| Tool | Reads Issues | Provisions Infra | Real Hardware | Multi-Service | Evidence | Regression |
|------|:---:|:---:|:---:|:---:|:---:|:---:|
| SWE-agent | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| GitHub Agentic Workflows | ✅ | ❌ | ❌ | ❌ | Partial | ❌ |
| CI/CD tools | ❌ | ✅ | ❌ | ✅ | ❌ | ❌ |
| **OpinAI** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |

## Proven at Scale

OpinAI was born from the [vLLM Semantic Router](https://github.com/vllm-project/semantic-router) project, where it:

- ✅ Validated **8 known bugs** on real GPU hardware (RTX 4090)
- ✅ **Discovered 6 new bugs** through AI-driven bug hunting
- ✅ **Validated a fix** (PR #1502) end-to-end
- ✅ Runs **daily regression** checks
- ✅ Correctly **identified 1 false positive**

## Custom Test Scripts

Override the built-in tests with your own:

```bash
#!/bin/bash
# my-tests.sh — OpinAI will run this instead of built-in tests

SERVER_URL="${SERVER_URL:-http://localhost:8000}"
RESULTS_DIR="${RESULTS_DIR:-/tmp/opinai}"
mkdir -p "$RESULTS_DIR"

# Test 1
response=$(curl -s "$SERVER_URL/api/endpoint")
if echo "$response" | jq -e '.field' > /dev/null 2>&1; then
  echo '{"test":"my test","provider":"custom","status":"pass","details":"field present"}' > "$RESULTS_DIR/test_001.json"
else
  echo '{"test":"my test","provider":"custom","status":"fail","details":"field missing"}' > "$RESULTS_DIR/test_001.json"
fi
```

## Architecture

```
┌─────────────────┐     ┌──────────────┐     ┌───────────────────┐
│  GitHub Issue    │────▶│   OpinAI     │────▶│  Your Server      │
│  labeled with    │     │              │     │                   │
│  needs-          │     │  1. Read     │     │  Started by       │
│  reproduction    │     │  2. Analyze  │     │  install_command   │
│                  │     │  3. Test     │     │  + server_command  │
│                  │     │  4. Report   │     │                   │
└─────────────────┘     └──────────────┘     └───────────────────┘
```

## Status

- [x] GitHub Action with protocol compliance tests
- [x] Structured issue commenting
- [x] Keyword-based test scoping
- [x] Multi-provider LLM API testing (OpenAI, Anthropic, Bedrock, Vertex, Azure)
- [x] Custom test script support
- [ ] AI-powered issue analysis (coming soon)
- [ ] Self-hosted GPU runner support
- [ ] OpenShift Operator
- [ ] Regression tracking

## License

Apache-2.0

---

*The Dude abides... and so does OpinAI.* 🎳

*Created by [Yossi Ovadia](https://github.com/yossiovadia)*
