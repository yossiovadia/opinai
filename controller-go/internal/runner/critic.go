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

// CriticResult holds the parsed output from the critic evaluation.
type CriticResult struct {
	Score              int              `json:"score"`
	Strengths          []string         `json:"strengths"`
	Weaknesses         []string         `json:"weaknesses"`
	MetaLearnings      []criticLearning `json:"meta_learnings"`
	RewriteSuggestions []string         `json:"rewrite_suggestions"`
}

type criticLearning struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Scope string `json:"scope"`
}

// runCritic evaluates the quality of a completed review using the critic prompt.
// It fetches existing meta-learnings and repo memory from the controller,
// calls the AI critic, parses the response, stores new meta-learnings and
// the critic score, and returns the result.
func runCritic(repo, taskType, reviewOutput string) (*CriticResult, error) {
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		return nil, fmt.Errorf("no AI provider configured")
	}

	controllerURL := os.Getenv("OPINAI_CONTROLLER_URL")
	if controllerURL == "" {
		return nil, fmt.Errorf("no controller URL for critic evaluation")
	}

	// Fetch existing meta-learnings for context
	existingMetaLearnings := fetchMetaLearningsContext(controllerURL, repo)

	// Fetch repo memory for architectural context
	repoMemory := fetchExistingMemory(controllerURL, repo)

	// Render the critic prompt
	prompt := prompts.Render("critic.txt", map[string]string{
		"Repo":                  repo,
		"TaskType":              taskType,
		"ReviewOutput":          truncStr(reviewOutput, 6000),
		"ExistingMetaLearnings": existingMetaLearnings,
		"RepoMemory":            repoMemory,
	})

	// Call AI
	response, err := ai.Call(prompt, 2048)
	if err != nil {
		return nil, fmt.Errorf("critic AI call failed: %w", err)
	}

	// Parse JSON response
	response = strings.TrimSpace(response)
	// Strip markdown fences if present
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

	// Extract JSON from potential surrounding text
	if idx := strings.Index(response, "{"); idx > 0 {
		response = response[idx:]
	}
	if idx := strings.LastIndex(response, "}"); idx >= 0 && idx < len(response)-1 {
		response = response[:idx+1]
	}

	var result CriticResult
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		return nil, fmt.Errorf("failed to parse critic JSON: %w (response: %s)", err, truncStr(response, 200))
	}

	// Determine issue number from env
	issueNumber := 0
	if n := os.Getenv("ISSUE_NUMBER"); n != "" {
		fmt.Sscanf(n, "%d", &issueNumber)
	}
	if n := os.Getenv("OPINAI_PR_NUMBER"); n != "" {
		fmt.Sscanf(n, "%d", &issueNumber)
	}

	// Store critic score
	storeCriticScore(controllerURL, repo, issueNumber, taskType, result)

	// Store new meta-learnings
	storeMetaLearnings(controllerURL, repo, issueNumber, result.Score, result.MetaLearnings)

	slog.Info("critic evaluation complete",
		"repo", repo,
		"score", result.Score,
		"strengths", len(result.Strengths),
		"weaknesses", len(result.Weaknesses),
		"meta_learnings", len(result.MetaLearnings),
	)

	return &result, nil
}

const criticScoreThreshold = 7
const criticMaxRetries = 2

// criticLoop runs the critic evaluation and, if the score is below threshold,
// rewrites the review with critic feedback and re-evaluates. Returns the
// (possibly improved) output and the final critic result.
// Falls back to the original output on any error.
func criticLoop(repo, taskType, reviewOutput string) (string, *CriticResult) {
	output := reviewOutput
	var lastResult *CriticResult

	for attempt := 0; attempt <= criticMaxRetries; attempt++ {
		result, err := runCritic(repo, taskType, output)
		if err != nil {
			slog.Warn("critic evaluation failed", "attempt", attempt, "error", err)
			return output, lastResult
		}
		lastResult = result

		if result.Score >= criticScoreThreshold {
			if attempt > 0 {
				slog.Info("critic loop: review improved to passing score",
					"attempt", attempt, "score", result.Score)
			}
			go triggerCuratorCheck(repo)
			return output, result
		}

		if attempt == criticMaxRetries {
			slog.Warn("critic loop: max retries reached, posting as-is",
				"score", result.Score, "attempts", attempt+1)
			go triggerCuratorCheck(repo)
			return output, result
		}

		slog.Info("critic loop: score below threshold, rewriting",
			"score", result.Score, "attempt", attempt+1)

		improved, err := reanalyzeWithFeedback(taskType, output, result)
		if err != nil {
			slog.Warn("critic rewrite failed, using original", "error", err)
			go triggerCuratorCheck(repo)
			return output, result
		}
		output = improved
	}

	return output, lastResult
}

