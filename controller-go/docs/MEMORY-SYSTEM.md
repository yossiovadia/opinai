# Intelligent Memory System for OpinAI

**Design Document — v0.1 Draft**
**Date:** 2026-04-09
**Author:** Yossi Ovadia
**Status:** Proposal

---

## Table of Contents

1. [Problem Statement](#1-problem-statement)
2. [Current State](#2-current-state)
3. [Proposed Architecture](#3-proposed-architecture)
4. [Memory Event Schema](#4-memory-event-schema)
5. [Self-Correction Rules](#5-self-correction-rules)
6. [User Interface — Memory Journal](#6-user-interface--memory-journal)
7. [Outcome Tracking — RL-Inspired](#7-outcome-tracking--rl-inspired)
8. [Cross-Investigation Learning](#8-cross-investigation-learning)
9. [Portability — Ambient Integration](#9-portability--ambient-integration)
10. [Risks and Mitigations](#10-risks-and-mitigations)
11. [Implementation Phases](#11-implementation-phases)
12. [Open Questions](#12-open-questions)

---

## 1. Problem Statement

OpinAI's memory is a lobotomy patient with a notebook. It can write things down, but it never re-reads its notes, never crosses out mistakes, and never notices when the world changed around it.

**Concrete failure modes today:**

- **Stale commands:** OpinAI learns `pip install -r requirements.txt` works for a repo. The maintainer migrates to Poetry. OpinAI keeps using pip, fails, and has no mechanism to update its memory from the failure. A human has to delete the repo and re-add it.

- **Wrong verdicts persist:** OpinAI investigates issue #42, says BUG_CONFIRMED with HIGH confidence. The issue author clarifies it was user error and closes it. OpinAI's memory still says "BUG_CONFIRMED HIGH" — and uses that false context in future investigations on the same repo.

- **No learning from outcomes:** OpinAI reviews a PR and says APPROVE. The PR gets merged, then reverted because it broke production. OpinAI has no idea this happened and continues to trust its own review quality at the same level.

- **Silent overwrites:** When memory values change, the old value is destroyed. There's no audit trail. If a README re-analysis produces worse commands than what was working before, the working knowledge is silently replaced.

- **No cross-referencing:** OpinAI investigates a streaming bug caused by `DashboardMiddleware` buffering responses. Two weeks later, it reviews a PR that refactors `DashboardMiddleware`. It has no mechanism to flag: "This middleware was the root cause of issue #10 — be careful here."

The root cause is architectural: memory is a flat key-value store with upsert semantics. There is no history, no provenance, no feedback loop, and no staleness detection. This design proposes replacing that with a versioned event ledger that learns from outcomes.

---

## 2. Current State

### Storage

SQLite table `repo_memory` — flat key-value per repo:

```sql
CREATE TABLE repo_memory (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo TEXT NOT NULL,
    key TEXT NOT NULL,
    value TEXT,
    updated_at TEXT DEFAULT (datetime('now')),
    UNIQUE(repo, key)
);
```

Two functions: `SetRepoMemory(repo, key, value)` and `GetRepoMemory(repo, key)`. That's the entire API surface.

### What Gets Stored (26 distinct keys)

| Category | Keys | Set By |
|----------|------|--------|
| **Project identity** | `description`, `tech_stack`, `deployment_type`, `needs_cluster` | README/agent analysis |
| **Commands** | `install_command`, `run_command`, `build_command`, `health_endpoint` | README analysis |
| **Proven commands** | `working_install_command`, `working_run_command` | Runner (after health check passes) |
| **Deep knowledge** | `rich_analysis` (~8KB JSON), `runtime_requirements` (JSON) | Agent analyzer, deployment analyzer |
| **Testing** | `test_strategy`, `how_to_test` | README analysis |
| **Deployment** | `working_deploy_option`, `deploy_option_{id}_error` | Sandbox deployer |
| **Session** | `last_analyzed_issue`, `last_verdict`, `last_confidence` | Runner (per-issue) |
| **Tracking** | `monitored_since` | Poller |

### Data Flow

```
GitHub repo added → Poller → README analysis → Memory (initial knowledge)
                           → Agent analysis  → Memory (rich_analysis)
Issue investigation → Runner reads Memory → reproduces → writes back proven commands
PR review → reads Memory + runs → generates review (doesn't write back)
Dashboard → reads Memory → displays to user (admin page)
```

### What's Missing

| Capability | Status |
|-----------|--------|
| Version history | None — upsert destroys previous value |
| Change reasoning | None — no "why" attached to updates |
| Staleness detection | None — no expiry, no freshness checks |
| Outcome feedback | None — verdicts/reviews never validated against reality |
| Cross-referencing | None — investigations don't inform PR reviews |
| Undo | None — old values are gone |
| Confidence tracking | Implicit only (working_* > analyzed) |
| Export | None — locked in SQLite |

---

## 3. Proposed Architecture

### Core Idea: Event Sourcing for AI Knowledge

Replace the flat key-value store with an **append-only event ledger**. The current state of memory is derived by replaying events, but every change is preserved with its reasoning, source, and confidence.

```
┌─────────────────────────────────────────────────────┐
│                   Memory System                      │
│                                                      │
│  ┌──────────────┐    ┌──────────────┐               │
│  │ Event Ledger │───▶│ Materialized │               │
│  │ (append-only)│    │    View      │               │
│  │              │    │ (current     │               │
│  │  event_1     │    │  state)      │               │
│  │  event_2     │    │              │               │
│  │  event_3     │    │ key → value  │               │
│  │  ...         │    │ key → value  │               │
│  └──────────────┘    └──────┬───────┘               │
│         │                   │                        │
│         ▼                   ▼                        │
│  ┌──────────────┐    ┌──────────────┐               │
│  │  Staleness   │    │   Outcome    │               │
│  │  Detector    │    │   Tracker    │               │
│  │              │    │              │               │
│  │ "This key    │    │ "Verdict was │               │
│  │  is 45 days  │    │  wrong 3/5   │               │
│  │  old, repo   │    │  times for   │               │
│  │  changed"    │    │  this repo"  │               │
│  └──────────────┘    └──────────────┘               │
│         │                   │                        │
│         ▼                   ▼                        │
│  ┌──────────────────────────────────┐               │
│  │       Self-Correction Engine     │               │
│  │                                  │               │
│  │  Evaluates triggers, emits new   │               │
│  │  events to update/invalidate     │               │
│  │  stale or incorrect knowledge    │               │
│  └──────────────────────────────────┘               │
│                      │                               │
│                      ▼                               │
│  ┌──────────────────────────────────┐               │
│  │       Memory Journal (UI)        │               │
│  │                                  │               │
│  │  Timeline · Diffs · Undo · Stats │               │
│  └──────────────────────────────────┘               │
└─────────────────────────────────────────────────────┘
```

### Backward Compatibility

The existing `SetRepoMemory` / `GetRepoMemory` API continues to work. Internally, `SetRepoMemory` emits an event and updates the materialized view. `GetRepoMemory` reads from the materialized view. Zero changes required in callers for MVP.

### New Components

| Component | Responsibility |
|-----------|---------------|
| **Event Ledger** | Append-only storage of all memory changes with metadata |
| **Materialized View** | Current-state key-value (replaces existing `repo_memory` table semantics) |
| **Staleness Detector** | Background goroutine that scans for stale entries on a schedule |
| **Outcome Tracker** | Correlates predictions (verdicts, reviews) with real-world results |
| **Self-Correction Engine** | Evaluates triggers and emits corrective events |
| **Memory Journal** | Dashboard UI for transparency and manual overrides |
| **Export API** | JSON/YAML export of memory state and history |

---

## 4. Memory Event Schema

### SQLite Table

```sql
CREATE TABLE memory_events (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo TEXT NOT NULL,
    key TEXT NOT NULL,
    value TEXT,
    previous_value TEXT,
    reason TEXT NOT NULL,
    source TEXT NOT NULL,           -- enum below
    source_ref TEXT,                -- issue #, commit SHA, PR #, run ID
    confidence TEXT DEFAULT 'medium', -- low | medium | high | verified
    reversible INTEGER DEFAULT 1,
    created_at TEXT DEFAULT (datetime('now')),
    superseded_by INTEGER REFERENCES memory_events(id)
);

CREATE INDEX idx_memory_events_repo_key ON memory_events(repo, key);
CREATE INDEX idx_memory_events_repo_created ON memory_events(repo, created_at);
CREATE INDEX idx_memory_events_source ON memory_events(source);
```

### Source Enum

| Source | Description | Example |
|--------|------------|---------|
| `repo_analysis` | Initial or periodic repo analysis | README parsing, agent exploration |
| `investigation` | Learned during issue reproduction | Discovered working commands |
| `pr_review` | Learned during PR review | Architecture insight from diff |
| `deployment` | Learned during sandbox deployment | Working deploy option |
| `outcome_correction` | Auto-corrected from outcome tracking | Verdict was wrong |
| `staleness_correction` | Auto-corrected from staleness detection | Dep removed from requirements.txt |
| `user_correction` | Manually corrected by a human via dashboard | User clicked "Undo" or edited |
| `cross_reference` | Derived from linking investigations | "This file was flagged in issue #10" |

### Confidence Levels

| Level | Meaning | When Applied |
|-------|---------|-------------|
| `low` | Single data point, unverified | First README analysis |
| `medium` | Multiple data points or reasonable inference | Agent analysis cross-referenced with README |
| `high` | Verified by execution | `working_install_command` (health check passed) |
| `verified` | Confirmed by real-world outcome | Verdict confirmed by fix PR merge |

### Event Lifecycle

```
                        ┌─ superseded_by → Event N+1
Event N ───created_at──┤
                        └─ (or remains current)

Current state = latest non-superseded event per (repo, key)
```

When a new event supersedes an old one, the old event's `superseded_by` is set to the new event's ID. This is the only mutation allowed on events — everything else is append-only.

### Materialized View

The existing `repo_memory` table becomes a materialized view. It's updated on every event write. Reads go through the materialized view for performance; history queries go through `memory_events`.

```sql
-- Materialized view (replaces current repo_memory semantics)
-- Same schema as today, but now derived from events
CREATE TABLE repo_memory_current (
    repo TEXT NOT NULL,
    key TEXT NOT NULL,
    value TEXT,
    confidence TEXT DEFAULT 'medium',
    event_id INTEGER REFERENCES memory_events(id),
    updated_at TEXT,
    UNIQUE(repo, key)
);
```

**Why a separate table instead of a view?** SQLite views with `GROUP BY` on large event tables get slow. A maintained denormalization keeps `GetRepoMemory` at O(1) per key, same as today.

---

## 5. Self-Correction Rules

Each rule is a trigger condition paired with a corrective action. Rules are evaluated by the Self-Correction Engine, which runs:
- After every investigation completes
- After every PR review completes
- On a periodic schedule (configurable, default: daily)
- On-demand via dashboard button

### Rule Catalog

#### 5.1 Post-Investigation Corrections

| Trigger | Action | Confidence Impact |
|---------|--------|------------------|
| Health check passed with install command X | Emit event: `working_install_command = X`, confidence `high` | Supersedes `medium` analyzed command |
| Health check failed with memory's `working_install_command` | Emit event: downgrade confidence to `low`, add reason "failed in run {id}" | Forces re-analysis on next run |
| Server started with a command different from memory | Emit event: update `working_run_command`, reason "discovered during run {id}" | New command gets `high` if health check passed |
| Investigation found files that don't match `rich_analysis` architecture | Flag `rich_analysis` for re-analysis (staleness event) | Downgrade to `low` |

#### 5.2 Post-PR-Review Corrections

| Trigger | Action | Confidence Impact |
|---------|--------|------------------|
| PR merged that modifies files referenced in `rich_analysis` | Flag `rich_analysis` for partial update | Downgrade affected sections to `medium` |
| PR merged that modifies `requirements.txt` / `go.mod` / `package.json` | Flag `runtime_requirements` for re-analysis | Downgrade to `low` |
| Maintainer disagreed with review verdict (closed PR we approved, merged PR we rejected) | Emit outcome correction event (see Section 7) | Log disagreement for outcome tracking |

#### 5.3 Staleness Corrections

| Trigger | Action | Threshold |
|---------|--------|-----------|
| `rich_analysis` older than 30 days | Flag for re-analysis on next run | Configurable per key |
| `runtime_requirements` references dep not in latest dep file | Emit correction event removing dep | Checked on repo analysis |
| `working_install_command` older than 14 days without a successful run | Downgrade confidence to `medium` | Configurable |
| `last_verdict` references issue that's now closed | Emit staleness event, mark as historical | Checked on poll |

#### 5.4 Correction Event Format

A correction event looks like any other event, but with a specific source:

```json
{
  "repo": "owner/repo",
  "key": "working_install_command",
  "value": null,
  "previous_value": "pip install -r requirements.txt",
  "reason": "Failed health check in run #847. pip install succeeded but server crashed on import: ModuleNotFoundError 'torch'. Likely missing from requirements.txt or needs GPU.",
  "source": "outcome_correction",
  "source_ref": "run:847",
  "confidence": "low",
  "reversible": true
}
```

Setting `value` to `null` means "I no longer trust this knowledge" without claiming to know the correct answer. The next investigation will re-derive it from scratch.

### Correction Guardrails

**The cascade problem:** A wrong correction can cascade. If OpinAI incorrectly invalidates `working_install_command`, the next run has to re-derive it, potentially getting something even worse.

**Mitigations:**

1. **Never delete, only downgrade.** Corrections lower confidence and add context — they don't erase the value. The old value is still available as a fallback.

2. **Require multiple signals.** A single failed run doesn't invalidate a `high` confidence command. The confidence drops to `medium`. Two consecutive failures drop to `low`. Three trigger a null-out with re-analysis flag.

3. **Human-in-the-loop for `verified` knowledge.** Knowledge at `verified` confidence (confirmed by outcomes) requires user confirmation to invalidate. The Memory Journal shows a warning instead of auto-correcting.

4. **Rate limiting.** No more than 3 auto-corrections per key per 24 hours. If a key is bouncing, flag it for human review.

---

## 6. User Interface — Memory Journal

### Dashboard Page: `/memory` (or `/memory/{repo}`)

The Memory Journal is the transparency layer. Its purpose: let a human understand what OpinAI thinks it knows, why it thinks that, and whether it's right.

### 6.1 Timeline View

```
┌─────────────────────────────────────────────────────────────┐
│  Memory Journal — owner/repo                                 │
│                                                              │
│  ┌─ Today ──────────────────────────────────────────────┐   │
│  │                                                       │   │
│  │  14:23  working_install_command  UPDATED  ▲ high      │   │
│  │         "poetry install" → "poetry install --no-root" │   │
│  │         Reason: health check passed in run #912       │   │
│  │         Source: investigation (issue #87)              │   │
│  │         [Undo] [Details]                              │   │
│  │                                                       │   │
│  │  14:20  runtime_requirements  FLAGGED  ⚠ medium       │   │
│  │         needs_gpu: false — may be stale               │   │
│  │         Reason: issue #87 mentions CUDA errors        │   │
│  │         Source: cross_reference                        │   │
│  │         [Undo] [Investigate] [Dismiss]                │   │
│  │                                                       │   │
│  └───────────────────────────────────────────────────────┘   │
│                                                              │
│  ┌─ Yesterday ──────────────────────────────────────────┐   │
│  │                                                       │   │
│  │  09:15  rich_analysis  UPDATED  ● medium              │   │
│  │         Reason: periodic re-analysis (30-day refresh) │   │
│  │         Changes: +2 new API endpoints, -1 removed     │   │
│  │         Source: repo_analysis                         │   │
│  │         [Undo] [Diff]                                 │   │
│  │                                                       │   │
│  └───────────────────────────────────────────────────────┘   │
│                                                              │
│  [Load older events...]                                      │
└─────────────────────────────────────────────────────────────┘
```

### 6.2 Knowledge State View

A snapshot of what OpinAI currently believes about a repo, with confidence indicators:

```
┌──────────────────────────────────────────────────────────────┐
│  Current Knowledge — owner/repo                               │
│                                                               │
│  Identity                                          Confidence │
│  ├─ description: "FastAPI web service for..."      ● medium   │
│  ├─ tech_stack: "Python, FastAPI, SQLAlchemy"      ● medium   │
│  └─ deployment_type: "docker"                      ▲ high     │
│                                                               │
│  Commands                                                     │
│  ├─ install: "poetry install --no-root"            ▲ high     │
│  │  └─ (verified in run #912, 2 hours ago)                    │
│  ├─ run: "uvicorn main:app --port 8000"            ▲ high     │
│  └─ health: "http://localhost:8000/health"         ● medium   │
│                                                               │
│  Runtime                                                      │
│  ├─ language: "python"                             ● medium   │
│  ├─ needs_gpu: false                               ⚠ flagged  │
│  │  └─ (cross-ref: issue #87 mentions CUDA)                   │
│  └─ infra_deps: ["postgresql"]                     ● medium   │
│                                                               │
│  Track Record                                                 │
│  ├─ Verdicts: 8 correct / 2 wrong (80%)                       │
│  ├─ PR Reviews: 5 correct / 1 wrong (83%)                     │
│  └─ Reproductions: 12 successful / 3 failed (80%)            │
│                                                               │
│  [Export JSON] [Re-analyze Now] [Clear Stale]                 │
└──────────────────────────────────────────────────────────────┘
```

### 6.3 Diff View

When clicking "Diff" on any event, show a side-by-side comparison:

```
┌──────────────────────┬──────────────────────┐
│  Before (run #847)   │  After (run #912)    │
├──────────────────────┼──────────────────────┤
│  pip install -r      │  poetry install      │
│  requirements.txt    │  --no-root           │
│                      │                      │
│  Confidence: medium  │  Confidence: high    │
│  Source: analysis    │  Source: investigation│
└──────────────────────┴──────────────────────┘
```

For `rich_analysis` (large JSON), show a structured diff highlighting added/removed/changed fields rather than raw text diff.

### 6.4 Undo

Clicking "Undo" on any event:
1. Creates a new event with `source: user_correction` and `reason: "Manually reverted by user"`
2. Restores the previous value from the superseded event
3. Sets confidence based on the restored event's confidence
4. Does NOT delete the bad event — it remains in history for learning

### 6.5 "Since Last Visit" Summary

When a user opens the Memory Journal, show a collapsible banner:

```
Since your last visit (3 days ago):
  • 4 memory updates across 2 repos
  • 1 auto-correction (install command failed, downgraded)
  • 1 staleness flag (rich_analysis for owner/repo, 32 days old)
  • Track record: 3/3 verdicts confirmed correct
```

### 6.6 API Endpoints

| Endpoint | Method | Description |
|----------|--------|-------------|
| `/api/memory/{repo}/events` | GET | Paginated event history. Query params: `key`, `source`, `since`, `limit` |
| `/api/memory/{repo}/current` | GET | Current materialized state with confidence |
| `/api/memory/{repo}/events/{id}/undo` | POST | Revert a specific event |
| `/api/memory/{repo}/stats` | GET | Track record and staleness summary |
| `/api/memory/{repo}/export` | GET | Full export (JSON or YAML, query param `format`) |
| `/api/memory/{repo}/re-analyze` | POST | Trigger re-analysis and emit new events |

---

## 7. Outcome Tracking — RL-Inspired

### The Idea

OpinAI makes predictions: "this is a bug," "this PR is safe," "this install command works." These predictions have real-world outcomes. By tracking the correlation between predictions and outcomes, OpinAI can build a track record — and adjust its confidence accordingly.

This is **not** reinforcement learning in the ML sense. There's no gradient descent or policy optimization. It's closer to a calibration system: "How often is OpinAI right, and in what contexts?"

### Outcome Table

```sql
CREATE TABLE outcomes (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo TEXT NOT NULL,
    prediction_type TEXT NOT NULL,   -- verdict | pr_review | reproduction
    prediction_value TEXT NOT NULL,  -- BUG_CONFIRMED | APPROVE | success
    prediction_ref TEXT NOT NULL,    -- run:847 | pr_review:23
    prediction_confidence TEXT,      -- HIGH | MEDIUM | LOW
    outcome_value TEXT,              -- confirmed | refuted | unknown
    outcome_ref TEXT,                -- issue:42:closed | pr:15:reverted
    outcome_detected_at TEXT,
    created_at TEXT DEFAULT (datetime('now'))
);

CREATE INDEX idx_outcomes_repo ON outcomes(repo);
CREATE INDEX idx_outcomes_type ON outcomes(prediction_type);
```

### Outcome Detection Rules

#### 7.1 Verdict Outcomes

| Prediction | Outcome Signal | Result |
|-----------|---------------|--------|
| BUG_CONFIRMED | Issue closed + fix PR merged | **confirmed** |
| BUG_CONFIRMED | Issue closed as "not planned" / "won't fix" | **refuted** (maybe not a real bug) |
| BUG_CONFIRMED | Issue closed by author with "user error" / "my mistake" | **refuted** |
| BUG_CONFIRMED | Issue still open after 90 days | **unknown** (no signal) |
| NOT_A_BUG | Issue reopened or new related issue filed | **refuted** |
| NOT_A_BUG | Issue stays closed | **confirmed** |
| NEEDS_MORE_INFO | Author provides info + issue resolved | **confirmed** (correct to ask) |

**Detection mechanism:** The poller already watches repos. Extend it to check the status of issues OpinAI has investigated. When an issue's state changes (closed, labeled, linked to a PR), evaluate against the verdict.

#### 7.2 PR Review Outcomes

| Prediction | Outcome Signal | Result |
|-----------|---------------|--------|
| APPROVE | PR merged, no revert within 7 days | **confirmed** |
| APPROVE | PR merged, then reverted | **refuted** |
| APPROVE | PR closed without merge | **ambiguous** (could be unrelated) |
| CHANGES_REQUESTED | Author pushes new commits addressing feedback | **confirmed** (author agreed) |
| CHANGES_REQUESTED | PR merged without changes | **refuted** (maintainer disagreed) |
| CHANGES_REQUESTED | PR closed | **ambiguous** |

**Detection mechanism:** Poll PR status for reviews OpinAI submitted. Check for revert commits (commit message contains "revert" + original PR number or title).

#### 7.3 Reproduction Outcomes

| Prediction | Outcome Signal | Result |
|-----------|---------------|--------|
| Reproduction succeeded | Fix PR references this issue | **confirmed** (bug was real and reproducible) |
| Reproduction failed | Issue closed as duplicate of known bug | **expected** (was real, just couldn't reproduce) |
| Reproduction failed | Issue closed as invalid | **confirmed** (correct to not reproduce) |

### Track Record Computation

```
accuracy(repo, type) = confirmed / (confirmed + refuted)
accuracy(repo, type, confidence) = per-confidence-level breakdown
overall_accuracy(type) = across all repos
```

**How this feeds back into the system:**

1. **Confidence calibration.** If OpinAI's HIGH confidence verdicts are only right 60% of the time for a specific repo, auto-downgrade future HIGH to MEDIUM for that repo and log the reason.

2. **Prompt adjustment.** The track record summary is included in the AI prompt context: "Your verdict accuracy for this repo is 80% (8/10). Your last two verdicts were wrong — you said BUG_CONFIRMED but both were user error. Be more skeptical of user-reported bugs in this repo."

3. **Dashboard visibility.** Track record is prominently displayed per repo and overall, so users can gauge trust level.

### Honest Assessment

This is the most speculative part of the design. The risk is that outcome signals are noisy:

- An issue closed without a fix PR might mean "maintainer deprioritized" not "not a real bug"
- A PR merged without addressing review comments might mean "comments were about style, not substance"
- A reverted PR might have been reverted for reasons unrelated to what OpinAI reviewed

**Mitigation:** Use a three-value outcome (confirmed/refuted/unknown) and only count confirmed+refuted for accuracy. When the signal is ambiguous, record it as `unknown` and exclude from accuracy calculations. Over time, the ratio of confirmed-to-refuted matters more than individual data points.

**Risk:** Small sample sizes. For a repo with 3 investigations, an accuracy of 67% is meaningless. Require a minimum of 5 outcomes before computing accuracy, and display "insufficient data" below that threshold.

---

## 8. Cross-Investigation Learning

### The Problem

OpinAI's investigations produce deep knowledge about specific code paths, but this knowledge is siloed in run reports. When a PR touches code that was previously investigated, OpinAI starts from scratch.

### Knowledge Graph (Lightweight)

Not a full graph database — that's overkill. Instead, maintain a mapping of **files → investigation findings**:

```sql
CREATE TABLE investigation_findings (
    id INTEGER PRIMARY KEY AUTOINCREMENT,
    repo TEXT NOT NULL,
    file_path TEXT NOT NULL,
    finding_type TEXT NOT NULL,      -- root_cause | symptom | workaround | architecture
    finding TEXT NOT NULL,
    issue_number INTEGER,
    run_id INTEGER,
    confidence TEXT DEFAULT 'medium',
    created_at TEXT DEFAULT (datetime('now')),
    invalidated_at TEXT              -- set when finding is superseded
);

CREATE INDEX idx_findings_repo_file ON investigation_findings(repo, file_path);
CREATE INDEX idx_findings_repo_issue ON investigation_findings(repo, issue_number);
```

### Population

When an investigation completes, the AI's report is parsed for:

1. **Root causes** — "The bug is in `middleware.go:145` where `Flush()` is never called"
2. **Symptoms** — "This manifests as SSE connections hanging after 30 seconds"
3. **Workarounds** — "Setting `Transfer-Encoding: chunked` bypasses the buffering"
4. **Architecture insights** — "The dashboard uses a middleware chain: CORS → RateLimit → Auth → Handler"

The AI already produces structured output. Adding a `findings` section to the investigation prompt is low-effort.

### Consumption

When a PR review starts, query `investigation_findings` for any files touched by the PR:

```sql
SELECT * FROM investigation_findings
WHERE repo = ? AND file_path IN (?, ?, ...)
AND invalidated_at IS NULL
ORDER BY created_at DESC;
```

Inject matching findings into the PR review prompt:

```
## Prior Investigation Context

The following files in this PR were previously investigated:

- `middleware.go` — ROOT CAUSE of issue #10 (streaming bug): Flush() is never called
  after writing SSE events, causing responses to buffer until the connection
  times out. Identified in run #847 (2026-03-15). Confidence: high.
```

This turns OpinAI's institutional memory into an active review tool. The AI reviewer isn't just seeing the diff — it's seeing the diff in context of everything OpinAI has ever learned about those files.

### Staleness of Findings

Findings become stale when:
- The referenced file is deleted or substantially rewritten (>50% of lines changed)
- The referenced issue is closed with a fix PR that modifies the same file
- A newer investigation produces a contradictory finding for the same file

When a finding is invalidated, set `invalidated_at` and emit a memory event explaining why.

### Scaling Concern

For repos with hundreds of investigations, the findings table could grow large. Mitigation:
- Only store findings for files with actionable insights (root causes, not "this file exists")
- Limit to 10 findings per file (oldest non-invalidated findings get archived)
- Index on (repo, file_path) makes queries fast regardless of table size

---

## 9. Portability — Ambient Integration

### Design Goal

The memory system should be useful beyond OpinAI. Jeremy Eder's Ambient Code project wants AI agents that build up institutional knowledge about codebases. If OpinAI's memory format becomes a portable standard, any agent can read/write it.

### Export Format

Two export modes:

#### 9.1 Current State Export (Snapshot)

```yaml
# .ai-memory/state.yaml
schema_version: "1.0"
repo: "owner/repo"
exported_at: "2026-04-09T14:30:00Z"
exported_by: "opinai-controller/0.1.0"

knowledge:
  identity:
    description:
      value: "FastAPI web service for data processing"
      confidence: medium
      last_updated: "2026-04-01T10:00:00Z"
      source: repo_analysis

    tech_stack:
      value: "Python, FastAPI, SQLAlchemy, PostgreSQL"
      confidence: medium
      last_updated: "2026-04-01T10:00:00Z"
      source: repo_analysis

  commands:
    install:
      value: "poetry install --no-root"
      confidence: high
      last_updated: "2026-04-09T14:23:00Z"
      source: investigation
      source_ref: "run:912"

    run:
      value: "uvicorn main:app --port 8000"
      confidence: high
      last_updated: "2026-04-09T14:23:00Z"
      source: investigation

  runtime:
    language: "python"
    needs_gpu: false
    needs_glibc: false
    infra_deps:
      - "postgresql"

  findings:
    - file: "middleware.go"
      type: root_cause
      summary: "Flush() never called after SSE writes, causing buffering"
      issue: 10
      confidence: high
      discovered: "2026-03-15T09:00:00Z"

track_record:
  verdicts: { correct: 8, wrong: 2, unknown: 3 }
  pr_reviews: { correct: 5, wrong: 1, unknown: 2 }
```

#### 9.2 Event History Export (Full Ledger)

```yaml
# .ai-memory/history.yaml
schema_version: "1.0"
repo: "owner/repo"

events:
  - id: 1
    key: "install_command"
    value: "pip install -r requirements.txt"
    reason: "Parsed from README.md"
    source: repo_analysis
    confidence: medium
    timestamp: "2026-03-01T10:00:00Z"

  - id: 2
    key: "install_command"
    value: "poetry install --no-root"
    previous_value: "pip install -r requirements.txt"
    reason: "pip install failed in run #847, poetry detected in pyproject.toml"
    source: investigation
    source_ref: "run:847"
    confidence: high
    timestamp: "2026-04-09T14:23:00Z"
    supersedes: 1
```

### Agent-Agnostic Design Principles

1. **No OpinAI-specific fields.** The schema uses generic terms: `source`, `confidence`, `finding`. Any agent can populate these.

2. **No runtime coupling.** The export is a file. No API calls required to consume it. An agent reads `.ai-memory/state.yaml` from the repo (or from a shared location) and has full context.

3. **Namespace for agent-specific data.** If OpinAI needs to store something only OpinAI understands, it goes under an `extensions` key:

```yaml
knowledge:
  extensions:
    opinai:
      deployment_plan_id: "plan-847"
      sandbox_namespace: "sandbox-owner-repo-abc123"
```

4. **Mergeable.** If two agents both produce memory for the same repo, their exports can be merged. Conflicts are resolved by timestamp (latest wins) or confidence (highest wins), with the merge itself logged as an event.

### Ambient Integration Path

1. **Phase 1:** OpinAI exports to `.ai-memory/` in the repo (or a configurable path). Ambient agents can read it.

2. **Phase 2:** Ambient agents write their findings in the same format. OpinAI imports them on next analysis.

3. **Phase 3:** Shared memory server (optional). Agents register as producers/consumers. Real-time sync via webhooks or polling.

Phase 3 is speculative and should only be built if Phase 1-2 adoption proves the format works.

### File Placement

**Option A: In-repo (`.ai-memory/` directory).** Pro: versioned with the code, visible in PRs. Con: clutters the repo, may contain sensitive investigation details.

**Option B: External (configurable path or cloud storage).** Pro: clean repo, centralized. Con: requires configuration, not automatically shared.

**Recommendation:** Default to external storage (OpinAI's data directory), with an explicit export command for sharing. Don't commit AI memory to repos without the maintainer opting in.

---

## 10. Risks and Mitigations

### 10.1 Auto-Correction Cascade

**Risk:** OpinAI makes a wrong correction, which cascades into further wrong corrections. Example: a flaky test makes OpinAI think `working_install_command` is broken → it nulls it out → next run falls back to README analysis → gets a worse command → that also fails → confidence drops globally.

**Mitigation:**
- **Cooling period.** After an auto-correction, don't allow another auto-correction on the same key for 1 hour. Let the next run verify with fresh data.
- **Fallback chain preservation.** Never delete superseded values. The materialized view shows the latest, but the event history preserves all alternatives. If the latest value fails, the correction engine can propose falling back to the previous value rather than starting from scratch.
- **Circuit breaker.** If a key has been corrected more than 3 times in 24 hours, stop auto-correcting and flag it for human review. Display a prominent warning in the Memory Journal.
- **Dry-run mode.** Initially, corrections are proposed but not applied. The Memory Journal shows "Proposed correction" and a human approves. Once calibrated, auto-apply can be enabled per key type.

### 10.2 Memory Bloat

**Risk:** Every event stored forever means the `memory_events` table grows unboundedly. A repo with frequent re-analyses could generate thousands of events per year.

**Mitigation:**
- **Compaction.** Events older than 90 days where the key has been superseded 3+ times since: compress the intermediate events into a single summary event. Keep the first and last event in each chain; discard the middle.
- **Archival.** Events older than 1 year: export to a JSON file in the data directory, delete from SQLite. The Memory Journal shows "Archived events available for download" with a link.
- **Bounded per key.** Maximum 50 events per (repo, key) pair. When limit is hit, compact the oldest 25 into a summary.
- **Napkin math.** Each event is ~500 bytes. 100 events/month × 10 repos × 12 months = 12,000 events = ~6MB/year. This is not a real problem for SQLite. Compaction is a nice-to-have, not urgent.

### 10.3 Query Performance

**Risk:** Event sourcing means "get current state" requires scanning all events for a key. Naive implementation is O(events) per read.

**Mitigation:** The materialized view (`repo_memory_current`) is the primary read path. It's updated synchronously on every event write. Cost is O(1) for reads, O(1) extra for writes (upsert into materialized view). The event table is only queried for history views (paginated, indexed). This is a solved problem.

### 10.4 Over-Confidence in Self-Knowledge

**Risk:** OpinAI trusts its memory too much. If memory says `needs_gpu: false`, it might skip GPU-related investigation even when the user explicitly mentions CUDA errors.

**Mitigation:**
- **Memory is context, not constraint.** The AI prompt should frame memory as "previous findings" not "ground truth." Add a system instruction: "Your memory may be outdated. If the current evidence contradicts your memory, trust the evidence and flag the memory as potentially stale."
- **Confidence decay.** Memory confidence decreases over time. A `high` confidence finding from 60 days ago auto-degrades to `medium`. This is implemented in the materialized view (computed column based on `updated_at` and `confidence`).
- **User-visible confidence.** The Memory Journal shows confidence levels prominently. Users can see at a glance which knowledge is trustworthy and which is stale.

### 10.5 Privacy and Sensitivity

**Risk:** Memory contains code snippets, error messages, file paths, and investigation details. Exporting memory could leak sensitive information.

**Mitigation:**
- **Export is opt-in.** No automatic sharing. Users must explicitly export.
- **Redaction layer.** Export can filter out sensitive keys (API keys found in env, credentials in error messages). Implement a basic regex scan for common secret patterns before export.
- **Scope limitation.** Memory only stores repo-level knowledge derived from public repositories (since OpinAI monitors public GitHub repos). For private repos, memory export should require additional confirmation.

### 10.6 Complexity vs. Value

**Risk:** This entire system might be over-engineered. The current flat key-value store works. Adding event sourcing, outcome tracking, cross-referencing, and a new UI is a lot of machinery. If OpinAI only monitors 5-10 repos, the sophisticated learning systems may never accumulate enough data to be useful.

**Mitigation:** This is a real risk, and the implementation phases (Section 11) are designed to address it. MVP is just the event ledger + basic Memory Journal. Outcome tracking and cross-investigation learning are Phase 2-3, only built if Phase 1 proves valuable. The design should be read as a north star, not a sprint commitment. If the event ledger alone gives 80% of the value, stop there.

---

## 11. Implementation Phases

### Phase 1: Event Ledger + Memory Journal (MVP)

**Goal:** Every memory change is recorded with reasoning. Users can see what changed and undo mistakes.

**Database changes:**
- Add `memory_events` table
- Add `repo_memory_current` table (materialized view with confidence)
- Migrate existing `repo_memory` data: create initial events from current values, populate `repo_memory_current`
- Keep old `repo_memory` table as alias (backward compat)

**Code changes:**
- `SetRepoMemory(repo, key, value)` → `SetRepoMemory(repo, key, value, reason, source)`. Old signature wraps new one with `reason: "legacy set"`, `source: "unknown"`.
- Every existing `SetRepoMemory` call site gets a meaningful reason and source. There are ~15 call sites — straightforward but tedious.
- `GetRepoMemory` reads from `repo_memory_current` (same behavior, different table).
- New function: `GetMemoryHistory(repo, key, limit, offset)` for timeline queries.

**Dashboard:**
- New `/memory` page with timeline view and current knowledge state.
- Undo button per event.
- "Since last visit" banner (using a simple `last_visit` cookie or DB record).

**Estimated scope:** ~800-1200 lines of Go (database + API), ~400-600 lines of frontend (HTML/JS for journal page). 

### Phase 2: Self-Correction + Staleness Detection

**Goal:** Memory fixes itself when it detects problems.

**Code changes:**
- Staleness detector: background goroutine, runs daily, checks `updated_at` against configured thresholds.
- Post-investigation hook: after a run completes, evaluate correction rules against memory.
- Post-PR-review hook: after a PR status changes, check for disagreement signals.
- Confidence decay: computed on read from `repo_memory_current`, based on age and confidence level.
- Circuit breaker: rate-limit auto-corrections per key.

**Dashboard:**
- Staleness flags in the Knowledge State view.
- "Proposed corrections" queue (initially requiring human approval).
- Correction history in timeline.

**Prerequisite:** Phase 1 deployed and validated. Need real event data to tune correction rules.

### Phase 3: Outcome Tracking

**Goal:** Track whether OpinAI's predictions were right.

**Database changes:**
- Add `outcomes` table.

**Code changes:**
- Outcome detector: extend poller to check issue/PR status changes for repos where OpinAI has made predictions.
- Track record computation: aggregate outcomes per repo, per type, per confidence level.
- Prompt injection: include track record summary in investigation and review prompts.

**Dashboard:**
- Track record display per repo and overall.
- Outcome timeline (separate tab or integrated with memory timeline).

**Prerequisite:** Phase 2 deployed. Need self-correction infrastructure to act on outcome data.

### Phase 4: Cross-Investigation Learning

**Goal:** Findings from investigations inform PR reviews and future investigations.

**Database changes:**
- Add `investigation_findings` table.

**Code changes:**
- Finding extraction: add `findings` section to investigation prompt output schema.
- Finding injection: query findings for PR-touched files, inject into review prompt.
- Finding invalidation: detect when findings are stale (file deleted, issue fixed).

**Dashboard:**
- Findings view per repo: which files have findings, what they say.
- Finding references in PR review display.

**Prerequisite:** Phase 3 deployed. Need outcome data to validate whether cross-referencing actually improves review quality.

### Phase 5: Portability + Export

**Goal:** Memory is exportable and consumable by other agents.

**Code changes:**
- Export API: JSON and YAML export of current state and event history.
- Import API: read `.ai-memory/state.yaml` and merge into event ledger.
- Schema validation: ensure exports conform to spec.

**Integration:**
- Ambient agent import support (coordinated with Ambient team).
- Shared format specification (RFC-style document).

**Prerequisite:** Phase 1-4 stable. The export format is defined by what's actually useful, not what we imagine might be useful.

---

## 12. Open Questions

### Architecture

1. **Event sourcing vs. change-data-capture?** Full event sourcing (derive state from events) vs. keeping the current table and adding a CDC audit log. Event sourcing is cleaner but requires more upfront work. CDC is pragmatic but can drift. This design proposes event sourcing with a materialized view, which is a hybrid — is that the right tradeoff?

2. **SQLite scaling.** OpinAI currently uses a single SQLite file with WAL mode and a write mutex. Adding event tables increases write volume. At what point does this need a migration to PostgreSQL or a separate SQLite file for events? (Napkin math suggests years of headroom, but worth monitoring.)

3. **Async vs. sync corrections.** Should corrections be applied synchronously (blocking the run that triggered them) or asynchronously (queued for background processing)? Sync is simpler but could slow down investigations. Async risks the next run using stale data. Proposed answer: corrections are synchronous but fast (single DB write), with complex corrections (re-analysis) queued as async tasks.

### Product

4. **Who is the user?** The Memory Journal assumes someone regularly checks the dashboard. For repos OpinAI monitors autonomously, nobody may look at the journal for weeks. Should there be email/Slack notifications for important memory changes? Or is passive display sufficient?

5. **How much transparency is too much?** Showing every memory event could overwhelm users. Should there be a "summary mode" that collapses routine events and only surfaces anomalies?

6. **Should users be able to teach OpinAI?** Beyond the Undo button, should users be able to manually create memory events? "I know this repo needs GPU — set needs_gpu to true with confidence verified." This is powerful but risks human errors propagating with high confidence.

### Ambient Integration

7. **Who owns the schema?** If this becomes a shared standard, it needs governance. Is OpinAI the reference implementation, or should Ambient define the schema and OpinAI conform to it? (Recommendation: OpinAI iterates fast in Phase 1-4, then proposes a schema to Ambient based on what actually works.)

8. **Merge conflicts.** Two agents produce contradictory findings for the same file. Who wins? Timestamp-based (latest wins) is simple but could overwrite better-quality findings with newer-but-worse ones. Confidence-based (highest wins) requires agents to calibrate confidence consistently, which they won't.

9. **Memory as a repo artifact?** Committing `.ai-memory/` to a repo is a strong signal of institutional knowledge. But it's also noisy in diffs, potentially sensitive, and adds AI-generated content to the git history. Should this be opt-in, and if so, what's the default?

### Technical

10. **Compaction strategy.** When compacting old events into summaries, how much detail to preserve? A summary event like "install_command changed 7 times between 2026-03 and 2026-06, settling on 'poetry install'" loses the intermediate debugging context but keeps the narrative. Is that enough?

11. **Finding extraction reliability.** The cross-investigation learning system depends on the AI reliably extracting structured findings from investigation reports. If extraction is inconsistent, the findings table fills with noise. Should there be a validation step (second AI call to verify findings) or is that too expensive?

12. **Privacy-preserving export.** For open-source repos, memory export is fine. For private repos, the memory might contain proprietary code patterns, internal API endpoints, or infrastructure details. Should private-repo memory be encrypted at rest, or is access control sufficient?
