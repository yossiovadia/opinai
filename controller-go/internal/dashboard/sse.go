package dashboard

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	k8sCorev1 "k8s.io/api/core/v1"
	k8sMetav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sKubernetes "k8s.io/client-go/kubernetes"
	k8sRest "k8s.io/client-go/rest"
	k8sClientcmd "k8s.io/client-go/tools/clientcmd"

	"github.com/yossiovadia/opinai/controller-go/internal/agent"
	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/config"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

// writeSSE writes a single SSE event and flushes.
func writeSSE(w http.ResponseWriter, event string, data any) {
	b, _ := json.Marshal(data)
	fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event, b)
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}
}

func sseHeaders(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("Connection", "keep-alive")
}

// --- GET /api/admin/analyze-stream?repo=X ---

func (s *Server) handleAnalyzeStream(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	if repo == "" {
		http.Error(w, `{"error":"repo required"}`, 400)
		return
	}
	sseHeaders(w)

	// Stage 1: Clone repo for deep analysis
	writeSSE(w, "progress", map[string]string{
		"stage": "cloning", "message": "Cloning repository for deep analysis...",
	})

	cloneDir, err := cloneRepoForAnalysis(repo)
	if err != nil {
		slog.Warn("agent analysis: clone failed, falling back to shallow analysis", "error", err)
		s.handleAnalyzeStreamFallback(w, r, repo)
		return
	}
	defer os.RemoveAll(cloneDir)

	// Stage 2: Agent-based deep analysis
	writeSSE(w, "progress", map[string]string{
		"stage": "analyzing", "message": "AI agent reading code and building project understanding...",
	})

	type analyzeResult struct {
		analysis agent.RepoAnalysis
		err      error
	}
	done := make(chan analyzeResult, 1)
	go func() {
		a, err := agent.AnalyzeRepo(cloneDir, repo, 15)
		done <- analyzeResult{a, err}
	}()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	start := time.Now()
	var analysis agent.RepoAnalysis

waitLoop:
	for {
		select {
		case res := <-done:
			analysis, err = res.analysis, res.err
			break waitLoop
		case <-ticker.C:
			elapsed := int(time.Since(start).Seconds())
			writeSSE(w, "progress", map[string]string{
				"stage":   "analyzing",
				"message": fmt.Sprintf("AI agent analyzing code... (%ds elapsed, reading files)", elapsed),
			})
		case <-r.Context().Done():
			return
		}
	}

	if err != nil {
		slog.Warn("agent analysis failed, falling back to shallow analysis", "error", err)
		s.handleAnalyzeStreamFallback(w, r, repo)
		return
	}

	// Stage 3: Save to repo_memory
	writeSSE(w, "progress", map[string]string{
		"stage": "saving", "message": "Saving analysis results...",
	})

	flatMap := analysis.ToFlatMap()
	for k, v := range flatMap {
		if v != "" {
			database.SetRepoMemory(repo, k, v)
		}
	}

	// Also run the deployment plan analysis for backward compat
	writeSSE(w, "progress", map[string]string{
		"stage": "deployment_plan", "message": "Generating deployment options...",
	})

	files := fetchRepoDeployFiles(repo)
	readme := fetchRepoReadme(repo)
	clusterState := readClusterState()
	profile := config.LoadRepoProfile(repo)
	profileJSON, _ := json.Marshal(profile)

	planData, planErr := ai.AnalyzeDeployment(repo, readme, files, clusterState, string(profileJSON))
	if planErr == nil && planData != nil {
		planBytes, _ := json.Marshal(planData)
		database.SaveDeploymentPlan(repo, string(planBytes))
		autoUpdateProfileFromPlan(repo, planData)
	}

	opts := 0
	if planData != nil {
		if o, ok := planData["options"].([]any); ok {
			opts = len(o)
		}
	}

	writeSSE(w, "done", map[string]string{
		"message": fmt.Sprintf("Deep analysis complete — %d endpoints found, %d deployment options, %d tool calls",
			len(analysis.APISurface.Endpoints), opts, analysis.ToolCalls),
	})
}

