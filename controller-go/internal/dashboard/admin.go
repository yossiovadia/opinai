package dashboard

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

// --- GET /api/admin/repos ---

func (s *Server) handleAdminReposGet(w http.ResponseWriter, r *http.Request) {
	repos := ParseRepos(os.Getenv("REPOS"))
	result := make([]map[string]any, 0, len(repos))
	for _, name := range repos {
		profile := loadProfile(name)
		result = append(result, map[string]any{
			"name":    name,
			"profile": profile,
		})
	}
	json.NewEncoder(w).Encode(result)
}

// --- POST /api/admin/repos ---

func (s *Server) handleAdminReposAdd(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string         `json:"name"`
		Profile map[string]any `json:"profile"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Name == "" {
		jsonError(w, "name required", 400)
		return
	}
	if err := updateRepoEnv(req.Name, req.Profile, false); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	// Update state
	s.state.UpdateRepo(req.Name, RepoStatus{ManualOnly: true})
	json.NewEncoder(w).Encode(map[string]any{"status": "added", "name": req.Name})
}

// --- PUT /api/admin/repos ---

func (s *Server) handleAdminReposUpdate(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name    string         `json:"name"`
		Profile map[string]any `json:"profile"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Name == "" {
		jsonError(w, "name required", 400)
		return
	}
	if err := updateRepoEnv(req.Name, req.Profile, false); err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"status": "updated", "name": req.Name})
}

// --- DELETE /api/admin/repos ---

func (s *Server) handleAdminReposDelete(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Name == "" {
		jsonError(w, "name required", 400)
		return
	}
	if err := updateRepoEnv(req.Name, nil, true); err != nil {
		slog.Error("failed to delete repo from env", "repo", req.Name, "error", err)
		jsonError(w, err.Error(), 500)
		return
	}
	// Clean DB
	if err := database.DeleteRepoData(req.Name); err != nil {
		slog.Warn("failed to clean DB for repo", "repo", req.Name, "error", err)
	}
	// Remove from state
	s.state.mu.Lock()
	delete(s.state.Repos, req.Name)
	s.state.mu.Unlock()

	json.NewEncoder(w).Encode(map[string]any{"status": "deleted", "name": req.Name})
}

// --- GET /api/admin/settings ---

func (s *Server) handleAdminSettings(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{
		"poll_interval_minutes": Env("POLL_INTERVAL_MINUTES", "60"),
		"done_label":            Env("DONE_LABEL", "opinai-done"),
		"ai_provider":           os.Getenv("AI_PROVIDER"),
		"ai_model":              os.Getenv("AI_MODEL"),
		"ai_region":             os.Getenv("AI_REGION"),
		"ai_project":            os.Getenv("AI_PROJECT"),
		"namespace":             Env("NAMESPACE", "opinai"),
	})
}

// --- PUT /api/admin/settings ---

func (s *Server) handleAdminSettingsUpdate(w http.ResponseWriter, r *http.Request) {
	var req map[string]string
	if err := decodeJSON(r, &req); err != nil {
		jsonError(w, "invalid request", 400)
		return
	}
	if v, ok := req["poll_interval_minutes"]; ok {
		os.Setenv("POLL_INTERVAL_MINUTES", v)
	}
	if v, ok := req["done_label"]; ok {
		os.Setenv("DONE_LABEL", v)
	}
	// TODO: update K8s ConfigMap (Phase 3)
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// --- GET /api/admin/system ---

func (s *Server) handleAdminSystem(w http.ResponseWriter, r *http.Request) {
	uptime := time.Since(s.state.StartTime).Seconds()
	hostname, _ := os.Hostname()
	json.NewEncoder(w).Encode(map[string]any{
		"namespace":      Env("NAMESPACE", "opinai"),
		"pod_name":       hostname,
		"uptime_human":   FormatDuration(uptime),
		"uptime_seconds": int(uptime),
		"image":          "opinai-controller-go:latest",
		"go_version":     runtime.Version(),
	})
}

// --- GET /api/admin/logs ---

func (s *Server) handleAdminLogs(w http.ResponseWriter, r *http.Request) {
	count := intQuery(r, "count", 50)
	lines := s.logBuf.Last(count)
	json.NewEncoder(w).Encode(lines)
}

// --- POST /api/admin/test-ai ---

func (s *Server) handleAdminTestAI(w http.ResponseWriter, r *http.Request) {
	// Stub: AI integration in Phase 4
	json.NewEncoder(w).Encode(map[string]string{
		"status":  "ok",
		"reply":   "AI test not yet implemented in Go controller.",
	})
}

// --- POST /api/admin/test-github ---

func (s *Server) handleAdminTestGitHub(w http.ResponseWriter, r *http.Request) {
	body, code, err := ghGet("/user")
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
		return
	}
	if code != 200 {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": fmt.Sprintf("HTTP %d", code)})
		return
	}
	var user struct {
		Login string `json:"login"`
	}
	json.Unmarshal(body, &user)
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "login": user.Login})
}

// --- GET /api/admin/db-stats ---

