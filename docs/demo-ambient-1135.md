# OpinAI + Amber: Better Together

## Case Study: Issue #1135 — GITHUB_TOKEN not updated after credential refresh

### The Problem
When `refresh_credentials` is called mid-session, the git credential helper gets a fresh token, but `gh` CLI keeps using the stale `GITHUB_TOKEN` from the subprocess's frozen environment. All `gh` commands fail with 401.

---

### What Amber Did (without OpinAI)
- ⏱️ Auto-created PR #1137 within 15 minutes
- Generated 228 lines: a `gh` wrapper script at `/tmp/.ambient-bin/gh` that reads the token from a file before calling the real binary
- **No investigation report. No root cause analysis. Jumped straight to code.**

### What OpinAI Did
- ⏱️ 3-minute investigation, 56 iterations, 58 tool calls
- **Read 24 source files** across runner, bridge, operator, auth, and tests
- Traced the full credential lifecycle:
  1. `auth.py` → `populate_runtime_credentials` updates `os.environ["GITHUB_TOKEN"]` ✓
  2. `auth.py` → writes fresh token to `/tmp/.ambient_github_token` ✓
  3. `bridge.py` → CLI subprocess spawned once, env frozen at spawn time ✗
  4. `gh` CLI prioritizes `GITHUB_TOKEN` env var over credential helpers ✗
- Found the **root cause**: `auth.py` line 580 explicitly documents this limitation:
  > *"The helper runs inside the CLI subprocess's environment (which is fixed at spawn time), so updating os.environ mid-run would not reach it without these files."*
- Produced a plain-language summary ("The Dude's Take") explaining the full architecture

---

### The Key Difference

When we fed OpinAI's analysis back to an AI and asked "given this understanding, evaluate Amber's fix" — it identified critical gaps:

| Edge Case | Amber's Wrapper Handles? |
|-----------|------------------------|
| Token rotation during a long-running session | ❌ Wrapper only re-reads on launch |
| Multiple concurrent bridges sharing rotated creds | ❌ Each has its own stale snapshot |
| Signal propagation (SIGTERM/SIGKILL) | ⚠️ Extra PID layer = fragile |
| Non-env-var config (mounted secrets, API keys) | ❌ Only fixes env var path |

**Verdict:** The wrapper moves the freeze point one layer out, but doesn't fix the fundamental issue. For any session lasting longer than the credential TTL, the same bug returns.

### Better Solution (proposed with OpinAI context)

**Env Provider Pattern** — replace the static env snapshot with a lazy-evaluated provider:

```python
class DynamicEnvProvider:
    """Re-evaluates env on every call."""
    def get_env(self) -> dict[str, str]:
        env = os.environ.copy()  # Always fresh
        # Read dynamic sources (token files, rotated creds)
        for source in self._dynamic_sources:
            env.update(source())
        return env
```

The bridge queries the provider at well-defined lifecycle points (before each `gh` call, before MCP tool execution) instead of inheriting a frozen snapshot.

---

### The Takeaway

| | Amber alone | Amber + OpinAI |
|---|---|---|
| Time to fix | 15 min | Same |
| Root cause understood | ❌ | ✅ 24 files traced |
| Fix quality | Band-aid (wrapper) | Architectural (env provider) |
| Edge cases identified | 0 | 6 |
| Confidence in fix | Unknown | High — verified against source |

**OpinAI doesn't replace Amber — it makes Amber smarter.** The investigation context turns a quick patch into a proper architectural fix.

---

*"That's just, like, your opinion, man." — OpinAI*
