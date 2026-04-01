package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

// buildChatContext builds the system prompt with issue context, repo memory, and previous runs.
func buildChatContext(ctx map[string]any) string {
	system := "You are OpinAI, an AI bug reproduction assistant running on a Kubernetes cluster. " +
		"You help developers understand bugs, analyze reproduction results, and suggest fixes. " +
		"Be concise, technical, and helpful. Use markdown formatting.\n\n"

	repo, _ := ctx["repo"].(string)
	issueNum := 0
	if n, ok := ctx["issue_number"].(float64); ok {
		issueNum = int(n)
	}

	if repo != "" && issueNum > 0 {
		system += fmt.Sprintf("Current issue: %s#%d\n", repo, issueNum)

		// Include previous runs
		runs, _ := database.GetRuns(repo, 3)
		for _, run := range runs {
			if run.Issue == issueNum {
				report := run.Report
				if len(report) > 1000 {
					report = report[:1000]
				}
				system += fmt.Sprintf("Previous reproduction result:\n%s\n\n", report)
				break
			}
		}

		// Include repo memory
		mem, _ := database.GetRepoMemory(repo, nil)
		if len(mem) > 0 {
			system += "Project knowledge:\n"
			for k, v := range mem {
				system += fmt.Sprintf("- %s: %s\n", k, v)
			}
			system += "\n"
		}

		// Include repo profile
		profile := loadProfile(repo)
		if len(profile) > 0 {
			b, _ := json.Marshal(profile)
			system += "Project profile: " + string(b) + "\n\n"
		}
	} else {
		// General context
		stats, _ := database.GetTotalStats()
		repos := ParseRepos(envGet("REPOS"))
		system += fmt.Sprintf("Monitored repos: %s\n", strings.Join(repos, ", "))
		system += fmt.Sprintf("Total runs: %d\n", stats.TotalRuns)
		system += fmt.Sprintf("Pending issues: check dashboard for current counts\n\n")
	}

	return system
}

// handleChatFull handles the non-streaming /api/chat endpoint.
func (s *Server) handleChatFull(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Message string         `json:"message"`
		Context map[string]any `json:"context"`
	}
	if err := decodeJSON(r, &req); err != nil || req.Message == "" {
		jsonError(w, "message required", 400)
		return
	}

	system := buildChatContext(req.Context)
	reply, err := ai.Call(system+"\n\nUser question: "+req.Message, 2048)
	if err != nil {
		json.NewEncoder(w).Encode(map[string]string{"reply": "AI error: " + err.Error()})
		return
	}
	json.NewEncoder(w).Encode(map[string]string{"reply": ai.Sanitize(reply)})
}

func envGet(key string) string {
	return Env(key, "")
}