// handleAnalyzeStreamFallback is the original shallow analysis (README + deps only).
func (s *Server) handleAnalyzeStreamFallback(w http.ResponseWriter, r *http.Request, repo string) {
	writeSSE(w, "progress", map[string]string{
		"stage": "reading_repo", "message": "Reading repository files (fallback)...",
	})

	files := fetchRepoDeployFiles(repo)
	readme := fetchRepoReadme(repo)

	writeSSE(w, "progress", map[string]string{
		"stage": "reading_cluster", "message": "Scanning cluster operators and CRDs...",
	})
	clusterState := readClusterState()

	writeSSE(w, "progress", map[string]string{
		"stage": "calling_ai", "message": "AI analyzing deployment options...",
	})

	profile := config.LoadRepoProfile(repo)
	profileJSON, _ := json.Marshal(profile)

	type aiResult struct {
		data map[string]any
		err  error
	}
	aiDone := make(chan aiResult, 1)
	go func() {
		data, err := ai.AnalyzeDeployment(repo, readme, files, clusterState, string(profileJSON))
		aiDone <- aiResult{data, err}
	}()

	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	aiStart := time.Now()
	var planData map[string]any
	var err error

fallbackWait:
	for {
		select {
		case res := <-aiDone:
			planData, err = res.data, res.err
			break fallbackWait
		case <-ticker.C:
			elapsed := int(time.Since(aiStart).Seconds())
			writeSSE(w, "progress", map[string]string{
				"stage":   "calling_ai",
				"message": fmt.Sprintf("AI analyzing project... (%ds elapsed)", elapsed),
			})
		case <-r.Context().Done():
			return
		}
	}

	if err != nil {
		writeSSE(w, "error", map[string]string{"message": "Analysis failed: " + err.Error()})
		return
	}

	writeSSE(w, "progress", map[string]string{
		"stage": "saving", "message": "Saving deployment plan...",
	})

	planBytes, _ := json.Marshal(planData)
	database.SaveDeploymentPlan(repo, string(planBytes))
	autoUpdateProfileFromPlan(repo, planData)

	opts, _ := planData["options"].([]any)
	writeSSE(w, "done", map[string]string{
		"message": fmt.Sprintf("Analysis complete — %d options generated", len(opts)),
	})
}

// cloneRepoForAnalysis clones a repo to a temp directory for agent analysis.
func cloneRepoForAnalysis(repo string) (string, error) {
	dir, err := os.MkdirTemp("", "opinai-analyze-*")
	if err != nil {
		return "", fmt.Errorf("create temp dir: %w", err)
	}

	cloneURL := "https://github.com/" + repo + ".git"
	token := os.Getenv("GITHUB_TOKEN")
	if token != "" {
		cloneURL = "https://x-access-token:" + token + "@github.com/" + repo + ".git"
	}

	cloneCtx, cloneCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cloneCancel()
	cmd := exec.CommandContext(cloneCtx, "git", "clone", "--depth=1", cloneURL, dir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.RemoveAll(dir)
		return "", fmt.Errorf("git clone: %s: %w", strings.TrimSpace(string(out)), err)
	}

	return dir, nil
}

// --- GET /api/reproduce-stream?repo=X&issue=N ---

