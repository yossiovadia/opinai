"""OpinAI SQLite persistence layer — stores runs, processed issues, and repo memory."""

import json
import logging
import os
import sqlite3
import threading

log = logging.getLogger("opinai-db")

DB_PATH = os.environ.get("OPINAI_DB_PATH", "/data/opinai.db")
_local = threading.local()
_lock = threading.Lock()


def _get_conn() -> sqlite3.Connection:
    """Get a thread-local SQLite connection."""
    if not hasattr(_local, "conn") or _local.conn is None:
        _local.conn = sqlite3.connect(DB_PATH, check_same_thread=False)
        _local.conn.row_factory = sqlite3.Row
        _local.conn.execute("PRAGMA journal_mode=WAL")
        _local.conn.execute("PRAGMA busy_timeout=5000")
    return _local.conn


def init_db():
    """Create tables if they don't exist."""
    os.makedirs(os.path.dirname(DB_PATH) or ".", exist_ok=True)
    with _lock:
        conn = _get_conn()
        conn.executescript("""
            CREATE TABLE IF NOT EXISTS runs (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                repo TEXT NOT NULL,
                issue INTEGER NOT NULL,
                title TEXT,
                category TEXT,
                verdict TEXT,
                confidence TEXT,
                report TEXT,
                posted BOOLEAN DEFAULT FALSE,
                posted_at TEXT,
                ai_powered BOOLEAN DEFAULT TRUE,
                duration TEXT,
                created_at TEXT DEFAULT (datetime('now')),
                UNIQUE(repo, issue, created_at)
            );

            CREATE TABLE IF NOT EXISTS repo_memory (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                repo TEXT NOT NULL,
                key TEXT NOT NULL,
                value TEXT,
                updated_at TEXT DEFAULT (datetime('now')),
                UNIQUE(repo, key)
            );

            CREATE TABLE IF NOT EXISTS processed_issues (
                repo TEXT NOT NULL,
                issue INTEGER NOT NULL,
                processed_at TEXT DEFAULT (datetime('now')),
                job_name TEXT,
                PRIMARY KEY(repo, issue)
            );

            CREATE TABLE IF NOT EXISTS deployment_plans (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                repo TEXT NOT NULL UNIQUE,
                plan_json TEXT NOT NULL,
                status TEXT DEFAULT 'analyzed',
                created_at TEXT DEFAULT (datetime('now')),
                updated_at TEXT DEFAULT (datetime('now'))
            );

            CREATE INDEX IF NOT EXISTS idx_runs_repo ON runs(repo);
            CREATE INDEX IF NOT EXISTS idx_runs_repo_issue ON runs(repo, issue);
        """)
        conn.commit()
    log.info("Database initialized at %s", DB_PATH)


def add_run(run: dict) -> int:
    """Insert a run and return its id."""
    with _lock:
        conn = _get_conn()
        cur = conn.execute(
            """INSERT OR REPLACE INTO runs
               (repo, issue, title, category, verdict, confidence, report,
                posted, ai_powered, duration, created_at)
               VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)""",
            (
                run.get("repo", ""),
                int(run.get("issue", 0)),
                run.get("title", ""),
                run.get("category", ""),
                run.get("verdict", ""),
                run.get("confidence", ""),
                run.get("report", ""),
                bool(run.get("posted", False)),
                bool(run.get("ai", True)),
                run.get("duration", ""),
                run.get("timestamp", ""),
            ),
        )
        conn.commit()
        return cur.lastrowid


def get_runs(repo: str | None = None, limit: int = 50) -> list[dict]:
    """List runs, optionally filtered by repo, newest first."""
    with _lock:
        conn = _get_conn()
        if repo:
            rows = conn.execute(
                "SELECT * FROM runs WHERE repo = ? ORDER BY created_at DESC LIMIT ?",
                (repo, limit),
            ).fetchall()
        else:
            rows = conn.execute(
                "SELECT * FROM runs ORDER BY created_at DESC LIMIT ?",
                (limit,),
            ).fetchall()
    return [_row_to_dict(r) for r in rows]


def get_run(run_id: int) -> dict | None:
    """Get a single run by id."""
    with _lock:
        conn = _get_conn()
        row = conn.execute("SELECT * FROM runs WHERE id = ?", (run_id,)).fetchone()
    return _row_to_dict(row) if row else None


def mark_posted(run_id: int):
    """Set posted=True and posted_at for a run."""
    with _lock:
        conn = _get_conn()
        conn.execute(
            "UPDATE runs SET posted = TRUE, posted_at = datetime('now') WHERE id = ?",
            (run_id,),
        )
        conn.commit()


def is_processed(repo: str, issue: int) -> bool:
    """Check if an issue has already been processed."""
    with _lock:
        conn = _get_conn()
        row = conn.execute(
            "SELECT 1 FROM processed_issues WHERE repo = ? AND issue = ?",
            (repo, issue),
        ).fetchone()
    return row is not None


