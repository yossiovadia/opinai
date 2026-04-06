package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/config"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

// buildChatContext builds the system prompt with issue context, repo memory, and previous runs.
func buildChatContext(ctx map[string]any) string {
	system := prompts.Render("chat_system.txt", nil) + "\n\n"

	repo, _ := ctx["repo"].(string)
	issueNum := 0
	if n, ok := ctx["issue_number"].(float64); ok {
		issueNum = int(n)
	}

	if repo != "" && issueNum > 0 {
		system += fmt.Sprintf("Current issue: %s#%d\n", repo, issueNum)

		runs, _ := database.GetRuns(repo, 3)
		for _, run := range runs {
			if run.Issue == issueNum {
				report := run.Report
				if len(report) > 1000 {
					report = report[:1000]
				}
				system += fmt.Sprintf("Previous reproduction result:\n%s\n\n", report)
				if run.ReproDetails != "" {
					// Extract key fields from repro_details for richer chat context
					var details map[string]any
					if json.Unmarshal([]byte(run.ReproDetails), &details) == nil {
						if summary, ok := details["summary"].(string); ok && summary != "" {
							s := summary
							if len(s) > 1500 {
								s = s[:1500]
							}
							system += fmt.Sprintf("Investigation summary:\n%s\n\n", s)
						}
						if files, ok := details["files_investigated"].([]any); ok && len(files) > 0 {
							system += "Files investigated:"
							for _, f := range files {
								if fs, ok := f.(string); ok {
									system += " " + fs
								}
							}
							system += "\n\n"
						}
						if reason, ok := details["classification_reason"].(string); ok && reason != "" {
							system += fmt.Sprintf("Classification: %s\n", reason)
						}
						if failReason, ok := details["deployment_failure_reason"].(string); ok && failReason != "" {
							system += fmt.Sprintf("Deployment failure: %s\n", failReason)
						}
					}
					system += fmt.Sprintf("Reproduction details: %s\n\n", run.ReproDetails)
				}
				break
			}
		}

		mem, _ := database.GetRepoMemory(repo, nil)
		if len(mem) > 0 {
			system += "Project knowledge:\n"
			for k, v := range mem {
				system += fmt.Sprintf("- %s: %s\n", k, v)
			}
			system += "\n"
		}

		profile := config.LoadRepoProfile(repo)
		if len(profile) > 0 {
			b, _ := json.Marshal(profile)
			system += "Project profile: " + string(b) + "\n\n"
		}
	} else {
		stats, _ := database.GetTotalStats()
		repos := ParseRepos(envGet("REPOS"))
		system += fmt.Sprintf("Monitored repos: %s\n", strings.Join(repos, ", "))
		system += fmt.Sprintf("Total runs: %d\n", stats.TotalRuns)
		system += fmt.Sprintf("Pending issues: check dashboard for current counts\n\n")
	}

	return system
}

func extractChatIssue(ctx map[string]any) (string, int) {
	repo, _ := ctx["repo"].(string)
	issue := 0
	if n, ok := ctx["issue_number"].(float64); ok {
		issue = int(n)
	}
	return repo, issue
}

// handleChatFull handles the non-streaming /api/chat endpoint with persistence.
func (s *Server) handleChatFull(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string         `json:"message"`
		Context map[string]any `json:"context"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Message == "" {
		jsonError(w, "message required", 400)
		return
	}

	repo, issue := extractChatIssue(req.Context)

	// Save user message
	if repo != "" && issue > 0 {
		database.AddChatMessage(repo, issue, "user", req.Message)
	}

	system := buildChatContext(req.Context)
	reply, err := ai.Call(system+"\n\nUser question: "+req.Message, 2048)
	if err != nil {
		reply = "AI error: " + err.Error()
	}
	reply = ai.Sanitize(reply)

	// Save AI response
	if repo != "" && issue > 0 {
		database.AddChatMessage(repo, issue, "ai", reply)
	}

	json.NewEncoder(w).Encode(map[string]string{"reply": reply})
}

// handleChatHistory returns saved chat messages for a repo+issue.
func (s *Server) handleChatHistory(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	issueStr := r.URL.Query().Get("issue")
	if repo == "" || issueStr == "" {
		jsonError(w, "repo and issue required", 400)
		return
	}
	issue, _ := strconv.Atoi(issueStr)

	msgs, err := database.GetChatHistory(repo, issue)
	if err != nil {
		jsonError(w, err.Error(), 500)
		return
	}
	if msgs == nil {
		msgs = []database.ChatMessage{}
	}
	json.NewEncoder(w).Encode(msgs)
}

// handleClearChatHistory deletes chat history for a repo+issue.
func (s *Server) handleClearChatHistory(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Repo  string `json:"repo"`
		Issue int    `json:"issue"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Repo == "" || req.Issue == 0 {
		jsonError(w, "repo and issue required", 400)
		return
	}
	database.ClearChatHistory(req.Repo, req.Issue)
	json.NewEncoder(w).Encode(map[string]string{"status": "cleared"})
}

func envGet(key string) string {
	return Env(key, "")
}
