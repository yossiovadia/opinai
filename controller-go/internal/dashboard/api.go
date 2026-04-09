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
		"uptime_seconds":       int(uptime),
		"uptime_human":         FormatDuration(uptime),
		"last_poll":            s.state.GetLastPoll(),
		"poll_count":           s.state.GetPollCount(),
		"repos_count":          len(s.state.GetRepos()),
		"total_runs":           dbStats.TotalRuns,
		"total_processed":      dbStats.TotalProcessed,
		"bugs_confirmed":       dbStats.BugsConfirmed,
		"not_reproducible":     dbStats.NotReproducible,
		"prs_reviewed":         dbStats.PRsReviewed,
		"prs_approved":         dbStats.PRsApproved,
		"prs_changes_requested": dbStats.PRsChangesReq,
		"prs_commented":        dbStats.PRsCommented,
	})
}

// --- /api/repos ---

func (s *Server) handleRepos(w http.ResponseWriter, r *http.Request) {
	repos := s.state.GetRepos()
	result := make([]map[string]any, 0, len(repos))
	for name, status := range repos {
		result = append(result, map[string]any{
			"name":           name,
			"pending":        status.Pending,
			"processed":      status.Processed,
			"manual_only":    status.ManualOnly,
			"is_analyzing":   false,
			"last_check":     status.LastCheck,
			"memory_events":  database.CountMemoryEvents(name),
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

	// Store repo memory with reason
	reason := fmt.Sprintf("investigation #%d", req.Issue)
	for k, v := range req.RepoMemory {
		if v != "" {
			database.SetRepoMemoryWithReason(req.Repo, k, v, reason, "runner")
		}
	}

	// Extract investigation findings from repro_details
	extractAndStoreFindings(req.Repo, req.Issue, req.Verdict, req.Confidence, req.ReproDetails)

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

// --- /api/pr-reviews ---

func (s *Server) handlePRReviews(w http.ResponseWriter, r *http.Request) {
	limit := intQuery(r, "limit", 50)
	repo := r.URL.Query().Get("repo")
	reviews, err := database.GetPRReviews(repo, limit)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if reviews == nil {
		reviews = []database.PRReview{}
	}
	json.NewEncoder(w).Encode(reviews)
}

// --- /api/pr-reviews/{id} ---

func (s *Server) handlePRReview(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid id", 400)
		return
	}
	review, err := database.GetPRReview(id)
	if err != nil || review == nil {
		jsonError(w, "review not found", 404)
		return
	}
	json.NewEncoder(w).Encode(review)
}

// --- POST /api/review-pr ---

func (s *Server) handleReviewPR(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo     string `json:"repo"`
		PRNumber int    `json:"pr_number"`
	}
	if err := decodeJSON(r, &req); err != nil {
		jsonError(w, "invalid request", 400)
		return
	}
	if req.Repo == "" || req.PRNumber == 0 {
		jsonError(w, "repo and pr_number required", 400)
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
		jsonError(w, "Repo not monitored. Add it via Admin first.", 403)
		return
	}

	if s.reviewPR == nil {
		jsonError(w, "PR review not available", 503)
		return
	}

	// Fetch PR title for the job
	title := fmt.Sprintf("PR #%d", req.PRNumber)
	if err := s.reviewPR(req.Repo, req.PRNumber, title); err != nil {
		jsonError(w, "failed to create PR review job: "+err.Error(), 500)
		return
	}

	s.hub.Broadcast(WSEvent{
		Type: "pr_review_queued",
		Data: map[string]any{"repo": req.Repo, "pr_number": req.PRNumber},
	})

	json.NewEncoder(w).Encode(map[string]any{
		"status":    "queued",
		"repo":      req.Repo,
		"pr_number": req.PRNumber,
	})
}

// --- POST /api/pr-reviews/{id}/post-comment ---

func (s *Server) handlePostPRComment(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid review id", 400)
		return
	}

	review, err := database.GetPRReview(id)
	if err != nil || review == nil {
		jsonError(w, "review not found", 404)
		return
	}
	if review.Posted {
		jsonError(w, "already posted", 400)
		return
	}
	if review.Repo == "" || review.ReviewText == "" {
		jsonError(w, "missing repo or review text", 400)
		return
	}

	// Post to GitHub as a PR comment (issue comments API works for PRs too)
	ghToken := os.Getenv("GITHUB_TOKEN")
	url := fmt.Sprintf("https://api.github.com/repos/%s/issues/%d/comments", review.Repo, review.PRNumber)
	body := fmt.Sprintf(`{"body":%s}`, jsonString(sanitize(review.ReviewText)))

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

	database.MarkPRReviewPosted(id)
	json.NewEncoder(w).Encode(map[string]any{
		"status":    "posted",
		"repo":      review.Repo,
		"pr_number": review.PRNumber,
	})
}

// --- POST /api/internal/pr-result ---
// Called by the runner pod to report PR review results directly.

func (s *Server) handleInternalPRResult(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo     string `json:"repo"`
		PRNumber int    `json:"pr_number"`
		Title    string `json:"title"`
		Author   string `json:"author"`
		Verdict  string `json:"verdict"`
		Risk     string `json:"risk"`
		Report   string `json:"report"`
		Duration string `json:"duration"`
	}
	if err := decodeJSON(r, &req); err != nil {
		jsonError(w, "invalid request: "+err.Error(), 400)
		return
	}
	if req.Repo == "" || req.PRNumber == 0 {
		jsonError(w, "repo and pr_number required", 400)
		return
	}

	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	_, dbErr := database.AddPRReview(database.PRReview{
		Repo: req.Repo, PRNumber: req.PRNumber, Title: req.Title,
		Author: req.Author, Verdict: req.Verdict, Risk: req.Risk,
		ReviewText: req.Report, Posted: false, Duration: req.Duration, CreatedAt: ts,
	})
	if dbErr != nil {
		slog.Error("internal PR result: failed to store review", "error", dbErr)
		jsonError(w, "failed to store review: "+dbErr.Error(), 500)
		return
	}

	// Extract investigation findings from the PR review text
	extractAndStoreFindingsFromPRReview(req.Repo, req.PRNumber, req.Verdict, req.Report)

	// Mark as recorded so the log-scraping harvester skips this job
	if s.markPRRecorded != nil {
		s.markPRRecorded(req.Repo, req.PRNumber)
	}

	s.hub.Broadcast(WSEvent{
		Type: "pr_review_completed",
		Data: map[string]any{"repo": req.Repo, "pr_number": req.PRNumber, "verdict": req.Verdict, "risk": req.Risk},
	})

	slog.Info("received PR review result via callback", "repo", req.Repo, "pr", req.PRNumber, "verdict", req.Verdict)
	json.NewEncoder(w).Encode(map[string]any{"status": "ok"})
}

