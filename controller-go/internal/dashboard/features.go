package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

// --- POST /api/rerun-all/{owner}/{repo} ---

func (s *Server) handleRerunAll(w http.ResponseWriter, r *http.Request) {
	wildcard := chi.URLParam(r, "*")
	if wildcard == "" {
		wildcard = strings.TrimPrefix(r.URL.Path, "/api/rerun-all/")
	}
	repo := strings.TrimPrefix(wildcard, "/")
	if repo == "" {
		jsonError(w, "repo required", 400)
		return
	}

	// Clear all processed entries for this repo
	database.DeleteProcessedForRepo(repo)

	// Get all runs for this repo, trigger re-run for each unique issue
	runs, _ := database.GetRuns(repo, 100)
	issues := make(map[int]bool)
	triggered := 0
	for _, run := range runs {
		if issues[run.Issue] {
			continue
		}
		issues[run.Issue] = true
		if s.rerun != nil {
			if err := s.rerun(repo, run.Issue); err == nil {
				triggered++
			}
		} else if s.reproduce != nil {
			if err := s.reproduce(repo, run.Issue); err == nil {
				triggered++
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status":    "triggered",
		"repo":      repo,
		"triggered": triggered,
	})
}

// --- GET /api/report/{id} ---

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(chi.URLParam(r, "id"), 10, 64)
	if err != nil {
		jsonError(w, "invalid id", 400)
		return
	}
	run, err := database.GetRun(id)
	if err != nil || run == nil {
		jsonError(w, "run not found", 404)
		return
	}

	// Accept header determines format
	if strings.Contains(r.Header.Get("Accept"), "text/html") || r.URL.Query().Get("format") == "html" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!DOCTYPE html>
<html><head><title>OpinAI Report — %s#%d</title>
<style>body{font-family:sans-serif;max-width:800px;margin:40px auto;padding:20px;background:#0d1117;color:#c9d1d9;}
pre{background:#161b22;padding:16px;border-radius:8px;overflow-x:auto;}
table{border-collapse:collapse;width:100%%;}th,td{border:1px solid #30363d;padding:8px;text-align:left;}</style>
</head><body>`, run.Repo, run.Issue)
		fmt.Fprintf(w, "<h1>OpinAI Report: %s#%d</h1>", html.EscapeString(run.Repo), run.Issue)
		fmt.Fprintf(w, "<p><strong>Verdict:</strong> %s | <strong>Confidence:</strong> %s | <strong>Category:</strong> %s</p>",
			html.EscapeString(run.Verdict), html.EscapeString(run.Confidence), html.EscapeString(run.Category))
		fmt.Fprintf(w, "<pre>%s</pre>", html.EscapeString(run.Report))
		fmt.Fprintf(w, `<hr><p><em>"That's just, like, your opinion, man." — OpinAI</em></p></body></html>`)
		return
	}

	// Default: JSON
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(run)
}

// --- GET /api/run-history?repo=X&issue=N ---

func (s *Server) handleRunHistory(w http.ResponseWriter, r *http.Request) {
	repo := r.URL.Query().Get("repo")
	issueStr := r.URL.Query().Get("issue")
	if repo == "" || issueStr == "" {
		jsonError(w, "repo and issue required", 400)
		return
	}
	issue, _ := strconv.Atoi(issueStr)

	history, _ := database.GetRunsByIssue(repo, issue)
	if history == nil {
		history = []database.Run{}
	}
	json.NewEncoder(w).Encode(history)
}

// --- POST /api/webhook/github ---

func (s *Server) handleGitHubWebhook(w http.ResponseWriter, r *http.Request) {
	secret := os.Getenv("OPINAI_WEBHOOK_SECRET")
	body, _ := io.ReadAll(r.Body)
	r.Body.Close()

	// Verify signature if secret is set
	if secret != "" {
		sig := r.Header.Get("X-Hub-Signature-256")
		if sig == "" {
			jsonError(w, "missing signature", 401)
			return
		}
		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		expected := "sha256=" + hex.EncodeToString(mac.Sum(nil))
		if !hmac.Equal([]byte(sig), []byte(expected)) {
			jsonError(w, "invalid signature", 401)
			return
		}
	}

	event := r.Header.Get("X-GitHub-Event")
	if event != "push" {
		json.NewEncoder(w).Encode(map[string]string{"status": "ignored", "event": event})
		return
	}

	var payload struct {
		Repository struct {
			FullName      string `json:"full_name"`
			DefaultBranch string `json:"default_branch"`
		} `json:"repository"`
		Ref string `json:"ref"`
	}
	json.Unmarshal(body, &payload)
	repo := payload.Repository.FullName

	// Only process pushes to monitored repos
	monitored := ParseRepos(os.Getenv("REPOS"))
	found := false
	for _, m := range monitored {
		if m == repo {
			found = true
			break
		}
	}
	if !found {
		json.NewEncoder(w).Encode(map[string]string{"status": "not_monitored", "repo": repo})
		return
	}

	// Only re-run on pushes to the default branch (main/master)
	defaultBranch := payload.Repository.DefaultBranch
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	expectedRef := "refs/heads/" + defaultBranch
	if payload.Ref != expectedRef {
		slog.Info("webhook: ignoring push to non-default branch", "repo", repo, "ref", payload.Ref, "default", defaultBranch)
		json.NewEncoder(w).Encode(map[string]any{
			"status": "ignored",
			"reason": "not default branch",
			"ref":    payload.Ref,
		})
		return
	}

	slog.Info("webhook: push to default branch", "repo", repo, "ref", payload.Ref)

	// Re-run all BUG_CONFIRMED issues (check if they're fixed)
	runs, _ := database.GetRuns(repo, 50)
	rerunCount := 0
	for _, run := range runs {
		if run.Verdict == "BUG_CONFIRMED" && s.reproduce != nil {
			database.DeleteProcessedIssue(repo, run.Issue)
			if err := s.reproduce(repo, run.Issue); err == nil {
				rerunCount++
			}
		}
	}

	json.NewEncoder(w).Encode(map[string]any{
		"status":  "processed",
		"repo":    repo,
		"reruns":  rerunCount,
	})
}
