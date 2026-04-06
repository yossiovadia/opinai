package dashboard

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

// --- /api/status ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(s.state.StartTime).Seconds()
	dbStats, _ := database.GetTotalStats()
	json.NewEncoder(w).Encode(map[string]any{
		"uptime_seconds":  int(uptime),
		"uptime_human":    FormatDuration(uptime),
		"last_poll":       s.state.GetLastPoll(),
		"poll_count":      s.state.GetPollCount(),
		"repos_count":     len(s.state.GetRepos()),
		"total_runs":      dbStats.TotalRuns,
		"total_processed": dbStats.TotalProcessed,
		"bugs_confirmed":  dbStats.BugsConfirmed,
		"not_reproducible": dbStats.NotReproducible,
	})
}

// --- /api/repos ---

func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	repos := s.state.GetRepos()
	result := make([]map[string]any, 0, len(repos))
	for name, status := range repos {
		result = append(result, map[string]any{
			"name":        name,
			"pending":     status.Pending,
			"processed":   status.Processed,
			"manual_only": status.ManualOnly,
			"is_analyzing": false,
			"last_check":  status.LastCheck,
		})
	}
	json.NewEncoder(w).Encode(result)
}

// --- /api/runs ---

func (s *Server) handleRuns(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", 50)
	repo := r.URL.Query().Get("repo")
	runs, err := database.GetRuns(repo, limit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if runs == nil {
		runs = []database.Run{}
	}
	json.NewEncoder(w).Encode(runs)
}

// --- /api/jobs ---

func (s *Server) handleJobs(w http.ResponseWriter, r *http.Request) {
	if s.listJobs == nil {
		json.NewEncoder(w).Encode([]any{})
		return
	}
	jobs := s.listJobs()
	if jobs == nil {
		jobs = []JobInfo{}
	}
	json.NewEncoder(w).Encode(jobs)
}

// --- /api/check-now ---

func (s *Server) handleCheckNow(w http.ResponseWriter, r *http.Request) {
	s.state.TriggerCheckNow()
	json.NewEncoder(w).Encode(map[string]string{"status": "triggered"})
}

// --- /api/reproduce ---

func (s *Server) handleReproduce(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo        string `json:"repo"`
		IssueNumber int    `json:"issue_number"`
	}
	if err := decodeJSON(r, &req); err != nil {
		jsonError(w, "invalid request", 400)
		return
	}
	if req.Repo == "" || req.IssueNumber == 0 {
		jsonError(w, "repo and issue_number required", 400)
		return
	}

	// Check monitored repos
	monitored := ParseRepos(os.Getenv("REPOS"))
	found := false
	for _, m := range monitored {
		if m == req.Repo {
			found = true
			break
		}
	}
	if !found {
		jsonError(w, "Repo not monitored. Add it via Admin or setup.sh first.", 403)
		return
	}

	if s.reproduce == nil {
		jsonError(w, "Controller not ready", 503)
		return
	}
	// Queue the issue and return immediately — processing happens in background
	database.AddPending(req.Repo, req.IssueNumber, "")
	s.hub.Broadcast(WSEvent{
		Type: "job_queued",
		Data: map[string]any{"repo": req.Repo, "issue": req.IssueNumber},
	})
	json.NewEncoder(w).Encode(map[string]any{
		"status":       "queued",
		"repo":         req.Repo,
		"issue_number": req.IssueNumber,
	})
}

// --- /api/verify-fix ---

func (s *Server) handleVerifyFix(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo        string `json:"repo"`
		IssueNumber int    `json:"issue_number"`
	}
	if err := decodeJSON(r, &req); err != nil {
		jsonError(w, "invalid request", 400)
		return
	}
	if req.Repo == "" || req.IssueNumber == 0 {
		jsonError(w, "repo and issue_number required", 400)
		return
	}
	if s.verifyFix == nil {
		jsonError(w, "verify-fix not available", 503)
		return
	}
	if err := s.verifyFix(req.Repo, req.IssueNumber); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"status":       "triggered",
		"repo":         req.Repo,
		"issue_number": req.IssueNumber,
		"mode":         "verify-fix",
	})
}

// --- /api/runs/{id}/post-comment ---

func (s *Server) handlePostComment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid run id", 400)
		return
	}

	run, err := database.GetRun(id)
	if err != nil || run == nil {
		jsonError(w, "run not found", 404)
		return
	}
	if run.Posted {
		jsonError(w, "already posted", 400)
		return
	}
	if run.Repo == "" || run.Report == "" {
		jsonError(w, "missing repo or report", 400)
		return
	}

	// Post to GitHub
	ghToken := os.Getenv("GITHUB_TOKEN")
	url := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments", run.Repo, run.Issue)
	body := fmt.Sprintf(`{"body":%s}`, jsonString(sanitize(run.Report)))

	req2, _ := http.NewRequest("POST", url, strings.NewReader(body))
	req2.Header.Set("Accept", "application/vnd.github+json")
	req2.Header.Set("Authorization", "Bearer "+ghToken)
	req2.Header.Set("Content-Type", "application/json")

	ghClient := &http.Client{Timeout: 30 * time.Second}
	resp, err := ghClient.Do(req2)
	if err != nil {
		jsonError(w, "GitHub API error: "+err.Error(), 500)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		jsonError(w, fmt.Sprintf("GitHub returned %d", resp.StatusCode), 500)
		return
	}

	database.MarkPosted(id)
	json.NewEncoder(w).Encode(map[string]any{
		"status": "posted",
		"repo":   run.Repo,
		"issue":  run.Issue,
	})
}

// --- DELETE /api/runs/{owner}/{repo}/{issue} ---