def mark_processed(repo: str, issue: int, job_name: str = ""):
    """Mark an issue as processed."""
    with _lock:
        conn = _get_conn()
        conn.execute(
            "INSERT OR REPLACE INTO processed_issues (repo, issue, job_name) VALUES (?, ?, ?)",
            (repo, issue, job_name),
        )
        conn.commit()


def set_repo_memory(repo: str, key: str, value: str):
    """Upsert a repo memory entry."""
    with _lock:
        conn = _get_conn()
        conn.execute(
            """INSERT INTO repo_memory (repo, key, value, updated_at)
               VALUES (?, ?, ?, datetime('now'))
               ON CONFLICT(repo, key) DO UPDATE SET value = ?, updated_at = datetime('now')""",
            (repo, key, value, value),
        )
        conn.commit()


def get_repo_memory(repo: str, key: str | None = None) -> dict:
    """Get all or specific memory for a repo."""
    with _lock:
        conn = _get_conn()
        if key:
            row = conn.execute(
                "SELECT key, value FROM repo_memory WHERE repo = ? AND key = ?",
                (repo, key),
            ).fetchone()
            return {row["key"]: row["value"]} if row else {}
        rows = conn.execute(
            "SELECT key, value FROM repo_memory WHERE repo = ?", (repo,)
        ).fetchall()
    return {r["key"]: r["value"] for r in rows}


def get_stats(repo: str) -> dict:
    """Return counts for a repo."""
    with _lock:
        conn = _get_conn()
        processed = conn.execute(
            "SELECT COUNT(*) FROM processed_issues WHERE repo = ?", (repo,)
        ).fetchone()[0]
        total_runs = conn.execute(
            "SELECT COUNT(*) FROM runs WHERE repo = ?", (repo,)
        ).fetchone()[0]
        bugs = conn.execute(
            "SELECT COUNT(*) FROM runs WHERE repo = ? AND verdict = 'BUG_CONFIRMED'",
            (repo,),
        ).fetchone()[0]
        features = conn.execute(
            "SELECT COUNT(*) FROM runs WHERE repo = ? AND verdict = 'FEATURE_REQUEST'",
            (repo,),
        ).fetchone()[0]
    return {
        "processed": processed,
        "total_runs": total_runs,
        "bugs": bugs,
        "features": features,
    }


def get_total_stats() -> dict:
    """Return global counts."""
    with _lock:
        conn = _get_conn()
        total = conn.execute("SELECT COUNT(*) FROM runs").fetchone()[0]
        processed = conn.execute("SELECT COUNT(*) FROM processed_issues").fetchone()[0]
    return {"total_runs": total, "total_processed": processed}


def save_deployment_plan(repo: str, plan_json: str) -> int:
    """Save or replace a deployment plan for a repo."""
    with _lock:
        conn = _get_conn()
        cur = conn.execute(
            """INSERT INTO deployment_plans (repo, plan_json, status, updated_at)
               VALUES (?, ?, 'analyzed', datetime('now'))
               ON CONFLICT(repo) DO UPDATE SET
                 plan_json = ?, status = 'analyzed', updated_at = datetime('now')""",
            (repo, plan_json, plan_json),
        )
        conn.commit()
        return cur.lastrowid


def get_deployment_plan(repo: str) -> dict | None:
    """Get the deployment plan for a repo."""
    with _lock:
        conn = _get_conn()
        row = conn.execute(
            "SELECT * FROM deployment_plans WHERE repo = ?", (repo,)
        ).fetchone()
    if not row:
        return None
    return {
        "id": row["id"],
        "repo": row["repo"],
        "plan_json": row["plan_json"],
        "status": row["status"],
        "created_at": row["created_at"],
        "updated_at": row["updated_at"],
    }


def update_deployment_plan_status(repo: str, status: str):
    """Update the status of a deployment plan."""
    valid = ("analyzed", "tested", "failed")
    if status not in valid:
        raise ValueError(f"Invalid status: {status}. Must be one of {valid}")
    with _lock:
        conn = _get_conn()
        conn.execute(
            "UPDATE deployment_plans SET status = ?, updated_at = datetime('now') WHERE repo = ?",
            (status, repo),
        )
        conn.commit()


def _row_to_dict(row: sqlite3.Row) -> dict:
    """Convert a sqlite3.Row to a dict with API-compatible keys."""
    return {
        "id": row["id"],
        "repo": row["repo"],
        "issue": row["issue"],
        "title": row["title"],
        "category": row["category"],
        "verdict": row["verdict"],
        "confidence": row["confidence"],
        "report": row["report"],
        "posted": bool(row["posted"]),
        "posted_at": row["posted_at"],
        "ai": bool(row["ai_powered"]),
        "duration": row["duration"],
        "timestamp": row["created_at"],
    }