// extractAndStoreFindings parses repro_details JSON to extract files_investigated
// and stores findings with per-file context from the summary text.
func extractAndStoreFindings(repo string, issue int, verdict, confidence, reproDetailsJSON string) {
	if reproDetailsJSON == "" {
		return
	}
	var details map[string]any
	if err := json.Unmarshal([]byte(reproDetailsJSON), &details); err != nil {
		return
	}

	// Get the files investigated
	filesRaw, ok := details["files_investigated"]
	if !ok {
		return
	}
	filesSlice, ok := filesRaw.([]any)
	if !ok || len(filesSlice) == 0 {
		return
	}

	// Build the overall summary
	summary := ""
	if s, ok := details["summary"].(string); ok && s != "" {
		summary = s
	}

	// Extract per-file findings from the summary text.
	// The summary often contains references like "middleware.go:145" or "in handler.go"
	// near relevant descriptions. Build a map of file -> contextual sentence.
	fileFindings := extractPerFileFindings(summary, filesSlice)

	for _, f := range filesSlice {
		filePath, ok := f.(string)
		if !ok || filePath == "" {
			continue
		}

		finding := ""
		if pf, ok := fileFindings[filePath]; ok {
			finding = pf
		} else if summary != "" {
			finding = truncStr(summary, 500)
		} else {
			finding = verdict
		}

		database.AddInvestigationFinding(database.InvestigationFinding{
			Repo:        repo,
			IssueNumber: issue,
			FilePath:    filePath,
			Finding:     finding,
			Verdict:     verdict,
			Confidence:  confidence,
		})
	}
	if len(filesSlice) > 0 {
		slog.Info("stored investigation findings", "repo", repo, "issue", issue, "files", len(filesSlice))
	}
}

// extractPerFileFindings scans summary text for sentences that reference specific files
// from the investigated files list, returning file -> relevant context.
func extractPerFileFindings(summary string, files []any) map[string]string {
	result := make(map[string]string)
	if summary == "" {
		return result
	}

	// Build a set of base filenames and full paths for matching
	fileSet := make(map[string]string) // baseName -> fullPath
	for _, f := range files {
		fp, ok := f.(string)
		if !ok || fp == "" {
			continue
		}
		// Store both the full path and the basename
		parts := strings.Split(fp, "/")
		base := parts[len(parts)-1]
		fileSet[base] = fp
		fileSet[fp] = fp
	}

	// Split summary into sentences (roughly)
	sentences := splitSentences(summary)
	for _, sentence := range sentences {
		trimmed := strings.TrimSpace(sentence)
		if trimmed == "" {
			continue
		}
		// Check if this sentence references any investigated file
		for name, fullPath := range fileSet {
			if strings.Contains(trimmed, name) {
				existing := result[fullPath]
				if existing == "" {
					result[fullPath] = truncStr(trimmed, 500)
				} else if len(existing) < 500 {
					result[fullPath] = truncStr(existing+"; "+trimmed, 500)
				}
			}
		}
	}
	return result
}

