package runner

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

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

const (
	defaultCuratorEveryN        = 10
	defaultCuratorMaxLearnings  = 30
	curatorMaxRepoLearnings     = 25
	curatorMaxGeneralLearnings  = 15
	curatorMinReviewsForPrune   = 10
)

func curatorEveryN() int {
	if v := os.Getenv("CURATOR_EVERY_N"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultCuratorEveryN
}

func curatorMaxLearnings() int {
	if v := os.Getenv("CURATOR_MAX_LEARNINGS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return defaultCuratorMaxLearnings
}

type curatedLearning struct {
	Action   string  `json:"action"`
	ID       int64   `json:"id,omitempty"`
	MergeIDs []int64 `json:"merge_ids,omitempty"`
	Key      string  `json:"key,omitempty"`
	Value    string  `json:"value,omitempty"`
	Scope    string  `json:"scope,omitempty"`
	Category string  `json:"category,omitempty"`
}

type curatorResponse struct {
	Curated []curatedLearning `json:"curated"`
}

type metaLearningInput struct {
	ID              int64  `json:"id"`
	Key             string `json:"key"`
	Value           string `json:"value"`
	Scope           string `json:"scope"`
	Category        string `json:"category"`
	TimesApplied    int    `json:"times_applied"`
	ScoreAtCreation int    `json:"score_at_creation"`
	CreatedAt       string `json:"created_at"`
}

func checkAndRunCurator(controllerURL, repo string) {
	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(controllerURL + "/api/admin/self-improvement?repo=" + repo)
	if err != nil {
		slog.Debug("curator check: failed to fetch self-improvement stats", "error", err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		return
	}

	var stats struct {
		MetaLearningsCount int `json:"meta_learnings_count"`
		CriticScoreCount   int `json:"critic_score_count"`
	}
	if json.Unmarshal(body, &stats) != nil {
		return
	}

	everyN := curatorEveryN()
	maxLearnings := curatorMaxLearnings()

	shouldRun := false
	if stats.CriticScoreCount > 0 && stats.CriticScoreCount%everyN == 0 {
		slog.Info("curator trigger: every Nth critic evaluation", "count", stats.CriticScoreCount, "n", everyN)
		shouldRun = true
	}
	if stats.MetaLearningsCount > maxLearnings {
		slog.Info("curator trigger: learning count exceeds threshold", "count", stats.MetaLearningsCount, "threshold", maxLearnings)
		shouldRun = true
	}

	if !shouldRun {
		return
	}

	runCurator(controllerURL, repo)
}

func runCurator(controllerURL, repo string) {
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		slog.Warn("curator: no AI provider configured")
		return
	}

	client := &http.Client{Timeout: 10 * time.Second}

	resp, err := client.Get(controllerURL + "/api/admin/meta-learnings?repo=" + repo)
	if err != nil {
		slog.Warn("curator: failed to fetch meta-learnings", "error", err)
		return
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		slog.Warn("curator: bad response fetching meta-learnings", "status", resp.StatusCode)
		return
	}

	var learnings []metaLearningInput
	if json.Unmarshal(body, &learnings) != nil || len(learnings) == 0 {
		slog.Info("curator: no learnings to curate")
		return
	}

	learningsBefore := len(learnings)

	var statsResp struct {
		CriticScoreCount int `json:"critic_score_count"`
	}
	if r, err := client.Get(controllerURL + "/api/admin/self-improvement?repo=" + repo); err == nil {
		defer r.Body.Close()
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &statsResp)
	}
	reviewCount := statsResp.CriticScoreCount
	if reviewCount < curatorMinReviewsForPrune {
		reviewCount = curatorMinReviewsForPrune
	}

	var sb strings.Builder
	for _, l := range learnings {
		sb.WriteString(fmt.Sprintf("- ID=%d | key=%s | scope=%s | category=%s | times_applied=%d | score_at_creation=%d | created=%s\n  value: %s\n\n",
			l.ID, l.Key, l.Scope, l.Category, l.TimesApplied, l.ScoreAtCreation, l.CreatedAt, l.Value))
	}

	prompt := prompts.Render("curator.txt", map[string]string{
		"Repo":                repo,
		"Learnings":           sb.String(),
		"ReviewCount":         fmt.Sprintf("%d", reviewCount),
		"MaxRepoLearnings":    fmt.Sprintf("%d", curatorMaxRepoLearnings),
		"MaxGeneralLearnings": fmt.Sprintf("%d", curatorMaxGeneralLearnings),
		"MinReviewsForPrune":  fmt.Sprintf("%d", curatorMinReviewsForPrune),
	})

	response, err := ai.Call(prompt, 4096)
	if err != nil {
		slog.Warn("curator: AI call failed", "error", err)
		return
	}

	result, err := parseCuratorResponse(response)
	if err != nil {
		slog.Warn("curator: failed to parse response", "error", err)
		return
	}

	stats := applyCuratorResults(controllerURL, repo, learnings, result.Curated)

	storeCuratorRun(controllerURL, repo, learningsBefore, stats)

	slog.Info("curator run complete",
		"repo", repo,
		"before", learningsBefore,
		"after", stats.after,
		"kept", stats.kept,
		"merged", stats.merged,
		"rewritten", stats.rewritten,
		"deleted", stats.deleted,
	)
}

func parseCuratorResponse(response string) (*curatorResponse, error) {
	response = strings.TrimSpace(response)
	if strings.HasPrefix(response, "```") {
		lines := strings.Split(response, "\n")
		var clean []string
		for _, l := range lines {
			if !strings.HasPrefix(strings.TrimSpace(l), "```") {
				clean = append(clean, l)
			}
		}
		response = strings.Join(clean, "\n")
	}
	if idx := strings.Index(response, "{"); idx > 0 {
		response = response[idx:]
	}
	if idx := strings.LastIndex(response, "}"); idx >= 0 && idx < len(response)-1 {
		response = response[:idx+1]
	}

	var result curatorResponse
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		return nil, fmt.Errorf("JSON parse failed: %w (response: %s)", err, truncStr(response, 300))
	}
	return &result, nil
}