func triggerCuratorCheck(repo string) {
	controllerURL := os.Getenv("OPINAI_CONTROLLER_URL")
	if controllerURL == "" {
		return
	}
	checkAndRunCurator(controllerURL, repo)
}

// reanalyzeWithFeedback calls AI to produce an improved review based on critic feedback.
func reanalyzeWithFeedback(taskType, originalOutput string, critic *CriticResult) (string, error) {
	weaknessesJSON, _ := json.Marshal(critic.Weaknesses)
	suggestionsJSON, _ := json.Marshal(critic.RewriteSuggestions)

	prompt := prompts.Render("critic_rewrite.txt", map[string]string{
		"TaskType":           taskType,
		"OriginalReview":     truncStr(originalOutput, 6000),
		"Weaknesses":         string(weaknessesJSON),
		"RewriteSuggestions": string(suggestionsJSON),
		"CriticScore":        fmt.Sprintf("%d", critic.Score),
	})

	response, err := ai.Call(prompt, 4096)
	if err != nil {
		return "", fmt.Errorf("rewrite AI call failed: %w", err)
	}

	response = strings.TrimSpace(response)
	if response == "" {
		return "", fmt.Errorf("empty rewrite response")
	}

	slog.Info("critic rewrite produced improved review", "original_len", len(originalOutput), "improved_len", len(response))
	return response, nil
}

// fetchMetaLearningsContext fetches existing meta-learnings from the controller
// and formats them as context for the critic prompt.
func fetchMetaLearningsContext(controllerURL, repo string) string {
	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(controllerURL + "/api/admin/meta-learnings?repo=" + repo)
	if err != nil {
		slog.Debug("failed to fetch meta-learnings", "error", err)
		return "(none yet)"
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil || resp.StatusCode != 200 {
		return "(none yet)"
	}

	var learnings []struct {
		Key          string `json:"key"`
		Value        string `json:"value"`
		Scope        string `json:"scope"`
		TimesApplied int    `json:"times_applied"`
	}
	if json.Unmarshal(body, &learnings) != nil || len(learnings) == 0 {
		return "(none yet)"
	}

	var sb strings.Builder
	for _, l := range learnings {
		sb.WriteString(fmt.Sprintf("- [%s] %s: %s (applied %d times)\n", l.Scope, l.Key, l.Value, l.TimesApplied))
	}
	return sb.String()
}

// storeCriticScore sends the critic score to the controller for persistence.
func storeCriticScore(controllerURL, repo string, issueNumber int, taskType string, result CriticResult) {
	strengths, _ := json.Marshal(result.Strengths)
	weaknesses, _ := json.Marshal(result.Weaknesses)

	payload, _ := json.Marshal(map[string]any{
		"repo":         repo,
		"issue_number": issueNumber,
		"task_type":    taskType,
		"score":        result.Score,
		"strengths":    string(strengths),
		"weaknesses":   string(weaknesses),
	})

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Post(
		controllerURL+"/api/admin/critic-scores",
		"application/json",
		strings.NewReader(string(payload)),
	)
	if err != nil {
		slog.Warn("failed to store critic score", "error", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode >= 300 {
		slog.Warn("controller returned error storing critic score", "status", resp.StatusCode)
	}
}

// storeMetaLearnings sends new meta-learnings to the controller for persistence.
func storeMetaLearnings(controllerURL, repo string, issueNumber, score int, learnings []criticLearning) {
	client := &http.Client{Timeout: 10 * time.Second}

	for _, l := range learnings {
		if l.Key == "" || l.Value == "" {
			continue
		}
		payload, _ := json.Marshal(map[string]any{
			"repo":              repo,
			"scope":             l.Scope,
			"key":               l.Key,
			"value":             l.Value,
			"source_issue":      issueNumber,
			"score_at_creation": score,
		})
		resp, err := client.Post(
			controllerURL+"/api/admin/meta-learnings",
			"application/json",
			strings.NewReader(string(payload)),
		)
		if err != nil {
			slog.Warn("failed to store meta-learning", "key", l.Key, "error", err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode < 300 {
			slog.Info("stored meta-learning from critic", "key", l.Key, "scope", l.Scope)
		}
	}
}
