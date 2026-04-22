package runner

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

type criticDimensions struct {
	Depth                  int `json:"depth"`
	MissedAngles           int `json:"missed_angles"`
	ArchitecturalAwareness int `json:"architectural_awareness"`
	ActionableValue        int `json:"actionable_value"`
	Calibration            int `json:"calibration"`
}

type criticMetaLearning struct {
	Key   string `json:"key"`
	Value string `json:"value"`
	Scope string `json:"scope"`
}

type criticResponse struct {
	Score          int                  `json:"score"`
	Dimensions     criticDimensions     `json:"dimensions"`
	Strengths      []string             `json:"strengths"`
	Weaknesses     []string             `json:"weaknesses"`
	MetaLearnings  []criticMetaLearning `json:"meta_learnings"`
}

func runCritic(repo string, issueNumber int, taskType, reviewOutput string) {
	if reviewOutput == "" {
		return
	}

	// Fetch existing meta-learnings for context
	existingML, err := database.GetMetaLearnings(repo, 20)
	if err != nil {
		slog.Warn("critic: failed to fetch meta-learnings", "error", err)
	}
	mlText := "None yet."
	if len(existingML) > 0 {
		var lines []string
		for _, ml := range existingML {
			lines = append(lines, fmt.Sprintf("- [%s] %s: %s (applied %dx)", ml.Scope, ml.Key, ml.Value, ml.TimesApplied))
		}
		mlText = strings.Join(lines, "\n")
	}

	// Fetch repo memory
	repoMemory := fetchExistingMemory(
		fmt.Sprintf("http://localhost:9081"), repo,
	)

	prompt := prompts.Render("critic.txt", map[string]string{
		"Repo":                  repo,
		"TaskType":              taskType,
		"ReviewOutput":         reviewOutput,
		"ExistingMetaLearnings": mlText,
		"RepoMemory":           repoMemory,
	})

	response, err := ai.Call(prompt, 4096)
	if err != nil {
		slog.Warn("critic: AI call failed", "error", err)
		return
	}

	// Parse JSON response
	response = strings.TrimSpace(response)
	if idx := strings.Index(response, "{"); idx > 0 {
		response = response[idx:]
	}
	if idx := strings.LastIndex(response, "}"); idx >= 0 && idx < len(response)-1 {
		response = response[:idx+1]
	}

	var result criticResponse
	if err := json.Unmarshal([]byte(response), &result); err != nil {
		slog.Warn("critic: failed to parse response", "error", err, "response", ai.Sanitize(response))
		return
	}

	// Store critic score
	strengthsJSON, _ := json.Marshal(result.Strengths)
	weaknessesJSON, _ := json.Marshal(result.Weaknesses)

	database.AddCriticScore(database.CriticScore{
		Repo:                   repo,
		IssueNumber:            issueNumber,
		TaskType:               taskType,
		Score:                  result.Score,
		Depth:                  result.Dimensions.Depth,
		MissedAngles:           result.Dimensions.MissedAngles,
		ArchitecturalAwareness: result.Dimensions.ArchitecturalAwareness,
		ActionableValue:        result.Dimensions.ActionableValue,
		Calibration:            result.Dimensions.Calibration,
		Strengths:              string(strengthsJSON),
		Weaknesses:             string(weaknessesJSON),
	})

	// Store meta-learnings
	stored := 0
	for _, ml := range result.MetaLearnings {
		if ml.Key == "" || ml.Value == "" {
			continue
		}
		if ml.Scope != "repo" && ml.Scope != "general" {
			ml.Scope = "repo"
		}
		if len(ml.Value) > 150 {
			// Condense via AI
			condensed, cerr := ai.Call(fmt.Sprintf(
				"Condense this review lesson into under 150 characters. Plain text only. Return ONLY the condensed text.\n\nOriginal: %s", ml.Value), 256)
			if cerr == nil && len(strings.TrimSpace(condensed)) <= 150 {
				ml.Value = strings.TrimSpace(condensed)
			} else {
				slog.Warn("critic: dropping oversized meta-learning", "key", ml.Key)
				continue
			}
		}

		database.AddMetaLearning(database.MetaLearning{
			Repo:            repo,
			Scope:           ml.Scope,
			Key:             ml.Key,
			Value:           ml.Value,
			SourceIssue:     issueNumber,
			ScoreAtCreation: result.Score,
		})
		stored++
	}

	slog.Info("critic evaluation complete",
		"repo", repo,
		"issue", issueNumber,
		"score", result.Score,
		"depth", result.Dimensions.Depth,
		"missed_angles", result.Dimensions.MissedAngles,
		"meta_learnings_stored", stored,
		"strengths", len(result.Strengths),
		"weaknesses", len(result.Weaknesses),
	)
}

// buildMetaLearningsContext builds the "Lessons from Previous Reviews" section
// to inject into review prompts. Returns the text and the IDs to increment.
func buildMetaLearningsContext(repo string) (string, []int64) {
	learnings, err := database.GetMetaLearnings(repo, 15)
	if err != nil || len(learnings) == 0 {
		return "", nil
	}

	var lines []string
	var ids []int64
	for _, ml := range learnings {
		scope := "this repo"
		if ml.Scope == "general" {
			scope = "all repos"
		}
		lines = append(lines, fmt.Sprintf("- [%s] %s (learned from #%d, applied %dx)", scope, ml.Value, ml.SourceIssue, ml.TimesApplied))
		ids = append(ids, ml.ID)
	}

	return "## Lessons from Previous Reviews\n\n" + strings.Join(lines, "\n") + "\n", ids
}