// splitSentences splits text into sentences on period/newline boundaries.
func splitSentences(text string) []string {
	// Split on newlines first, then on sentence-ending punctuation
	var sentences []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		// Split on ". " but keep the sentence together otherwise
		parts := strings.Split(line, ". ")
		for _, p := range parts {
			p = strings.TrimSpace(p)
			if p != "" {
				sentences = append(sentences, p)
			}
		}
	}
	return sentences
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// BackfillFindings reads all existing runs with repro_details and extracts
// investigation findings for any that don't already have findings stored.
// Idempotent — safe to call multiple times.
func BackfillFindings() {
	runs, err := database.GetAllRunsWithReproDetails()
	if err != nil {
		slog.Error("backfill findings: failed to get runs", "error", err)
		return
	}

	backfilled := 0
	for _, run := range runs {
		if run.ReproDetails == "" || run.Verdict == "" {
			continue
		}
		// Skip if findings already exist for this repo+issue
		has, _ := database.HasFindingsForIssue(run.Repo, run.Issue)
		if has {
			continue
		}
		extractAndStoreFindings(run.Repo, run.Issue, run.Verdict, run.Confidence, run.ReproDetails)
		backfilled++
	}
	if backfilled > 0 {
		slog.Info("backfilled investigation findings", "runs_processed", backfilled)
	}
}

// extractAndStoreFindingsFromPRReview parses a PR review's text to find
// file-specific findings and stores them as investigation findings.
// Uses the PR number as the reference and "PR_REVIEW" as the verdict prefix.
func extractAndStoreFindingsFromPRReview(repo string, prNumber int, verdict, reviewText string) {
	if reviewText == "" {
		return
	}

	// Extract file paths mentioned in the review text.
	// PR reviews commonly reference files in backtick-quoted paths or markdown code blocks.
	files := extractFilePathsFromText(reviewText)
	if len(files) == 0 {
		return
	}

	// Build per-file findings from context around file mentions
	filesAny := make([]any, len(files))
	for i, f := range files {
		filesAny[i] = f
	}
	fileFindings := extractPerFileFindings(reviewText, filesAny)

	stored := 0
	for _, filePath := range files {
		finding := ""
		if pf, ok := fileFindings[filePath]; ok {
			finding = pf
		} else {
			finding = truncStr(reviewText, 500)
		}

		database.AddInvestigationFinding(database.InvestigationFinding{
			Repo:        repo,
			IssueNumber: prNumber,
			FilePath:    filePath,
			Finding:     finding,
			Verdict:     "PR_REVIEW:" + verdict,
			Confidence:  "MEDIUM",
		})
		stored++
	}
	if stored > 0 {
		slog.Info("stored PR review findings", "repo", repo, "pr", prNumber, "files", stored)
	}
}

// extractFilePathsFromText finds file paths in review text.
// Looks for backtick-quoted paths (e.g. `src/main.go`) and bare paths with extensions.
func extractFilePathsFromText(text string) []string {
	seen := make(map[string]bool)
	var paths []string

	// Find backtick-quoted paths that look like file references
	for _, part := range strings.Split(text, "`") {
		trimmed := strings.TrimSpace(part)
		// Strip line number suffix like :42 before checking
		candidate := trimmed
		if idx := strings.LastIndex(candidate, ":"); idx > 0 {
			stripped := candidate[:idx]
			if looksLikeFilePath(stripped) {
				candidate = stripped
			}
		}
		if looksLikeFilePath(candidate) {
			if !seen[candidate] {
				seen[candidate] = true
				paths = append(paths, candidate)
			}
		}
	}
	return paths
}

// looksLikeFilePath checks if a string resembles a file path.
func looksLikeFilePath(s string) bool {
	if s == "" || len(s) > 200 || strings.ContainsAny(s, " \t\n{}()[]<>") {
		return false
	}
	// Must have a file extension
	dot := strings.LastIndex(s, ".")
	if dot < 1 || dot >= len(s)-1 {
		return false
	}
	ext := s[dot+1:]
	// Common code file extensions
	for _, e := range []string{"go", "py", "js", "ts", "tsx", "jsx", "rs", "java", "rb", "c", "cpp", "h", "hpp", "yaml", "yml", "json", "toml", "md", "sql", "sh", "css", "html", "vue", "svelte", "proto", "graphql", "tf"} {
		if ext == e {
			return true
		}
	}
	return false
}

// handleCheckOutcomes handles POST /api/admin/check-outcomes — triggers outcome checking immediately.
func (s *Server) handleCheckOutcomes(w http.ResponseWriter, r *http.Request) {
	if s.checkOutcomes == nil {
		jsonError(w, "outcome checking not available", 503)
		return
	}
	go s.checkOutcomes()
	json.NewEncoder(w).Encode(map[string]any{
		"status":  "ok",
		"message": "outcome check triggered",
	})
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