func (s *Server) handleAdminDBStats(w http.ResponseWriter, r *http.Request) {
	stats, _ := database.GetTotalStats()
	// Count repo_memory entries
	var memCount int
	row := database.DB().QueryRow("SELECT COUNT(*) FROM repo_memory")
	row.Scan(&memCount)

	json.NewEncoder(w).Encode(map[string]int{
		"total_runs":        stats.TotalRuns,
		"total_processed":   stats.TotalProcessed,
		"repo_memory_count": memCount,
	})
}

// --- GET /api/admin/db-runs ---

func (s *Server) handleAdminDBRuns(w http.ResponseWriter, r *http.Request) {
	runs, err := database.GetRuns("", 20)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if runs == nil {
		runs = []database.Run{}
	}
	json.NewEncoder(w).Encode(runs)
}

// --- GET /api/admin/db-memory ---

func (s *Server) handleAdminDBMemory(w http.ResponseWriter, r *http.Request) {
	rows, err := database.DB().Query(
		"SELECT repo, key, value, updated_at FROM repo_memory ORDER BY repo, key",
	)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	defer rows.Close()

	result := make(map[string]map[string]map[string]string)
	for rows.Next() {
		var repo, key, value, updatedAt string
		rows.Scan(&repo, &key, &value, &updatedAt)
		if result[repo] == nil {
			result[repo] = make(map[string]map[string]string)
		}
		result[repo][key] = map[string]string{"value": value, "updated_at": updatedAt}
	}
	json.NewEncoder(w).Encode(result)
}

// --- GET /api/admin/repo-memory/{repo} ---

func (s *Server) handleAdminRepoMemory(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	memory, err := database.GetRepoMemory(repo, nil)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	json.NewEncoder(w).Encode(memory)
}

// --- GET /api/admin/deployment-plan/{repo} ---

func (s *Server) handleAdminGetPlan(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	plan, err := database.GetDeploymentPlan(repo)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if plan == nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "not_analyzed"})
		return
	}

	// Parse options from plan_json
	var planData map[string]any
	json.Unmarshal([]byte(plan.PlanJSON), &planData)
	options, _ := planData["options"]

	json.NewEncoder(w).Encode(map[string]any{
		"id":         plan.ID,
		"repo":       plan.Repo,
		"plan_json":  plan.PlanJSON,
		"status":     plan.Status,
		"created_at": plan.CreatedAt,
		"updated_at": plan.UpdatedAt,
		"options":    options,
	})
}

// --- PUT /api/admin/deployment-plan/{repo} ---

func (s *Server) handleAdminUpdatePlan(w http.ResponseWriter, r *http.Request) {
	repo := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	var req map[string]any
	if err := decodeJSON(r, &req); err != nil {
		jsonError(w, "invalid request", 400)
		return
	}
	if pj, ok := req["plan_json"]; ok {
		b, _ := json.Marshal(pj)
		database.SaveDeploymentPlan(repo, string(b))
	}
	if st, ok := req["status"].(string); ok {
		database.UpdateDeploymentPlanStatus(repo, st)
	}
	json.NewEncoder(w).Encode(map[string]string{"status": "updated"})
}

// --- POST /api/admin/analyze-deployment ---

func (s *Server) handleAdminAnalyze(w http.ResponseWriter, r *http.Request) {
	// Stub: AI integration in Phase 4
	json.NewEncoder(w).Encode(map[string]string{
		"error": "Deployment analysis not yet implemented in Go controller. Use the Python dashboard on port 8080.",
	})
}

// --- GET /api/admin/sandboxes ---

func (s *Server) handleAdminSandboxes(w http.ResponseWriter, r *http.Request) {
	// Stub: sandbox integration in Phase 7
	json.NewEncoder(w).Encode([]any{})
}

// --- DELETE /api/admin/sandboxes/{ns} ---

func (s *Server) handleAdminSandboxDelete(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]string{"status": "not_implemented"})
}

// --- POST /api/admin/sandboxes/cleanup ---

func (s *Server) handleAdminSandboxCleanup(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(map[string]int{"cleaned": 0})
}

// --- repo profile helpers ---

func loadProfile(repo string) map[string]any {
	key := profileKey(repo)
	raw := os.Getenv(key)
	if raw == "" {
		return map[string]any{}
	}
	var profile map[string]any
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		return map[string]any{}
	}
	return profile
}

func profileKey(repo string) string {
	r := strings.NewReplacer("/", "_", "-", "_", ".", "_")
	return "REPO_PROFILE_" + r.Replace(repo)
}

func updateRepoEnv(repo string, profile map[string]any, delete bool) error {
	repos := ParseRepos(os.Getenv("REPOS"))
	key := profileKey(repo)

	if delete {
		filtered := make([]string, 0, len(repos))
		for _, r := range repos {
			if r != repo {
				filtered = append(filtered, r)
			}
		}
		os.Setenv("REPOS", strings.Join(filtered, ","))
		os.Unsetenv(key)
	} else {
		// Add if not present
		found := false
		for _, r := range repos {
			if r == repo {
				found = true
				break
			}
		}
		if !found {
			repos = append(repos, repo)
		}
		os.Setenv("REPOS", strings.Join(repos, ","))
		if profile != nil {
			b, _ := json.Marshal(profile)
			os.Setenv(key, string(b))
		}
	}
	// TODO: update K8s ConfigMap (Phase 3)
	return nil
}