func (s *Server) handleReproduceStream(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	issueStr := r.URL.Query().Get("issue")
	if repo == "" || issueStr == "" {
		http.Error(w, `{"error":"repo and issue required"}`, 400)
		return
	}
	var issue int
	fmt.Sscanf(issueStr, "%d", &issue)

	sseHeaders(w)

	writeSSE(w, "progress", map[string]any{
		"stage": "creating_job", "message": fmt.Sprintf("Creating reproduction job for %s#%d...", repo, issue),
	})

	if s.reproduce == nil {
		writeSSE(w, "error", map[string]string{"message": "Controller not ready"})
		return
	}

	// Record timestamp before job creation so we only match runs created after this point.
	jobCreatedAfter := time.Now().UTC().Format("2006-01-02T15:04:05Z")

	database.AddPending(repo, issue, "")
	if err := s.reproduce(repo, issue); err != nil {
		writeSSE(w, "error", map[string]string{"message": "Failed to create job: " + err.Error()})
		return
	}

	writeSSE(w, "progress", map[string]any{
		"stage": "job_created", "message": "Job created. Waiting for pod to start...",
	})

	prevLogLen := 0
	repoSafe := strings.ToLower(strings.ReplaceAll(repo, "/", "-"))
	jobName := fmt.Sprintf("opinai-%s-%d", repoSafe, issue)

	for i := 0; i < 120; i++ { // 10 minutes
		time.Sleep(5 * time.Second)

		// Check if a NEW run appeared in DB (created after we started the job)
		runs, _ := database.GetRunsByIssue(repo, issue)
		for _, run := range runs {
			if run.CreatedAt >= jobCreatedAfter {
				writeSSE(w, "done", map[string]string{
					"message": fmt.Sprintf("Reproduction complete for %s#%d — %s", repo, issue, run.Verdict),
				})
				return
			}
		}

		// Stream a progress heartbeat
		writeSSE(w, "progress", map[string]any{
			"stage": "job_running", "message": fmt.Sprintf("Running... (%ds)", (i+1)*5),
			"job":   jobName,
		})
		_ = prevLogLen // future: stream actual pod logs
	}

	writeSSE(w, "error", map[string]string{"message": "Timed out waiting for job"})
}

// --- POST /api/chat-stream ---

func (s *Server) handleChatStream(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string         `json:"message"`
		Context map[string]any `json:"context"`
	}
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()
	if err := json.Unmarshal(body, &req); err != nil || req.Message == "" {
		http.Error(w, `{"error":"message required"}`, 400)
		return
	}

	sseHeaders(w)

	repo, issue := extractChatIssue(req.Context)

	// Save user message
	if repo != "" && issue > 0 {
		database.AddChatMessage(repo, issue, "user", req.Message)
	}

	systemCtx := buildChatContext(req.Context)
	prompt := systemCtx + "\n\nUser question: " + req.Message

	cfg := ai.LoadConfig()
	if !cfg.Available() {
		writeSSE(w, "error", map[string]string{"message": "No AI provider configured"})
		return
	}

	var fullReply strings.Builder

	streamResp, err := doStreamingAIRequest(cfg, prompt)
	if err != nil {
		reply, err2 := ai.Call(prompt, 2048)
		if err2 != nil {
			writeSSE(w, "error", map[string]string{"message": "Chat failed: " + err2.Error()})
			return
		}
		reply = ai.Sanitize(reply)
		fullReply.WriteString(reply)
		writeSSE(w, "chunk", map[string]string{"text": reply})
		writeSSE(w, "done", map[string]string{"message": ""})
	} else {
		defer streamResp.Body.Close()
		buf := make([]byte, 4096)
		var leftover string
		for {
			n, err := streamResp.Body.Read(buf)
			if n > 0 {
				chunk := leftover + string(buf[:n])
				leftover = ""
				lines := strings.Split(chunk, "\n")
				if !strings.HasSuffix(chunk, "\n") {
					leftover = lines[len(lines)-1]
					lines = lines[:len(lines)-1]
				}
				for _, line := range lines {
					if !strings.HasPrefix(line, "data: ") {
						continue
					}
					data := line[6:]
					if strings.TrimSpace(data) == "[DONE]" {
						break
					}
					text := extractStreamText(cfg, data)
					if text != "" {
						fullReply.WriteString(text)
						writeSSE(w, "chunk", map[string]string{"text": text})
					}
				}
			}
			if err != nil {
				break
			}
		}
		writeSSE(w, "done", map[string]string{"message": ""})
	}

	// Save AI response
	if repo != "" && issue > 0 && fullReply.Len() > 0 {
		database.AddChatMessage(repo, issue, "ai", ai.Sanitize(fullReply.String()))
	}
}

// --- GET /api/check-now-stream ---