type curatorStats struct {
	kept      int
	merged    int
	rewritten int
	deleted   int
	after     int
}

func applyCuratorResults(controllerURL, repo string, original []metaLearningInput, curated []curatedLearning) curatorStats {
	client := &http.Client{Timeout: 10 * time.Second}
	var stats curatorStats

	var deleteIDs []int64

	for _, c := range curated {
		switch c.Action {
		case "keep":
			if c.ID > 0 && c.Category != "" {
				updateMetaLearning(client, controllerURL, c.ID, c.Key, c.Value, c.Category)
			}
			stats.kept++

		case "merge":
			deleteIDs = append(deleteIDs, c.MergeIDs...)
			maxApplied := 0
			for _, id := range c.MergeIDs {
				for _, orig := range original {
					if orig.ID == id && orig.TimesApplied > maxApplied {
						maxApplied = orig.TimesApplied
					}
				}
			}
			addCuratedLearning(client, controllerURL, repo, c.Key, c.Value, c.Scope, c.Category)
			stats.merged++

		case "rewrite":
			if c.ID > 0 {
				updateMetaLearning(client, controllerURL, c.ID, c.Key, c.Value, c.Category)
			}
			stats.rewritten++

		case "delete":
			if c.ID > 0 {
				deleteIDs = append(deleteIDs, c.ID)
			}
			stats.deleted++
		}
	}

	if len(deleteIDs) > 0 {
		bulkDeleteMetaLearnings(client, controllerURL, deleteIDs)
	}

	stats.after = stats.kept + stats.merged + stats.rewritten
	return stats
}

func updateMetaLearning(client *http.Client, controllerURL string, id int64, key, value, category string) {
	payload, _ := json.Marshal(map[string]any{
		"id":       id,
		"key":      key,
		"value":    value,
		"category": category,
	})
	req, _ := http.NewRequest("PUT", controllerURL+"/api/admin/meta-learnings/"+fmt.Sprintf("%d", id), strings.NewReader(string(payload)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		slog.Warn("curator: failed to update learning", "id", id, "error", err)
		return
	}
	resp.Body.Close()
}

func addCuratedLearning(client *http.Client, controllerURL, repo, key, value, scope, category string) {
	payload, _ := json.Marshal(map[string]any{
		"repo":     repo,
		"key":      key,
		"value":    value,
		"scope":    scope,
		"category": category,
	})
	resp, err := client.Post(
		controllerURL+"/api/admin/meta-learnings",
		"application/json",
		strings.NewReader(string(payload)),
	)
	if err != nil {
		slog.Warn("curator: failed to add merged learning", "key", key, "error", err)
		return
	}
	resp.Body.Close()
}

func bulkDeleteMetaLearnings(client *http.Client, controllerURL string, ids []int64) {
	for _, id := range ids {
		req, _ := http.NewRequest("DELETE", controllerURL+"/api/admin/meta-learnings/"+fmt.Sprintf("%d", id), nil)
		resp, err := client.Do(req)
		if err != nil {
			slog.Warn("curator: failed to delete learning", "id", id, "error", err)
			continue
		}
		resp.Body.Close()
	}
}

func storeCuratorRun(controllerURL, repo string, learningsBefore int, stats curatorStats) {
	client := &http.Client{Timeout: 10 * time.Second}
	payload, _ := json.Marshal(map[string]any{
		"repo":              repo,
		"learnings_before":  learningsBefore,
		"learnings_after":   stats.after,
		"kept":              stats.kept,
		"merged":            stats.merged,
		"rewritten":         stats.rewritten,
		"deleted":           stats.deleted,
	})
	resp, err := client.Post(
		controllerURL+"/api/admin/curator-runs",
		"application/json",
		strings.NewReader(string(payload)),
	)
	if err != nil {
		slog.Warn("curator: failed to store curator run", "error", err)
		return
	}
	resp.Body.Close()
}

func classifyPRType(title string) string {
	lower := strings.ToLower(title)
	switch {
	case strings.Contains(lower, "bump") || strings.Contains(lower, "upgrade") || strings.Contains(lower, "dependabot") || strings.Contains(lower, "update dep"):
		return "dep-bump"
	case strings.Contains(lower, "doc") || strings.Contains(lower, "readme"):
		return "docs"
	case strings.Contains(lower, "test") || strings.Contains(lower, "spec"):
		return "test"
	case strings.Contains(lower, "security") || strings.Contains(lower, "cve") || strings.Contains(lower, "vuln") || strings.Contains(lower, "auth"):
		return "security"
	case strings.Contains(lower, "config") || strings.Contains(lower, "env") || strings.Contains(lower, "yaml") || strings.Contains(lower, "helm"):
		return "config"
	case strings.Contains(lower, "feat") || strings.Contains(lower, "add") || strings.Contains(lower, "implement") || strings.Contains(lower, "introduce"):
		return "feature"
	default:
		return "general"
	}
}
