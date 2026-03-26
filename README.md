# 🎳 OpinAI

**"That's just, like, your opinion, man."**

AI-powered infrastructure-level bug reproduction and validation. You *think* it's a bug? Let OpinAI verify it on real infrastructure.

[**→ Watch the Demo**](https://yossiovadia.github.io/reprobot-demo/)

---

## What is OpinAI?

OpinAI automatically reproduces bugs on real infrastructure. When someone files a bug report, OpinAI:

1. **Reads** the issue and understands what the bug is
2. **Checks feasibility** — can this bug be reproduced with available hardware?
3. **Provisions** the test environment (containers, GPU pods, services)
4. **Reproduces** the bug with real requests and captures structured evidence
5. **Reports** back with proof: confirmed, not reproduced, or partially reproduced
6. **Tracks** the bug as a regression test — re-validates daily until the fix lands
7. **Validates** the fix when the PR merges — proves the bug is actually gone

No human debugging time wasted. No "works on my machine." Real evidence from real infrastructure.

## Why OpinAI?

Every existing tool stops short:

| Tool | Reads Issues | Provisions Infra | Real Hardware | Multi-Service | Evidence | Regression |
|------|:---:|:---:|:---:|:---:|:---:|:---:|
| SWE-agent | ✅ | ❌ | ❌ | ❌ | ❌ | ❌ |
| GitHub Agentic Workflows | ✅ | ❌ | ❌ | ❌ | Partial | ❌ |
| CI/CD tools | ❌ | ✅ | ❌ | ✅ | ❌ | ❌ |
| **OpinAI** | ✅ | ✅ | ✅ | ✅ | ✅ | ✅ |

## How It Works

```
┌─────────────────┐     ┌──────────────┐     ┌───────────────────┐
│  GitHub Issue    │────▶│   OpinAI     │────▶│  Test Environment │
│  "Cache leaks   │     │              │     │                   │
│   user data"    │     │  1. Analyze  │     │  ┌─── Router ───┐ │
│                 │     │  2. Plan     │     │  ├─── Envoy  ───┤ │
│  #1448          │     │  3. Deploy   │     │  ├─── Ollama ───┤ │
│                 │     │  4. Test     │     │  └─── Cache  ───┘ │
└─────────────────┘     │  5. Report   │     └───────────────────┘
                        └──────┬───────┘
                               │
                        ┌──────▼───────┐
                        │   Evidence   │
                        │              │
                        │ 🔴 CONFIRMED │
                        │ Bob got      │
                        │ Alice's data │
                        │ (sim=0.92)   │
                        └──────────────┘
```

## Proven at Scale

OpinAI was born from the [vLLM Semantic Router](https://github.com/vllm-project/semantic-router) project, where it:

- ✅ Validated **8 known bugs** on real GPU hardware (RTX 4090)
- ✅ **Discovered 6 new bugs** through AI-driven bug hunting
- ✅ **Validated a fix** (PR #1502) end-to-end with before/after evidence
- ✅ Runs **daily regression** checks automatically
- ✅ Correctly **identified 1 false positive** (preventing an embarrassing issue filing)

### Bugs Reproduced

| Bug | Severity | What OpinAI Found |
|-----|----------|-------------------|
| Cache cross-user data leak | P1 | Bob received Alice's $12,847.53 via cache hit (similarity 0.92) |
| Looper header injection | P0 | Security plugins completely bypassed via crafted headers |
| Giant prompt DoS | P1 | 100 chars → 830ms, 10K chars → 60s (super-linear growth) |
| No session affinity | P2 | 28x model parameter drop mid-conversation |
| Identity header spoofing | P1 | User identity forgeable without ext_authz |
| Cache stores without plugin | P2 | Per-decision cache opt-out broken |
| Feedback API unauthenticated | P2 | Elo ratings manipulable without auth |
| Cost explosion risk | P2 | No limits on looper breadth_schedule |

## Target Use Cases

OpinAI works for any project where bugs need real infrastructure to reproduce:

- **LLM serving** (vLLM, TGI, Triton) — bugs need GPU + model + specific config
- **Service mesh / proxy** (Envoy, Istio) — bugs need multi-service topology
- **Database systems** — bugs need specific data patterns and load
- **Distributed systems** — bugs need multiple nodes and network conditions
- **ML pipelines** — bugs need specific model + data + hardware combinations
- **Any project where "works on my machine" is a real problem**

## Architecture

### Deployment Options

| Option | Best For | How It Works |
|--------|----------|-------------|
| **Self-hosted** | Single project, known topology | AI agent SSHes into dedicated hardware |
| **Container-based** | Broad adoption, standard deployment | Docker Compose with GPU passthrough |
| **OpenShift native** | Enterprise, Red Hat customers | Operator spins up ephemeral namespaces with GPU scheduling |
| **Hybrid** | Complex topologies | Lightweight → containers, GPU → self-hosted, exotic → cloud |

### Core Components

```
opinai/
├── analyzer/        # Reads GitHub issues, classifies bugs
├── planner/         # Maps bugs to required infrastructure
├── provisioner/     # Spins up test environments
├── reproducer/      # Executes reproduction scripts
├── reporter/        # Posts structured evidence to GitHub
├── regression/      # Tracks bugs, detects fixes
└── manifests/       # Project-specific topology definitions
```

## Quick Start

### GitHub Action (coming soon)

```yaml
on:
  issues:
    types: [opened, labeled]

jobs:
  reproduce:
    if: contains(github.event.issue.labels.*.name, 'needs-reproduction')
    runs-on: self-hosted  # GPU runner
    steps:
      - uses: yossiovadia/opinai@v1
        with:
          issue: ${{ github.event.issue.number }}
          topology: ./opinai-manifest.yaml
```

### Self-hosted

```bash
# Clone and configure
git clone https://github.com/yossiovadia/opinai.git
cd opinai
cp config.example.yaml config.yaml  # Edit with your infra details

# Run against a specific issue
opinai reproduce --repo owner/repo --issue 1448

# Run regression suite
opinai regress --repo owner/repo
```

## Demo

Watch the full OpinAI flow — from issue filing to bug confirmation to fix validation:

[**→ Interactive Demo**](https://yossiovadia.github.io/reprobot-demo/)

## Status

🚧 **Early development** — core concepts proven, packaging in progress.

- [x] Concept validated on vLLM Semantic Router (8 bugs, 6 discoveries)
- [x] Fix validation workflow proven (PR #1502)
- [x] Daily regression cron running
- [x] Interactive demo
- [ ] Standalone CLI tool
- [ ] GitHub Action
- [ ] OpenShift Operator
- [ ] Plugin system for arbitrary projects

## Contributing

OpinAI is open source under Apache-2.0. Contributions welcome.

## License

Apache-2.0

---

*The Dude abides... and so does OpinAI.* 🎳

*Created by [Yossi Ovadia](https://github.com/yossiovadia)*
