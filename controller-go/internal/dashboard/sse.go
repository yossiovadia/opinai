package dashboard

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
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

	// Stage 1: Reading repo
	writeSSE(w, "progress", map[string]string{
		"stage": "reading_repo", "message": "Reading repository files...",
	})

	files := fetchRepoDeployFiles(repo)
	readme := fetchRepoReadme(repo)

	// Stage 2: Reading cluster
	writeSSE(w, "progress", map[string]string{
		"stage": "reading_cluster", "message": "Scanning cluster operators and CRDs...",
	})

	clusterState := readClusterState()

	// Stage 3: Calling AI
	writeSSE(w, "progress", map[string]string{
		"stage": "calling_ai", "message": "AI analyzing deployment options (30-60s)...",
	})

	profile := loadProfile(repo)
	profileJSON, _ := json.Marshal(profile)

	planData, err := ai.AnalyzeDeployment(repo, readme, files, clusterState, string(profileJSON))
	if err != nil {
		writeSSE(w, "error", map[string]string{"message": "Analysis failed: " + err.Error()})
		return
	}

	// Stage 4: Saving
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
	if err := s.reproduce(repo, issue); err != nil {
		writeSSE(w, "error", map[string]string{"message": "Failed to create job: " + err.Error()})
		return
	}

	writeSSE(w, "progress", map[string]any{
		"stage": "job_created", "message": "Job created. Waiting for pod to start...",
	})

	// Poll job status via K8s — use the same approach: fetch /api/jobs periodically
	// Since we're inside the dashboard process, we can call the handler's data source.
	// For simplicity, poll our own /api/jobs endpoint or directly check DB.
	prevLogLen := 0
	repoSafe := strings.ToLower(strings.ReplaceAll(repo, "/", "-"))
	jobName := fmt.Sprintf("opinai-%s-%d", repoSafe, issue)

	for i := 0; i < 120; i++ { // 10 minutes
		time.Sleep(5 * time.Second)

		// Check if run appeared in DB (means job completed and was harvested)
		runs, _ := database.GetRuns(repo, 5)
		for _, run := range runs {
			if run.Issue == issue {
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

	systemCtx := "You are OpinAI, an AI bug reproduction assistant running on a Kubernetes cluster. " +
		"You help developers understand bugs, analyze reproduction results, and suggest fixes. " +
		"Be concise, technical, and helpful. Use markdown formatting.\n\n"

	if repo, ok := req.Context["repo"].(string); ok && repo != "" {
		if issueNum, ok := req.Context["issue_number"].(float64); ok && issueNum > 0 {
			systemCtx += fmt.Sprintf("Current issue: %s#%d\n", repo, int(issueNum))
		}
	}

	prompt := systemCtx + "\n\nUser question: " + req.Message

	// Try streaming API call
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		writeSSE(w, "error", map[string]string{"message": "No AI provider configured"})
		return
	}

	// Build request for streaming
	streamResp, err := doStreamingAIRequest(cfg, prompt)
	if err != nil {
		// Fallback to non-streaming
		reply, err2 := ai.Call(prompt, 2048)
		if err2 != nil {
			writeSSE(w, "error", map[string]string{"message": "Chat failed: " + err2.Error()})
			return
		}
		writeSSE(w, "chunk", map[string]string{"text": ai.Sanitize(reply)})
		writeSSE(w, "done", map[string]string{"message": ""})
		return
	}
	defer streamResp.Body.Close()

	// Parse SSE stream from AI provider
	buf := make([]byte, 4096)
	var leftover string
	for {
		n, err := streamResp.Body.Read(buf)
		if n > 0 {
			chunk := leftover + string(buf[:n])
			leftover = ""
			lines := strings.Split(chunk, "\n")
			// Last element might be incomplete
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

// Suppress unused import warnings
func init() {
	_ = base64.StdEncoding
	_ = slog.Default()
}
