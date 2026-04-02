package database

import (
	"database/sql"
	"fmt"
	"log/slog"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

var (
	db   *sql.DB
	mu   sync.Mutex // serialize writes (SQLite is single-writer)
)

// Init opens the SQLite database and creates tables if needed.
func Init(path string) error {
	var err error
	db, err = sql.Open("sqlite", path+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		return fmt.Errorf("open sqlite %s: %w", path, err)
	}
	db.SetMaxOpenConns(4)
	db.SetMaxIdleConns(2)
	db.SetConnMaxLifetime(0)

	if err := migrate(); err != nil {
		return fmt.Errorf("migrate: %w", err)
	}
	slog.Info("database initialized", "path", path)
	return nil
}

// DB returns the underlying *sql.DB for advanced queries.
func DB() *sql.DB { return db }

func migrate() error {
	mu.Lock()
	defer mu.Unlock()

	_, err := db.Exec(`
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
			commit_sha TEXT DEFAULT '',
			created_at TEXT DEFAULT (datetime('now')),
			updated_at TEXT DEFAULT (datetime('now'))
		);

		CREATE INDEX IF NOT EXISTS idx_runs_repo ON runs(repo);
		CREATE INDEX IF NOT EXISTS idx_runs_repo_issue ON runs(repo, issue);
	`)
	if err != nil {
		return err
	}
	// Migration: add commit_sha column if missing (existing DBs)
	db.Exec("ALTER TABLE deployment_plans ADD COLUMN commit_sha TEXT DEFAULT ''")
	return err
}

// --- Runs ---

type Run struct {
	ID         int64   `json:"id"`
	Repo       string  `json:"repo"`
	Issue      int     `json:"issue"`
	Title      string  `json:"title"`
	Category   string  `json:"category"`
	Verdict    string  `json:"verdict"`
	Confidence string  `json:"confidence"`
	Report     string  `json:"report"`
	Posted     bool    `json:"posted"`
	PostedAt   *string `json:"posted_at"`
	AIPowered  bool    `json:"ai"`
	Duration   string  `json:"duration"`
	CreatedAt  string  `json:"timestamp"`
}

func AddRun(r Run) (int64, error) {
	mu.Lock()
	defer mu.Unlock()
	res, err := db.Exec(
		`INSERT OR REPLACE INTO runs
		 (repo, issue, title, category, verdict, confidence, report, posted, ai_powered, duration, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Repo, r.Issue, r.Title, r.Category, r.Verdict, r.Confidence,
		r.Report, r.Posted, r.AIPowered, r.Duration, r.CreatedAt,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func GetRuns(repo string, limit int) ([]Run, error) {
	var rows *sql.Rows
	var err error
	if repo != "" {
		rows, err = db.Query("SELECT * FROM runs WHERE repo = ? ORDER BY created_at DESC LIMIT ?", repo, limit)
	} else {
		rows, err = db.Query("SELECT * FROM runs ORDER BY created_at DESC LIMIT ?", limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

func GetRun(id int64) (*Run, error) {
	row := db.QueryRow("SELECT * FROM runs WHERE id = ?", id)
	return scanRun(row)
}

func MarkPosted(id int64) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("UPDATE runs SET posted = TRUE, posted_at = datetime('now') WHERE id = ?", id)
	return err
}

func scanRuns(rows *sql.Rows) ([]Run, error) {
	var result []Run
	for rows.Next() {
		r, err := scanRunFromRows(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, *r)
	}
	return result, rows.Err()
}

func scanRunFromRows(rows *sql.Rows) (*Run, error) {
	var r Run
	err := rows.Scan(&r.ID, &r.Repo, &r.Issue, &r.Title, &r.Category, &r.Verdict,
		&r.Confidence, &r.Report, &r.Posted, &r.PostedAt, &r.AIPowered, &r.Duration, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanRun(row *sql.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Repo, &r.Issue, &r.Title, &r.Category, &r.Verdict,
		&r.Confidence, &r.Report, &r.Posted, &r.PostedAt, &r.AIPowered, &r.Duration, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// --- Repo Memory ---

func SetRepoMemory(repo, key, value string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec(
		`INSERT INTO repo_memory (repo, key, value, updated_at)
		 VALUES (?, ?, ?, datetime('now'))
		 ON CONFLICT(repo, key) DO UPDATE SET value = ?, updated_at = datetime('now')`,
		repo, key, value, value,
	)
	return err
}

func GetRepoMemory(repo string, key *string) (map[string]string, error) {
	result := make(map[string]string)
	var rows *sql.Rows
	var err error
	if key != nil {
		rows, err = db.Query("SELECT key, value FROM repo_memory WHERE repo = ? AND key = ?", repo, *key)
	} else {
		rows, err = db.Query("SELECT key, value FROM repo_memory WHERE repo = ?", repo)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		result[k] = v
	}
	return result, rows.Err()
}

type RepoStats struct {
	Processed int `json:"processed"`
	TotalRuns int `json:"total_runs"`
	Bugs      int `json:"bugs"`
	Features  int `json:"features"`
}

func GetStats(repo string) (RepoStats, error) {
	var s RepoStats
	db.QueryRow("SELECT COUNT(*) FROM processed_issues WHERE repo = ?", repo).Scan(&s.Processed)
	db.QueryRow("SELECT COUNT(*) FROM runs WHERE repo = ?", repo).Scan(&s.TotalRuns)
	db.QueryRow("SELECT COUNT(*) FROM runs WHERE repo = ? AND verdict = 'BUG_CONFIRMED'", repo).Scan(&s.Bugs)
	db.QueryRow("SELECT COUNT(*) FROM runs WHERE repo = ? AND verdict = 'FEATURE_REQUEST'", repo).Scan(&s.Features)
	return s, nil
}

type TotalStats struct {
	TotalRuns      int `json:"total_runs"`
	TotalProcessed int `json:"total_processed"`
}

func GetTotalStats() (TotalStats, error) {
	var s TotalStats
	db.QueryRow("SELECT COUNT(*) FROM runs").Scan(&s.TotalRuns)
	db.QueryRow("SELECT COUNT(*) FROM processed_issues").Scan(&s.TotalProcessed)
	return s, nil
}

// --- Processed Issues ---

func IsProcessed(repo string, issue int) (bool, error) {
	var n int
	err := db.QueryRow("SELECT 1 FROM processed_issues WHERE repo = ? AND issue = ?", repo, issue).Scan(&n)
	if err == sql.ErrNoRows {
		return false, nil
	}
	return err == nil, err
}

func MarkProcessed(repo string, issue int, jobName string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec(
		"INSERT OR REPLACE INTO processed_issues (repo, issue, job_name) VALUES (?, ?, ?)",
		repo, issue, jobName,
	)
	return err
}

// --- Deployment Plans ---

type DeploymentPlan struct {
	ID        int64  `json:"id"`
	Repo      string `json:"repo"`
	PlanJSON  string `json:"plan_json"`
	Status    string `json:"status"`
	CommitSHA string `json:"commit_sha"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

func SaveDeploymentPlan(repo, planJSON string) (int64, error) {
	mu.Lock()
	defer mu.Unlock()
	res, err := db.Exec(
		`INSERT INTO deployment_plans (repo, plan_json, status, updated_at)
		 VALUES (?, ?, 'analyzed', datetime('now'))
		 ON CONFLICT(repo) DO UPDATE SET plan_json = ?, status = 'analyzed', updated_at = datetime('now')`,
		repo, planJSON, planJSON,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func GetDeploymentPlan(repo string) (*DeploymentPlan, error) {
	var p DeploymentPlan
	err := db.QueryRow("SELECT id, repo, plan_json, status, commit_sha, created_at, updated_at FROM deployment_plans WHERE repo = ?", repo).Scan(
		&p.ID, &p.Repo, &p.PlanJSON, &p.Status, &p.CommitSHA, &p.CreatedAt, &p.UpdatedAt,
	)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func SetDeploymentPlanSHA(repo, sha string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("UPDATE deployment_plans SET commit_sha = ? WHERE repo = ?", sha, repo)
	return err
}

func UpdateDeploymentPlanStatus(repo, status string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec(
		"UPDATE deployment_plans SET status = ?, updated_at = datetime('now') WHERE repo = ?",
		status, repo,
	)
	return err
}

func DeleteRepoData(repo string) error {
	mu.Lock()
	defer mu.Unlock()
	for _, table := range []string{"deployment_plans", "repo_memory", "processed_issues"} {
		if _, err := db.Exec("DELETE FROM "+table+" WHERE repo = ?", repo); err != nil {
			return err
		}
	}
	return nil
}

// Now returns a formatted timestamp string for consistency with Python.
func Now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}