func (s *Server) handleCheckNowStream(w http.ResponseWriter, r *http.Request) {
	sseHeaders(w)

	repos := ParseRepos(os.Getenv("REPOS"))
	if len(repos) == 0 {
		writeSSE(w, "done", map[string]any{"message": "No repos configured", "total": 0})
		return
	}

	ghToken := os.Getenv("GITHUB_TOKEN")
	totalNew := 0

	for _, repo := range repos {
		writeSSE(w, "progress", map[string]any{
			"stage": "checking", "message": fmt.Sprintf("Checking %s...", repo), "repo": repo,
		})

		url := fmt.Sprintf("https://api.github.com/repos/%s/issues?state=open&per_page=100", repo)
		req, _ := http.NewRequest("GET", url, nil)
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+ghToken)
		client := &http.Client{Timeout: 30 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			writeSSE(w, "progress", map[string]any{
				"stage": "error", "message": fmt.Sprintf("%s: %s", repo, err), "repo": repo,
			})
			continue
		}
		respBody, _ := io.ReadAll(resp.Body)
		resp.Body.Close()

		if resp.StatusCode != 200 {
			writeSSE(w, "progress", map[string]any{
				"stage": "error", "message": fmt.Sprintf("%s: HTTP %d", repo, resp.StatusCode), "repo": repo,
			})
			continue
		}

		var issues []struct {
			PullRequest *struct{} `json:"pull_request,omitempty"`
		}
		json.Unmarshal(respBody, &issues)
		count := 0
		for _, i := range issues {
			if i.PullRequest == nil {
				count++
			}
		}
		totalNew += count

		writeSSE(w, "progress", map[string]any{
			"stage": "found", "message": fmt.Sprintf("%s: %d open issues", repo, count),
			"repo": repo, "count": count,
		})
	}

	writeSSE(w, "done", map[string]any{
		"message": fmt.Sprintf("Found %d total open issues across %d repos", totalNew, len(repos)),
		"total":   totalNew,
	})
}

// --- GET /api/job-logs?repo=X&issue=N ---

func (s *Server) handleJobLogs(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	issueStr := r.URL.Query().Get("issue")
	if repo == "" || issueStr == "" {
		http.Error(w, `{"error":"repo and issue required"}`, 400)
		return
	}

	repoSafe := strings.ToLower(strings.ReplaceAll(repo, "/", "-"))
	jobName := fmt.Sprintf("opinai-%s-%s", repoSafe, issueStr)
	namespace := os.Getenv("NAMESPACE")
	if namespace == "" {
		namespace = "opinai"
	}

	sseHeaders(w)
	writeSSE(w, "progress", map[string]string{"message": "Waiting for pod..."})

	// Get K8s client
	k8sClient, err := getK8sClient()
	if err != nil {
		writeSSE(w, "error", map[string]string{"message": "K8s client unavailable: " + err.Error()})
		return
	}

	// Wait for pod to be ready for log streaming (up to 60s)
	ctx := r.Context()
	var podName string
	podReady := false
	for i := 0; i < 30; i++ {
		pods, err := k8sClient.CoreV1().Pods(namespace).List(ctx, k8sMetav1.ListOptions{
			LabelSelector: "job-name=" + jobName,
		})
		if err == nil && len(pods.Items) > 0 {
			podName = pods.Items[0].Name
			phase := string(pods.Items[0].Status.Phase)
			if phase == "Running" || phase == "Succeeded" || phase == "Failed" {
				podReady = true
				writeSSE(w, "progress", map[string]string{"message": "Pod " + phase + " — streaming logs..."})
				break
			}
			writeSSE(w, "progress", map[string]string{"message": "Pod " + phase + "..."})
		} else {
			writeSSE(w, "progress", map[string]string{
				"message": fmt.Sprintf("Waiting for pod... (%ds)", (i+1)*2),
			})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(2 * time.Second):
		}
	}

	if podName == "" || !podReady {
		msg := "No pod found for job " + jobName
		if podName != "" {
			msg = "Pod " + podName + " did not start within 60s"
		}
		writeSSE(w, "error", map[string]string{"message": msg})
		return
	}

	writeSSE(w, "progress", map[string]string{"message": "Streaming logs from " + podName + "..."})

	// Stream logs with Follow
	follow := true
	logReq := k8sClient.CoreV1().Pods(namespace).GetLogs(podName, &k8sCorev1.PodLogOptions{
		Follow: follow,
	})
	stream, err := logReq.Stream(ctx)
	if err != nil {
		writeSSE(w, "error", map[string]string{"message": "Log stream failed: " + err.Error()})
		return
	}
	defer stream.Close()

	buf := make([]byte, 4096)
	for {
		n, err := stream.Read(buf)
		if n > 0 {
			lines := strings.Split(string(buf[:n]), "\n")
			for _, line := range lines {
				if line != "" {
					writeSSE(w, "log", map[string]string{"line": line})
				}
			}
		}
		if err != nil {
			break
		}
	}

	writeSSE(w, "done", map[string]string{"message": "Log stream ended"})
}

