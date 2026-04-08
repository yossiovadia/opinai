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

		CREATE TABLE IF NOT EXISTS chat_history (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			repo TEXT NOT NULL,
			issue INTEGER NOT NULL,
			role TEXT NOT NULL,
			content TEXT NOT NULL,
			created_at TEXT DEFAULT (datetime('now'))
		);

		CREATE INDEX IF NOT EXISTS idx_runs_repo ON runs(repo);
		CREATE INDEX IF NOT EXISTS idx_runs_repo_issue ON runs(repo, issue);
		CREATE INDEX IF NOT EXISTS idx_chat_repo_issue ON chat_history(repo, issue);
	`)
	if err != nil {
		return err
	}
	// Migrations for existing DBs
	db.Exec("ALTER TABLE deployment_plans ADD COLUMN commit_sha TEXT DEFAULT ''")
	db.Exec("ALTER TABLE runs ADD COLUMN suggested_questions TEXT DEFAULT ''")
	db.Exec("ALTER TABLE runs ADD COLUMN repro_details TEXT DEFAULT ''")
	db.Exec(`CREATE TABLE IF NOT EXISTS pending_reproductions (
		repo TEXT NOT NULL,
		issue INTEGER NOT NULL,
		title TEXT DEFAULT '',
		created_at TEXT DEFAULT (datetime('now')),
		PRIMARY KEY (repo, issue)
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS monitored_repos (
		repo TEXT NOT NULL PRIMARY KEY,
		created_at TEXT DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS host_profiles (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		profile_json TEXT NOT NULL,
		created_at TEXT DEFAULT (datetime('now'))
	)`)
	db.Exec(`CREATE TABLE IF NOT EXISTS pr_reviews (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		repo TEXT NOT NULL,
		pr_number INTEGER NOT NULL,
		title TEXT,
		author TEXT,
		verdict TEXT,
		risk TEXT,
		review_text TEXT,
		posted BOOLEAN DEFAULT FALSE,
		posted_at TEXT,
		duration TEXT,
		created_at TEXT DEFAULT (datetime('now')),
		UNIQUE(repo, pr_number, created_at)
	)`)
	db.Exec("CREATE INDEX IF NOT EXISTS idx_pr_reviews_repo ON pr_reviews(repo)")
	db.Exec("CREATE INDEX IF NOT EXISTS idx_pr_reviews_repo_pr ON pr_reviews(repo, pr_number)")
	db.Exec(`CREATE TABLE IF NOT EXISTS infra_deps (
		name TEXT NOT NULL PRIMARY KEY,
		namespace TEXT NOT NULL DEFAULT 'opinai-infra',
		status TEXT NOT NULL DEFAULT 'not_installed',
		installed_at TEXT,
		last_used_at TEXT,
		connection TEXT,
		helm_release TEXT,
		created_at TEXT DEFAULT (datetime('now'))
	)`)
	return nil
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
	AIPowered          bool    `json:"ai"`
	Duration           string  `json:"duration"`
	SuggestedQuestions string  `json:"suggested_questions,omitempty"`
	ReproDetails       string  `json:"repro_details,omitempty"`
	CreatedAt          string  `json:"timestamp"`
}

func AddRun(r Run) (int64, error) {
	mu.Lock()
	defer mu.Unlock()
	res, err := db.Exec(
		`INSERT OR REPLACE INTO runs
		 (repo, issue, title, category, verdict, confidence, report, posted, ai_powered, duration, suggested_questions, repro_details, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Repo, r.Issue, r.Title, r.Category, r.Verdict, r.Confidence,
		r.Report, r.Posted, r.AIPowered, r.Duration, r.SuggestedQuestions, r.ReproDetails, r.CreatedAt,
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
		rows, err = db.Query("SELECT id,repo,issue,title,category,verdict,confidence,report,posted,posted_at,ai_powered,duration,suggested_questions,repro_details,created_at FROM runs WHERE repo = ? ORDER BY created_at DESC LIMIT ?", repo, limit)
	} else {
		rows, err = db.Query("SELECT id,repo,issue,title,category,verdict,confidence,report,posted,posted_at,ai_powered,duration,suggested_questions,repro_details,created_at FROM runs ORDER BY created_at DESC LIMIT ?", limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

// GetRunsByIssue returns all runs for a specific repo+issue, newest first.
func GetRunsByIssue(repo string, issue int) ([]Run, error) {
	rows, err := db.Query(
		"SELECT id,repo,issue,title,category,verdict,confidence,report,posted,posted_at,ai_powered,duration,suggested_questions,repro_details,created_at FROM runs WHERE repo = ? AND issue = ? ORDER BY created_at DESC",
		repo, issue,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanRuns(rows)
}

func GetRun(id int64) (*Run, error) {
	row := db.QueryRow("SELECT id,repo,issue,title,category,verdict,confidence,report,posted,posted_at,ai_powered,duration,suggested_questions,repro_details,created_at FROM runs WHERE id = ?", id)
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
		&r.Confidence, &r.Report, &r.Posted, &r.PostedAt, &r.AIPowered, &r.Duration, &r.SuggestedQuestions, &r.ReproDetails, &r.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func scanRun(row *sql.Row) (*Run, error) {
	var r Run
	err := row.Scan(&r.ID, &r.Repo, &r.Issue, &r.Title, &r.Category, &r.Verdict,
		&r.Confidence, &r.Report, &r.Posted, &r.PostedAt, &r.AIPowered, &r.Duration, &r.SuggestedQuestions, &r.ReproDetails, &r.CreatedAt)
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
	TotalRuns        int `json:"total_runs"`
	TotalProcessed   int `json:"total_processed"`
	BugsConfirmed    int `json:"bugs_confirmed"`
	NotReproducible  int `json:"not_reproducible"`
}

func GetTotalStats() (TotalStats, error) {
	var s TotalStats
	db.QueryRow("SELECT COUNT(*) FROM runs").Scan(&s.TotalRuns)
	db.QueryRow("SELECT COUNT(*) FROM processed_issues").Scan(&s.TotalProcessed)
	db.QueryRow("SELECT COUNT(*) FROM runs WHERE verdict = 'BUG_CONFIRMED'").Scan(&s.BugsConfirmed)
	db.QueryRow("SELECT COUNT(*) FROM runs WHERE verdict = 'NOT_REPRODUCIBLE'").Scan(&s.NotReproducible)
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

// DeleteIssueRuns removes all runs and processed_issues entries for a repo+issue.
func DeleteIssueRuns(repo string, issue int) error {
	mu.Lock()
	defer mu.Unlock()
	if _, err := db.Exec("DELETE FROM runs WHERE repo = ? AND issue = ?", repo, issue); err != nil {
		return err
	}
	_, err := db.Exec("DELETE FROM processed_issues WHERE repo = ? AND issue = ?", repo, issue)
	return err
}

// DeleteProcessedForRepo removes all processed_issues entries for a repo.
func DeleteProcessedForRepo(repo string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("DELETE FROM processed_issues WHERE repo = ?", repo)
	return err
}

// DeleteProcessedIssue removes a single processed_issues entry.
func DeleteProcessedIssue(repo string, issue int) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("DELETE FROM processed_issues WHERE repo = ? AND issue = ?", repo, issue)
	return err
}

// --- Pending Reproductions ---

type PendingItem struct {
	Repo  string `json:"repo"`
	Issue int    `json:"issue"`
	Title string `json:"title"`
}

func AddPending(repo string, issue int, title string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec(
		"INSERT OR IGNORE INTO pending_reproductions (repo, issue, title) VALUES (?, ?, ?)",
		repo, issue, title,
	)
	return err
}

func RemovePending(repo string, issue int) {
	mu.Lock()
	defer mu.Unlock()
	db.Exec("DELETE FROM pending_reproductions WHERE repo = ? AND issue = ?", repo, issue)
}

func GetPendingForRepo(repo string) []PendingItem {
	rows, err := db.Query("SELECT repo, issue, title FROM pending_reproductions WHERE repo = ? ORDER BY created_at", repo)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var items []PendingItem
	for rows.Next() {
		var p PendingItem
		rows.Scan(&p.Repo, &p.Issue, &p.Title)
		items = append(items, p)
	}
	return items
}

// GetAllPending returns all pending reproductions across all repos.
func GetAllPending() []PendingItem {
	rows, err := db.Query("SELECT repo, issue, title FROM pending_reproductions ORDER BY created_at")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var items []PendingItem
	for rows.Next() {
		var p PendingItem
		rows.Scan(&p.Repo, &p.Issue, &p.Title)
		items = append(items, p)
	}
	return items
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
// --- Chat History ---

type ChatMessage struct {
	ID        int64  `json:"id"`
	Repo      string `json:"repo"`
	Issue     int    `json:"issue"`
	Role      string `json:"role"`
	Content   string `json:"content"`
	CreatedAt string `json:"created_at"`
}

func AddChatMessage(repo string, issue int, role, content string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec(
		"INSERT INTO chat_history (repo, issue, role, content) VALUES (?, ?, ?, ?)",
		repo, issue, role, content,
	)
	return err
}

func GetChatHistory(repo string, issue int) ([]ChatMessage, error) {
	rows, err := db.Query(
		"SELECT id, repo, issue, role, content, created_at FROM chat_history WHERE repo = ? AND issue = ? ORDER BY created_at ASC",
		repo, issue,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var msgs []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.ID, &m.Repo, &m.Issue, &m.Role, &m.Content, &m.CreatedAt); err != nil {
			return nil, err
		}
		msgs = append(msgs, m)
	}
	return msgs, rows.Err()
}

func ClearChatHistory(repo string, issue int) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("DELETE FROM chat_history WHERE repo = ? AND issue = ?", repo, issue)
	return err
}

// --- Monitored Repos ---

// AddMonitoredRepo persists a repo to the monitored list.
func AddMonitoredRepo(repo string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("INSERT OR IGNORE INTO monitored_repos (repo) VALUES (?)", repo)
	return err
}

// RemoveMonitoredRepo removes a repo from the monitored list.
func RemoveMonitoredRepo(repo string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("DELETE FROM monitored_repos WHERE repo = ?", repo)
	return err
}

// GetMonitoredRepos returns all persisted monitored repos.
func GetMonitoredRepos() []string {
	rows, err := db.Query("SELECT repo FROM monitored_repos ORDER BY created_at")
	if err != nil {
		return nil
	}
	defer rows.Close()
	var repos []string
	for rows.Next() {
		var repo string
		rows.Scan(&repo)
		repos = append(repos, repo)
	}
	return repos
}

func Now() string {
	return time.Now().UTC().Format("2006-01-02T15:04:05Z")
}

// --- Host Profile ---

// SaveHostProfile stores the host profile JSON, replacing any previous entry.
func SaveHostProfile(profileJSON string) error {
	mu.Lock()
	defer mu.Unlock()
	// Keep only the latest profile
	db.Exec("DELETE FROM host_profiles")
	_, err := db.Exec("INSERT INTO host_profiles (profile_json) VALUES (?)", profileJSON)
	return err
}

// GetHostProfile returns the most recent host profile JSON, or empty string if none.
func GetHostProfile() string {
	var j string
	err := db.QueryRow("SELECT profile_json FROM host_profiles ORDER BY created_at DESC LIMIT 1").Scan(&j)
	if err != nil {
		return ""
	}
	return j
}

// --- PR Reviews ---

// PRReview represents a PR review result.
type PRReview struct {
	ID         int64   `json:"id"`
	Repo       string  `json:"repo"`
	PRNumber   int     `json:"pr_number"`
	Title      string  `json:"title"`
	Author     string  `json:"author"`
	Verdict    string  `json:"verdict"`
	Risk       string  `json:"risk"`
	ReviewText string  `json:"review_text"`
	Posted     bool    `json:"posted"`
	PostedAt   *string `json:"posted_at"`
	Duration   string  `json:"duration"`
	CreatedAt  string  `json:"timestamp"`
}

func AddPRReview(r PRReview) (int64, error) {
	mu.Lock()
	defer mu.Unlock()
	res, err := db.Exec(
		`INSERT OR REPLACE INTO pr_reviews
		 (repo, pr_number, title, author, verdict, risk, review_text, posted, duration, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		r.Repo, r.PRNumber, r.Title, r.Author, r.Verdict, r.Risk,
		r.ReviewText, r.Posted, r.Duration, r.CreatedAt,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func GetPRReviews(repo string, limit int) ([]PRReview, error) {
	var rows *sql.Rows
	var err error
	if repo != "" {
		rows, err = db.Query("SELECT id,repo,pr_number,title,author,verdict,risk,review_text,posted,posted_at,duration,created_at FROM pr_reviews WHERE repo = ? ORDER BY created_at DESC LIMIT ?", repo, limit)
	} else {
		rows, err = db.Query("SELECT id,repo,pr_number,title,author,verdict,risk,review_text,posted,posted_at,duration,created_at FROM pr_reviews ORDER BY created_at DESC LIMIT ?", limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPRReviews(rows)
}

func GetPRReview(id int64) (*PRReview, error) {
	row := db.QueryRow("SELECT id,repo,pr_number,title,author,verdict,risk,review_text,posted,posted_at,duration,created_at FROM pr_reviews WHERE id = ?", id)
	var r PRReview
	err := row.Scan(&r.ID, &r.Repo, &r.PRNumber, &r.Title, &r.Author, &r.Verdict,
		&r.Risk, &r.ReviewText, &r.Posted, &r.PostedAt, &r.Duration, &r.CreatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func GetPRReviewsByPR(repo string, prNumber int) ([]PRReview, error) {
	rows, err := db.Query(
		"SELECT id,repo,pr_number,title,author,verdict,risk,review_text,posted,posted_at,duration,created_at FROM pr_reviews WHERE repo = ? AND pr_number = ? ORDER BY created_at DESC",
		repo, prNumber,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanPRReviews(rows)
}

func MarkPRReviewPosted(id int64) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("UPDATE pr_reviews SET posted = TRUE, posted_at = datetime('now') WHERE id = ?", id)
	return err
}

func scanPRReviews(rows *sql.Rows) ([]PRReview, error) {
	var result []PRReview
	for rows.Next() {
		var r PRReview
		err := rows.Scan(&r.ID, &r.Repo, &r.PRNumber, &r.Title, &r.Author, &r.Verdict,
			&r.Risk, &r.ReviewText, &r.Posted, &r.PostedAt, &r.Duration, &r.CreatedAt)
		if err != nil {
			return nil, err
		}
		result = append(result, r)
	}
	return result, rows.Err()
}

// --- Infrastructure Dependencies ---

type InfraDep struct {
	Name        string  `json:"name"`
	Namespace   string  `json:"namespace"`
	Status      string  `json:"status"`
	InstalledAt *string `json:"installed_at"`
	LastUsedAt  *string `json:"last_used_at"`
	Connection  string  `json:"connection"`
	HelmRelease string  `json:"helm_release"`
}

func UpsertInfraDep(dep InfraDep) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec(
		`INSERT INTO infra_deps (name, namespace, status, installed_at, last_used_at, connection, helm_release)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(name) DO UPDATE SET
		   namespace = ?, status = ?, installed_at = ?, last_used_at = ?, connection = ?, helm_release = ?`,
		dep.Name, dep.Namespace, dep.Status, dep.InstalledAt, dep.LastUsedAt, dep.Connection, dep.HelmRelease,
		dep.Namespace, dep.Status, dep.InstalledAt, dep.LastUsedAt, dep.Connection, dep.HelmRelease,
	)
	return err
}

func GetInfraDep(name string) (*InfraDep, error) {
	var d InfraDep
	err := db.QueryRow(
		"SELECT name, namespace, status, installed_at, last_used_at, connection, helm_release FROM infra_deps WHERE name = ?",
		name,
	).Scan(&d.Name, &d.Namespace, &d.Status, &d.InstalledAt, &d.LastUsedAt, &d.Connection, &d.HelmRelease)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func ListInfraDeps() ([]InfraDep, error) {
	rows, err := db.Query("SELECT name, namespace, status, installed_at, last_used_at, connection, helm_release FROM infra_deps ORDER BY name")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deps []InfraDep
	for rows.Next() {
		var d InfraDep
		if err := rows.Scan(&d.Name, &d.Namespace, &d.Status, &d.InstalledAt, &d.LastUsedAt, &d.Connection, &d.HelmRelease); err != nil {
			return nil, err
		}
		deps = append(deps, d)
	}
	return deps, rows.Err()
}

func UpdateInfraDepStatus(name, status string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("UPDATE infra_deps SET status = ? WHERE name = ?", status, name)
	return err
}

func TouchInfraDepUsed(name string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("UPDATE infra_deps SET last_used_at = datetime('now') WHERE name = ?", name)
	return err
}

func DeleteInfraDep(name string) error {
	mu.Lock()
	defer mu.Unlock()
	_, err := db.Exec("DELETE FROM infra_deps WHERE name = ?", name)
	return err
}
