package dashboard

import (
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/config"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

// --- GET /api/admin/repos ---

func (s *Server) handleAdminReposGet(w http.ResponseWriter, r *http.Request) {
	// Merge env var repos with DB-persisted repos
	envRepos := ParseRepos(os.Getenv("REPOS"))
	dbRepos := database.GetMonitoredRepos()
	seen := make(map[string]bool)
	var repos []string
	for _, r := range envRepos {
		if !seen[r] {
			seen[r] = true
			repos = append(repos, r)
		}
	}
	for _, r := range dbRepos {
		if !seen[r] {
			seen[r] = true
			repos = append(repos, r)
		}
	}
	result := make([]map[string]any, 0, len(repos))
	for _, name := range repos {
		profile := config.LoadRepoProfile(name)
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
	database.AddMonitoredRepo(req.Name)
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
	database.RemoveMonitoredRepo(req.Name)
	s.state.DeleteRepo(req.Name)

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
	reply, err := ai.Call("You are OpinAI. Respond with exactly: OpinAI is online.", 64)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"status": "error", "message": err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"status": "ok", "reply": ai.Sanitize(reply)})
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
	var req struct {
		Repo string `json:"repo"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Repo == "" {
		jsonError(w, "repo required", 400)
		return
	}

	// Read repo files from GitHub
	files := fetchRepoDeployFiles(req.Repo)
	readme := fetchRepoReadme(req.Repo)
	profile := config.LoadRepoProfile(req.Repo)
	profileJSON, _ := json.Marshal(profile)

	// Read cluster state
	clusterState := readClusterState()

	// Load rich_analysis for deployment context
	richAnalysis := ""
	raKey := "rich_analysis"
	if mem, _ := database.GetRepoMemory(req.Repo, &raKey); len(mem) > 0 {
		richAnalysis = mem["rich_analysis"]
	}

	// Call AI
	planData, err := ai.AnalyzeDeployment(req.Repo, readme, files, clusterState, string(profileJSON), richAnalysis, "")
	if err != nil {
		jsonError(w, "Analysis failed: "+err.Error(), 500)
		return
	}

	// Save to DB
	planBytes, _ := json.Marshal(planData)
	database.SaveDeploymentPlan(req.Repo, string(planBytes))

	// Auto-update profile from analysis
	autoUpdateProfileFromPlan(req.Repo, planData)

	// Store runtime_requirements if present
	if rr, ok := planData["runtime_requirements"]; ok {
		rrBytes, _ := json.Marshal(rr)
		database.SetRepoMemory(req.Repo, "runtime_requirements", string(rrBytes))
		slog.Info("stored runtime_requirements", "repo", req.Repo)
	}

	json.NewEncoder(w).Encode(planData)
}

// --- GET /api/admin/sandboxes ---

func (s *Server) handleAdminSandboxes(w http.ResponseWriter, r *http.Request) {
	if s.sandbox == nil {
		json.NewEncoder(w).Encode([]any{})
		return
	}
	sandboxes := s.sandbox.ListSandboxes()
	if sandboxes == nil {
		sandboxes = []SandboxInfo{}
	}
	json.NewEncoder(w).Encode(sandboxes)
}

// --- DELETE /api/admin/sandboxes/{ns} ---

func (s *Server) handleAdminSandboxDelete(w http.ResponseWriter, r *http.Request) {
	ns := strings.TrimPrefix(chi.URLParam(r, "*"), "/")
	if s.sandbox == nil {
		jsonError(w, "sandbox manager not available", 503)
		return
	}
	ok := s.sandbox.TeardownSandbox(ns)
	status := "deleted"
	if !ok {
		status = "refused"
	}
	json.NewEncoder(w).Encode(map[string]string{"status": status})
}

// --- POST /api/admin/sandboxes/cleanup ---

func (s *Server) handleAdminSandboxCleanup(w http.ResponseWriter, r *http.Request) {
	if s.sandbox == nil {
		json.NewEncoder(w).Encode(map[string]int{"cleaned": 0})
		return
	}
	count := s.sandbox.AutoCleanup(1800)
	json.NewEncoder(w).Encode(map[string]int{"cleaned": count})
}

// --- repo profile helpers ---

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
	// TODO: update K8s ConfigMap
	return nil
}

// --- GitHub file reading for deployment analysis ---

func fetchRepoDeployFiles(repo string) map[string]string {
	files := make(map[string]string)
	paths := []string{"Dockerfile", "docker-compose.yml", "Makefile", "requirements.txt", "package.json", "go.mod"}

	// Check deploy directories
	for _, dir := range []string{"k8s", "manifests", "deploy", "helm", "config", "kustomize"} {
		body, code, _ := ghGetDashboard(fmt.Sprintf("/repos/%s/contents/%s", repo, dir))
		if code == 200 {
			var items []struct {
				Type string `json:"type"`
				Path string `json:"path"`
			}
			json.Unmarshal(body, &items)
			for _, item := range items {
				if item.Type == "file" {
					paths = append(paths, item.Path)
				}
			}
		}
	}

	for i, path := range paths {
		if i >= 20 {
			break
		}
		body, code, _ := ghGetDashboard(fmt.Sprintf("/repos/%s/contents/%s", repo, path))
		if code == 200 {
			var file struct {
				Content string `json:"content"`
			}
			json.Unmarshal(body, &file)
			if file.Content != "" {
					decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
				if err == nil {
					c := string(decoded)
					if len(c) > 5000 {
						c = c[:5000]
					}
					files[path] = c
				}
			}
		}
	}
	return files
}

func fetchRepoReadme(repo string) string {
	for _, name := range []string{"README.md", "readme.md", "README.rst"} {
		body, code, _ := ghGetDashboard(fmt.Sprintf("/repos/%s/contents/%s", repo, name))
		if code == 200 {
			var file struct {
				Content string `json:"content"`
			}
			json.Unmarshal(body, &file)
			if file.Content != "" {
					decoded, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
				if err == nil {
					c := string(decoded)
					if len(c) > 3000 {
						c = c[:3000]
					}
					return c
				}
			}
		}
	}
	return ""
}

func readClusterState() map[string][]string {
	result := map[string][]string{
		"crds":       {},
		"operators":  {},
		"namespaces": {},
	}

	// Try to read CRDs from the cluster via GitHub API-style K8s call
	// We use the same ghGetDashboard pattern but targeting the K8s API.
	// Since the dashboard doesn't hold a K8s client directly, we use the
	// discovery API via a lightweight HTTP call to the in-cluster API server.
	k8sHost := os.Getenv("KUBERNETES_SERVICE_HOST")
	k8sPort := os.Getenv("KUBERNETES_SERVICE_PORT")
	if k8sHost == "" {
		return result // not running in-cluster
	}

	tokenBytes, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/token")
	if err != nil {
		return result
	}
	token := strings.TrimSpace(string(tokenBytes))
	baseURL := fmt.Sprintf("https://%s:%s", k8sHost, k8sPort)

	k8sGet := func(path string) ([]byte, error) {
		req, _ := http.NewRequest("GET", baseURL+path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		// Skip TLS verify for in-cluster API (CA cert handling is complex)
		client := &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
			TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
		}}
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		return io.ReadAll(resp.Body)
	}

	// List CRDs
	if body, err := k8sGet("/apis/apiextensions.k8s.io/v1/customresourcedefinitions"); err == nil {
		var crdList struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
				Spec struct {
					Group string `json:"group"`
					Names struct {
						Kind string `json:"kind"`
					} `json:"names"`
				} `json:"spec"`
			} `json:"items"`
		}
		if json.Unmarshal(body, &crdList) == nil {
			for _, crd := range crdList.Items {
				result["crds"] = append(result["crds"], crd.Spec.Names.Kind+" ("+crd.Spec.Group+")")
			}
		}
	}

	// List OLM operator subscriptions (optional — only if OLM is installed)
	if body, err := k8sGet("/apis/operators.coreos.com/v1alpha1/subscriptions"); err == nil {
		var subList struct {
			Items []struct {
				Metadata struct {
					Name      string `json:"name"`
					Namespace string `json:"namespace"`
				} `json:"metadata"`
				Spec struct {
					Name string `json:"name"`
				} `json:"spec"`
			} `json:"items"`
		}
		if json.Unmarshal(body, &subList) == nil {
			for _, sub := range subList.Items {
				result["operators"] = append(result["operators"], sub.Spec.Name+" ("+sub.Metadata.Namespace+")")
			}
		}
	}

	// List namespaces
	if body, err := k8sGet("/api/v1/namespaces"); err == nil {
		var nsList struct {
			Items []struct {
				Metadata struct {
					Name string `json:"name"`
				} `json:"metadata"`
			} `json:"items"`
		}
		if json.Unmarshal(body, &nsList) == nil {
			for _, ns := range nsList.Items {
				result["namespaces"] = append(result["namespaces"], ns.Metadata.Name)
			}
		}
	}

	slog.Info("read cluster state", "crds", len(result["crds"]), "operators", len(result["operators"]), "namespaces", len(result["namespaces"]))
	return result
}

func autoUpdateProfileFromPlan(repo string, planData map[string]any) {
	projectType, _ := planData["project_type"].(string)
	depsRaw, _ := planData["dependencies"].([]any)
	var depsList []string
	for _, d := range depsRaw {
		if s, ok := d.(string); ok {
			depsList = append(depsList, s)
		}
	}
	depsStr := strings.Join(depsList, ", ")
	depsLower := strings.ToLower(depsStr)

	k8sKw := []string{"kubernetes", "k8s", "openshift", "operator", "istio", "kuadrant", "helm", "kustomize"}
	gpuKw := []string{"gpu", "cuda", "nvidia"}
	needsK8s := false
	needsGPU := false
	for _, kw := range k8sKw {
		if strings.Contains(depsLower, kw) {
			needsK8s = true
			break
		}
	}
	for _, kw := range gpuKw {
		if strings.Contains(depsLower, kw) {
			needsGPU = true
			break
		}
	}

	newProfile := map[string]any{
		"type": projectType, "build": "", "run": "", "health": "",
		"deps": depsStr, "k8s": needsK8s, "gpu": needsGPU,
	}
	existing := config.LoadRepoProfile(repo)
	for _, k := range []string{"build", "run", "health"} {
		if v, ok := existing[k]; ok && v != "" {
			newProfile[k] = v
		}
	}
	if t, ok := existing["type"]; ok && t != "" && t != "other" {
		newProfile["type"] = t
	}
	updateRepoEnv(repo, newProfile, false)

	// Store all analysis fields in repo_memory for AI context during reproduction
	memFields := map[string]string{}

	// Direct string fields from AI response
	directFields := map[string]string{
		"description":        "description",
		"detected_deployment_method": "deployment_type",
		"install_command":    "install_command",
		"install_notes":      "install_notes",
		"how_to_test":        "how_to_test",
		"build_command":      "build_command",
		"run_command":         "run_command",
		"health_endpoint":    "health_endpoint",
	}
	for aiKey, memKey := range directFields {
		if v, _ := planData[aiKey].(string); v != "" && v != "none" {
			memFields[memKey] = v
		}
	}

	// Derived fields
	if projectType != "" {
		memFields["tech_stack"] = projectType
	}
	if depsStr != "" {
		memFields["tech_stack"] = depsStr
	}
	if needsK8s {
		memFields["needs_cluster"] = "true"
		if memFields["test_strategy"] == "" {
			memFields["test_strategy"] = "code-review"
		}
	} else {
		memFields["needs_cluster"] = "false"
		if memFields["test_strategy"] == "" {
			memFields["test_strategy"] = "deploy-and-curl"
		}
	}

	for k, v := range memFields {
		if v != "" {
			database.SetRepoMemory(repo, k, v)
		}
	}
	slog.Info("stored AI knowledge from deployment analysis", "repo", repo, "fields", len(memFields))
}

func ghGetDashboard(path string) ([]byte, int, error) {
	token := os.Getenv("GITHUB_TOKEN")
	req, _ := http.NewRequest("GET", "https://api.github.com"+path, nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	return body, resp.StatusCode, nil
}