func (s *Server) handleDeleteRuns(w http.ResponseWriter, r *http.Request) {
	wildcard := chi.URLParam(r, "*")
	if wildcard == "" {
		wildcard = strings.TrimPrefix(r.URL.Path, "/api/runs/")
	}
	wildcard = strings.TrimPrefix(wildcard, "/")

	parts := strings.Split(wildcard, "/")
	if len(parts) < 2 {
		jsonError(w, "invalid path: expected owner/repo/issue", 400)
		return
	}
	issueStr := parts[len(parts)-1]
	repo := strings.Join(parts[:len(parts)-1], "/")
	issue, err := strconv.Atoi(issueStr)
	if err != nil || issue == 0 {
		jsonError(w, "invalid issue number: "+issueStr, 400)
		return
	}

	database.DeleteIssueRuns(repo, issue)
	if s.clearRecorded != nil {
		s.clearRecorded(repo, issue)
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status": "deleted",
		"repo":   repo,
		"issue":  issue,
	})
}

// --- /api/rerun/{repo}/{issue} ---

func (s *Server) handleRerun(w http.ResponseWriter, r *http.Request) {
	// Parse "owner/repo/123" from wildcard or URL path
	wildcard := chi.URLParam(r, "*")
	if wildcard == "" {
		// Fallback: parse from full path after /api/rerun/
		wildcard = strings.TrimPrefix(r.URL.Path, "/api/rerun/")
	}
	wildcard = strings.TrimPrefix(wildcard, "/")

	parts := strings.Split(wildcard, "/")
	if len(parts) < 2 {
		jsonError(w, "invalid path: expected owner/repo/issue or repo/issue, got: "+wildcard, 400)
		return
	}
	issueStr := parts[len(parts)-1]
	repo := strings.Join(parts[:len(parts)-1], "/")
	issue, err := strconv.Atoi(issueStr)
	if err != nil || issue == 0 {
		jsonError(w, "invalid issue number: "+issueStr, 400)
		return
	}

	database.AddPending(repo, issue, "")
	if s.rerun != nil {
		if err := s.rerun(repo, issue); err != nil {
			jsonError(w, "rerun failed: "+err.Error(), 500)
			return
		}
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status": "rerun_triggered",
		"repo":   repo,
		"issue":  issue,
	})
}

// --- helpers ---

func decodeJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	return json.NewDecoder(r.Body).Decode(v)
}

// --- POST /api/internal/result ---
// Called by the runner pod to report reproduction results directly.

func (s *Server) handleInternalResult(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo               string            `json:"repo"`
		Issue              int               `json:"issue"`
		Title              string            `json:"title"`
		Category           string            `json:"category"`
		Verdict            string            `json:"verdict"`
		Confidence         string            `json:"confidence"`
		Duration           string            `json:"duration"`
		Report             string            `json:"report"`
		SuggestedQuestions string            `json:"suggested_questions"`
		ReproDetails       string            `json:"repro_details"`
		RepoMemory         map[string]string `json:"repo_memory"`
	}
	if err := decodeJSON(r, &req); err != nil {
		jsonError(w, "invalid request: "+err.Error(), 400)
		return
	}
	if req.Repo == "" || req.Issue == 0 {
		jsonError(w, "repo and issue required", 400)
		return
	}

	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, dbErr := database.AddRun(database.Run{
		Repo: req.Repo, Issue: req.Issue, Title: req.Title,
		Category: req.Category, Verdict: req.Verdict, Confidence: req.Confidence,
		Duration: req.Duration, AIPowered: true, Posted: false,
		Report: req.Report, SuggestedQuestions: req.SuggestedQuestions,
		ReproDetails: req.ReproDetails, CreatedAt: ts,
	})
	if dbErr != nil {
		slog.Error("internal result: failed to store run", "error", dbErr)
		jsonError(w, "failed to store run: "+dbErr.Error(), 500)
		return
	}

	// Store repo memory
	for k, v := range req.RepoMemory {
		if v != "" {
			database.SetRepoMemory(req.Repo, k, v)
		}
	}

	// Remove from pending queue and mark as processed BEFORE retry
	database.RemovePending(req.Repo, req.Issue)
	database.MarkProcessed(req.Repo, req.Issue, "")

	// Mark as recorded so the log-scraping harvester skips this job
	if s.markRecorded != nil {
		s.markRecorded(req.Repo, req.Issue)
	}

	s.hub.Broadcast(WSEvent{
		Type: "job_completed",
		Data: map[string]any{"repo": req.Repo, "issue": req.Issue, "verdict": req.Verdict},
	})

	slog.Info("received runner result via callback", "repo", req.Repo, "issue", req.Issue, "verdict", req.Verdict)
	json.NewEncoder(w).Encode(map[string]any{"status": "ok"})

	// Trigger retry of pending issues for this repo (async)
	// Delay briefly so K8s has time to mark the job as inactive
	if s.retryPending != nil {
		go func() {
			time.Sleep(5 * time.Second)
			s.retryPending(req.Repo)
		}()
	}
}

func jsonError(w http.ResponseWriter, msg string, code int) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]any{
		"error":   true,
		"message": msg,
		"status":  code,
	})
}

func intQuery(r *http.Request, key string, fallback int) int {
	if v, err := strconv.Atoi(r.URL.Query().Get(key)); err == nil && v > 0 {
		return v
	}
	return fallback
}

func sanitize(text string) string {
	for _, key := range []string{"AI_API_KEY", "GITHUB_TOKEN"} {
		secret := os.Getenv(key)
		if len(secret) > 8 {
			text = strings.ReplaceAll(text, secret, "REDACTED")
		}
	}
	return text
}

func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// ghGet makes an authenticated GET to the GitHub API.
func ghGet(path string) ([]byte, int, error) {
	token := os.Getenv("GITHUB_TOKEN")
	url := "https://api.github.com" + path
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")

	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}