// lazy K8s client for SSE handlers
var (
	_sseK8sClient k8sKubernetes.Interface
	_sseK8sOnce   sync.Once
	_sseK8sErr    error
)

func getK8sClient() (k8sKubernetes.Interface, error) {
	_sseK8sOnce.Do(func() {
		config, err := k8sRest.InClusterConfig()
		if err != nil {
			home, _ := os.UserHomeDir()
			config, err = k8sClientcmd.BuildConfigFromFlags("", home+"/.kube/config")
			if err != nil {
				_sseK8sErr = err
				return
			}
		}
		_sseK8sClient, _sseK8sErr = k8sKubernetes.NewForConfig(config)
	})
	return _sseK8sClient, _sseK8sErr
}

// --- streaming AI helpers ---

func doStreamingAIRequest(cfg ai.Config, prompt string) (*http.Response, error) {
	messages := []ai.Message{{Role: "user", Content: prompt}}

	var url string
	headers := map[string]string{"Content-Type": "application/json"}
	payload := map[string]any{
		"messages":   messages,
		"max_tokens": 2048,
		"stream":     true,
	}

	switch cfg.Provider {
	case ai.ProviderVertex:
		ctx := context.Background()
		creds, err := findGoogleCreds(ctx)
		if err != nil {
			return nil, err
		}
		token, err := creds.Token()
		if err != nil {
			return nil, err
		}
		url = fmt.Sprintf(
			"https://%s-aiplatform.googleapis.com/v1/projects/%s/locations/%s/publishers/anthropic/models/%s:streamRawPredict",
			cfg.Region, cfg.Project, cfg.Region, cfg.Model,
		)
		headers["Authorization"] = "Bearer " + token.AccessToken
		payload["anthropic_version"] = "vertex-2023-10-16"

	case ai.ProviderOpenAI:
		url = cfg.BaseURL + "/v1/chat/completions"
		headers["Authorization"] = "Bearer " + cfg.APIKey
		payload["model"] = cfg.Model

	default: // Anthropic
		url = cfg.BaseURL + "/v1/messages"
		headers["x-api-key"] = cfg.APIKey
		headers["anthropic-version"] = "2023-06-01"
		payload["model"] = cfg.Model
	}

	data, _ := json.Marshal(payload)
	req, err := http.NewRequest("POST", url, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}

	client := &http.Client{Timeout: 120 * time.Second}
	return client.Do(req)
}

func findGoogleCreds(ctx context.Context) (interface{ Token() (*tokenResult, error) }, error) {
	// Simplified: use oauth2/google
	return nil, fmt.Errorf("vertex streaming requires google-auth setup")
}

type tokenResult struct {
	AccessToken string
}

func extractStreamText(cfg ai.Config, data string) string {
	var chunk map[string]any
	if err := json.Unmarshal([]byte(data), &chunk); err != nil {
		return ""
	}

	switch cfg.Provider {
	case ai.ProviderOpenAI:
		choices, _ := chunk["choices"].([]any)
		if len(choices) > 0 {
			choice, _ := choices[0].(map[string]any)
			delta, _ := choice["delta"].(map[string]any)
			text, _ := delta["content"].(string)
			return text
		}
	default: // Anthropic / Vertex
		if chunk["type"] == "content_block_delta" {
			delta, _ := chunk["delta"].(map[string]any)
			text, _ := delta["text"].(string)
			return text
		}
	}
	return ""
}

