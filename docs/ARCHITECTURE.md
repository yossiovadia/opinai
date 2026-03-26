# AI Bug Reproducer — Infrastructure-Level Automated Bug Validation

**Date:** 2026-03-13
**Origin:** VSR Lab work — realized nobody's doing this at the infra level
**Status:** Idea / early concept

## The Gap

Every existing tool stops short:
- **SWE-bench / SWE-agent** — reads issue, works at code level (patches, unit tests). No real infra.
- **GitHub Agentic Workflows** — triggers on issues, but sandboxed to GH Actions runners. No GPU, no custom topology.
- **CI/CD tools** — can spin up environments but have no AI reading the bug report to figure out *what* to spin up.

**Nobody does the full loop:**
Issue reported → AI reads it → provisions the right environment → writes reproduction → runs it → reports structured evidence back to the issue.

## The Concept

### Trigger
- GitHub webhook on `issues.opened` or `issues.labeled` (e.g., label `needs-reproduction`)
- Could also trigger on issue comments ("can someone reproduce this?")

### AI Analysis Phase
1. Read the issue title + body + any linked PRs/discussions
2. Determine what the bug actually is (classify: routing bug? cache bug? auth bug? performance?)
3. Map the bug to required infrastructure:
   - What services are needed? (proxy, router, database, cache, LLM endpoint, etc.)
   - What configuration? (specific settings that trigger the bug)
   - What data/state? (pre-populated cache, specific model loaded, etc.)
4. **Feasibility check** — can we reproduce this given available hardware/services?
5. Design the reproduction steps (what to send, what to check, what proves the bug)

### Environment Provisioning
- Topology-aware: knows what infra is available (GPUs, services, endpoints)
- Spins up only what's needed for THIS specific bug
- Options:
  - Local hardware (self-hosted runner with GPU)
  - Cloud instances (spin up on-demand, tear down after)
  - Container-based (Docker Compose for service topology)
  - Hybrid (some local, some cloud)
- Configuration generation: writes the config files needed to trigger the bug

### Reproduction Execution
- Writes a reproduction script from the bug description
- Runs it against the live environment
- Captures:
  - Request/response data
  - Log output from each service
  - Timing information
  - Headers, cache states, routing decisions
- Structured output: JSON with clear pass/fail + evidence

### Reporting
- Comments on the GitHub issue with:
  - ✅ "Bug confirmed" or ❌ "Could not reproduce" or ⚠️ "Partially reproduced"
  - Structured evidence (what was sent, what was received, what went wrong)
  - Environment details (what was spun up, what versions)
  - Reproduction script (so humans can re-run it)
  - Confidence level
- If confirmed: auto-label the issue, add to regression suite
- If not reproduced: ask reporter for more details

### Living Regression
- Confirmed bugs become permanent regression tests
- Re-run periodically (daily/weekly)
- When fix PR merges → automatically re-run → detect if bug is actually fixed
- Auto-comment: "This bug appears to be fixed as of commit X"
- Auto-close or suggest closing

## What Makes This Different

| Aspect | SWE-agent | GitHub Agentic | **This** |
|--------|-----------|---------------|----------|
| Reads issues | ✅ | ✅ | ✅ |
| Understands code | ✅ | ✅ | ✅ |
| Provisions infra | ❌ | ❌ | ✅ |
| Real hardware (GPU) | ❌ | ❌ | ✅ |
| Multi-service topology | ❌ | ❌ | ✅ |
| Structured evidence | ❌ | Partial | ✅ |
| Regression tracking | ❌ | ❌ | ✅ |
| Auto-detects fixes | ❌ | ❌ | ✅ |

## Target Use Cases (beyond VSR)

- **LLM serving infrastructure** (vLLM, TGI, Triton) — bugs often need GPU + model + specific config
- **Service mesh / proxy** (Envoy, Istio) — bugs need multi-service topology
- **Database systems** — bugs need specific data patterns, schema, load
- **Distributed systems** — bugs need multiple nodes, network partitions
- **ML pipelines** — bugs need specific model + data + hardware combination
- **Any project where "works on my machine" is a real problem**

## Architecture Options

### Option A: Self-Hosted Agent (what we built for VSR Lab)
- Dedicated hardware (yos with RTX 4090)
- AI agent SSHes in, manages services
- Cheap, fast, limited to available hardware
- Good for: single project, known topology

### Option B: Cloud-Provisioned
- AI agent calls cloud APIs to spin up instances
- Installs services, runs reproduction, tears down
- Expensive per-run, but can match any topology
- Good for: open-source projects with diverse infra needs

### Option C: Container-Based (most practical for v1)
- AI reads issue → generates Docker Compose topology
- Spins up containers with right services/configs
- Runs reproduction inside the compose network
- Moderate cost, works on any machine with Docker + GPU passthrough
- Good for: broad adoption, standard deployment

### Option D: Hybrid
- Lightweight bugs → containers
- GPU-dependent bugs → route to self-hosted GPU runner
- Cloud fallback for exotic hardware (TPU, multi-GPU, etc.)

## MVP Path

1. Start with VSR Lab (what we have now) — prove the concept works
2. Abstract the pattern into a reusable framework:
   - Issue parser (reads GitHub issue → structured bug description)
   - Topology planner (bug description → required infrastructure)
   - Environment provisioner (infrastructure plan → running services)
   - Reproduction engine (bug description + live env → test script + execution)
   - Evidence reporter (results → GitHub comment)
3. Package as a GitHub Action + self-hosted runner config
4. Open-source it

## Name Ideas
- **Repro** — "AI that reproduces your bugs"
- **BugWitness** — "automated bug witness"
- **InfraProbe** — infrastructure-level bug probing
- **ReproBot** — straightforward
- **GroundTruth** — validates bugs against real infrastructure

## Competitive Moat
- The feasibility check / topology awareness is the hard part
- Knowing what infra a bug needs from a text description = genuine AI reasoning problem
- The regression loop (reproduce → track → detect fix) is sticky — once a project has it, they won't give it up
- Hardware requirement is a barrier to entry but also a differentiator (can't do this with just Docker on GH Actions)

## Connection to Red Hat / VSR
- Fits the "agentic workflow security" angle Huamin/Joe are interested in
- Could position as: "semantic validation layer" — AI validates that bugs are real before humans spend time on them
- VSR itself could benefit from this (validate routing bugs, cache bugs, auth bugs)
- Could be a Red Hat product/service for OpenShift customers (managed bug reproduction infra)
