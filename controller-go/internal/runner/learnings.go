package runner

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

type learning struct {
	Key    string `json:"key"`
	Value  string `json:"value"`
	Action string `json:"action"`
}

type learningsResponse struct {
	Learnings []learning `json:"learnings"`
}

func extractAndStoreLearnings(repo, taskType, report string) {
	controllerURL := os.Getenv("OPINAI_CONTROLLER_URL")
	if controllerURL == "" {
		slog.Debug("skipping learnings extraction: no controller URL")
		return
	}

	existingMemory := fetchExistingMemory(controllerURL, repo)

	prompt := prompts.Render("extract_learnings.txt", map[string]string{
		"Repo":           repo,
		"TaskType":       taskType,
		"Report":         report,
		"ExistingMemory": existingMemory,
	})

	response, err := ai.Call(prompt, 2048)
	if err != nil {
		slog.Warn("learnings extraction AI call failed", "error", err)
		return
	}

	response = strings.TrimSpace(response)
	if idx := strings.Index(response, "{"); idx > 0 {
		response = response[idx:]
	}
	if idx := strings.LastIndex(response, "}"); idx >= 0 && idx < len(response)-1 {
		response = response[:idx+1]
	}

	var result learningsResponse
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		slog.Warn("failed to parse learnings JSON", "error", err, "response", ai.Sanitize(response))
		return
	}

	stored, skipped := 0, 0
	for _, l := range result.Learnings {
		if l.Action == "skip" || l.Key == "" || l.Value == "" {
			skipped++
			continue
		}
		if len(l.Value) > 500 {
			l.Value = l.Value[:500]
		}
		if err := postRepoMemory(controllerURL, repo, l.Key, l.Value); err != nil {
			slog.Warn("failed to store learning", "key", l.Key, "error", err)
			continue
		}
		stored++
		slog.Info("stored learning", "key", l.Key, "action", l.Action)
	}
	slog.Info("learnings extraction complete", "stored", stored, "skipped", skipped)
}

func fetchExistingMemory(controllerURL, repo string) string {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(controllerURL + "/api/admin/repo-memory/" + repo)
	if err != nil {
		slog.Debug("failed to fetch existing memory", "error", err)
		return "{}"
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		return "{}"
	}
	return string(body)
}

func postRepoMemory(controllerURL, repo, key, value string) error {
	payload, _ := json.Marshal(map[string]string{"key": key, "value": value})
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(
		controllerURL+"/api/admin/repo-memory/"+repo,
		"application/json",
		strings.NewReader(string(payload)),
	)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}
