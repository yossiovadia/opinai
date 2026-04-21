package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

// AgentResult holds the outcome of an agent investigation.
type AgentResult struct {
	Verdict    string   // BUG_CONFIRMED, NOT_REPRODUCIBLE, INCONCLUSIVE
	Confidence string   // HIGH, MEDIUM, LOW
	TestScript string   // Final test script used
	TestOutput string   // Raw test output
	Report     string   // AI's analysis/report
	Iterations int      // How many loop iterations
	ToolCalls  int      // Total tool calls made
	FilesRead  []string // Files the agent read
}

// Investigate runs the agent loop to investigate a bug report.
// repoDir is the path to the cloned repository (e.g. /tmp/opinai-repo).
// maxIter caps the number of AI round-trips (default 10).
func Investigate(title, body, serverURL, repoDir, repoContext string, maxIter int) AgentResult {
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		slog.Warn("agent: no AI provider configured, skipping investigation")
		return AgentResult{Verdict: "INCONCLUSIVE", Confidence: "LOW", Report: "No AI provider configured"}
	}

	if maxIter <= 0 {
		maxIter = 200
	}

	// Build system prompt
	hostTools := os.Getenv("OPINAI_HOST_TOOLS") == "true"
	systemPrompt := prompts.Render("agent_investigate.txt", map[string]any{
		"ServerURL":   serverURL,
		"RepoDir":     repoDir,
		"RepoContext": repoContext,
		"HostTools":   hostTools,
	})

	// Build user message with the bug report
	userMsg := fmt.Sprintf("## Bug Report\n\n**Title:** %s\n\n**Description:**\n%s", title, body)
	if serverURL != "" {
		userMsg += fmt.Sprintf("\n\nThe server is running at %s — check /health first.", serverURL)
	} else {
		userMsg += "\n\nNo server is running. Use code review (read_file, grep) to investigate the bug. " +
			"You can still use run_test to run local test scripts."
	}

	// Set up tool state
	state := &ToolState{
		RepoDir:   repoDir,
		ServerURL: serverURL,
	}

	tools := ToolDefs()
	// Remove server_request tool if no server is running
	if serverURL == "" {
		filtered := tools[:0]
		for _, t := range tools {
			if t.Name != "server_request" {
				filtered = append(filtered, t)
			}
		}
		tools = filtered
	}

	slog.Info("agent: starting investigation", "repo_dir", repoDir, "server_url", serverURL, "max_iter", maxIter)

	// Run the agent loop
	handler := func(call ai.ToolCall) (string, bool) {
		return state.HandleTool(call)
	}

	finalText, iterations, toolCalls, err := ai.RunAgentLoop(
		cfg, systemPrompt, userMsg, tools, handler, maxIter, 4096,
	)
	if err != nil {
		slog.Error("agent: loop error", "error", err)
		if finalText == "" {
			finalText = fmt.Sprintf("Agent investigation failed: %s", err)
		}
	}

	result := AgentResult{
		Report:     finalText,
		Iterations: iterations,
		ToolCalls:  toolCalls,
		FilesRead:  state.FilesRead,
	}

	// Parse verdict from the final text
	result.Verdict, result.Confidence = parseVerdict(finalText)

	// Extract the last test script and output from tool state
	// (These are captured in the report text by the AI)

	slog.Info("agent: investigation complete",
		"verdict", result.Verdict,
		"confidence", result.Confidence,
		"iterations", result.Iterations,
		"tool_calls", result.ToolCalls,
		"files_read", len(result.FilesRead),
	)

	return result
}

// PRReviewResult holds the outcome of a PR review investigation.
type PRReviewResult struct {
	Verdict            string   // APPROVE, CHANGES_REQUESTED, COMMENT
	Risk               string   // LOW, MEDIUM, HIGH, CRITICAL
	ReviewText         string   // Full review markdown
	Report             string   // AI's final analysis
	SuggestedQuestions string   // JSON array of follow-up questions
	Iterations         int
	ToolCalls          int
	FilesRead          []string
}

// ReviewPR runs the agent loop to review a pull request.
// existingComments is a pre-formatted string of existing PR comments/reviews for deduplication.
func ReviewPR(prTitle, prBody, prDiff, prAuthor, changedFiles, serverURL, repoDir, repoContext, linkedIssues, existingComments string, maxIter int) PRReviewResult {
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		slog.Warn("agent: no AI provider configured, skipping PR review")
		return PRReviewResult{Verdict: "COMMENT", Risk: "LOW", Report: "No AI provider configured"}
	}

	if maxIter <= 0 {
		maxIter = 200
	}

	// Build system prompt
	systemPrompt := prompts.Render("agent_pr_review.txt", map[string]string{
		"ServerURL":        serverURL,
		"RepoDir":          repoDir,
		"RepoContext":      repoContext,
		"PRTitle":          prTitle,
		"PRAuthor":         prAuthor,
		"PRBody":           prBody,
		"PRDiff":           prDiff,
		"ChangedFiles":     changedFiles,
		"LinkedIssues":     linkedIssues,
		"ExistingComments": existingComments,
	})

	// Build user message
	userMsg := fmt.Sprintf("Review this pull request.\n\n**Title:** %s\n**Author:** %s\n\nThe diff and changed files are in the system prompt. Start by reading the changed files in full context, then investigate.", prTitle, prAuthor)
	if serverURL != "" {
		userMsg += fmt.Sprintf("\n\nThe server is running at %s with the PR applied — you can test it.", serverURL)
	} else {
		userMsg += "\n\nNo server is running. Focus on code review using read_file, grep, and run_test."
	}

	state := &ToolState{
		RepoDir:   repoDir,
		ServerURL: serverURL,
	}

	tools := ToolDefs()
	if serverURL == "" {
		filtered := tools[:0]
		for _, t := range tools {
			if t.Name != "server_request" {
				filtered = append(filtered, t)
			}
		}
		tools = filtered
	}

	slog.Info("agent: starting PR review", "repo_dir", repoDir, "server_url", serverURL, "max_iter", maxIter)

	handler := func(call ai.ToolCall) (string, bool) {
		return state.HandleTool(call)
	}

	finalText, iterations, toolCalls, err := ai.RunAgentLoop(
		cfg, systemPrompt, userMsg, tools, handler, maxIter, 4096,
	)
	if err != nil {
		slog.Error("agent: PR review loop error", "error", err)
		if finalText == "" {
			finalText = fmt.Sprintf("PR review failed: %s", err)
		}
	}

	result := PRReviewResult{
		Report:     finalText,
		Iterations: iterations,
		ToolCalls:  toolCalls,
		FilesRead:  state.FilesRead,
	}

	result.Verdict, result.Risk = parsePRVerdict(finalText)
	result.ReviewText = extractPRReview(finalText)
	result.SuggestedQuestions = extractSuggestedQuestions(finalText)

	slog.Info("agent: PR review complete",
		"verdict", result.Verdict,
		"risk", result.Risk,
		"iterations", result.Iterations,
		"tool_calls", result.ToolCalls,
		"files_read", len(result.FilesRead),
	)

	return result
}

// parsePRVerdict extracts the verdict and risk from the AI's PR review response.
func parsePRVerdict(text string) (string, string) {
	verdict := "COMMENT"
	risk := "LOW"

	if idx := strings.Index(text, "===PR_VERDICT==="); idx >= 0 {
		block := text[idx:]
		if end := strings.Index(block, "===END_PR_VERDICT==="); end >= 0 {
			block = block[:end]
		}
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			upper := strings.ToUpper(line)
			if strings.HasPrefix(upper, "VERDICT:") {
				val := strings.TrimSpace(strings.TrimPrefix(upper, "VERDICT:"))
				for _, v := range []string{"APPROVE", "CHANGES_REQUESTED", "COMMENT"} {
					if strings.Contains(val, v) {
						verdict = v
						break
					}
				}
			}
			if strings.HasPrefix(upper, "RISK:") {
				val := strings.TrimSpace(strings.TrimPrefix(upper, "RISK:"))
				for _, r := range []string{"CRITICAL", "HIGH", "MEDIUM", "LOW"} {
					if strings.Contains(val, r) {
						risk = r
						break
					}
				}
			}
		}
		return verdict, risk
	}

	// Fallback keyword scan
	upper := strings.ToUpper(text)
	if strings.Contains(upper, "CHANGES_REQUESTED") {
		verdict = "CHANGES_REQUESTED"
	} else if strings.Contains(upper, "APPROVE") {
		verdict = "APPROVE"
	}
	for _, r := range []string{"CRITICAL", "HIGH", "MEDIUM", "LOW"} {
		if strings.Contains(upper, "RISK: "+r) || strings.Contains(upper, "RISK:"+r) {
			risk = r
			break
		}
	}

	return verdict, risk
}

// extractPRReview extracts the review text between markers.
func extractPRReview(text string) string {
	start := strings.Index(text, "--- OPINAI PR REVIEW ---")
	if start < 0 {
		return ""
	}
	start += len("--- OPINAI PR REVIEW ---")
	end := strings.Index(text[start:], "--- END PR REVIEW ---")
	if end < 0 {
		return strings.TrimSpace(text[start:])
	}
	return strings.TrimSpace(text[start : start+end])
}

// extractSuggestedQuestions extracts the JSON array of suggested questions from agent output.
func extractSuggestedQuestions(text string) string {
	start := strings.Index(text, "--- OPINAI SUGGESTED_QUESTIONS ---")
	if start < 0 {
		return ""
	}
	start += len("--- OPINAI SUGGESTED_QUESTIONS ---")
	end := strings.Index(text[start:], "--- END SUGGESTED_QUESTIONS ---")
	block := ""
	if end < 0 {
		block = strings.TrimSpace(text[start:])
	} else {
		block = strings.TrimSpace(text[start : start+end])
	}
	// Validate it's a JSON array
	var arr []string
	if json.Unmarshal([]byte(block), &arr) != nil {
		return ""
	}
	return block
}

// parseVerdict extracts the verdict and confidence from the AI's final response.
func parseVerdict(text string) (string, string) {
	verdict := "INCONCLUSIVE"
	confidence := "LOW"

	// Look for the structured verdict block
	if idx := strings.Index(text, "===VERDICT==="); idx >= 0 {
		block := text[idx:]
		if end := strings.Index(block, "===END_VERDICT==="); end >= 0 {
			block = block[:end]
		}
		for _, line := range strings.Split(block, "\n") {
			line = strings.TrimSpace(line)
			upper := strings.ToUpper(line)
			if strings.HasPrefix(upper, "VERDICT:") {
				val := strings.TrimSpace(strings.TrimPrefix(upper, "VERDICT:"))
				for _, v := range []string{"BUG_CONFIRMED", "NOT_REPRODUCIBLE", "INCONCLUSIVE"} {
					if strings.Contains(val, v) {
						verdict = v
						break
					}
				}
			}
			if strings.HasPrefix(upper, "CONFIDENCE:") {
				val := strings.TrimSpace(strings.TrimPrefix(upper, "CONFIDENCE:"))
				for _, c := range []string{"HIGH", "MEDIUM", "LOW"} {
					if strings.Contains(val, c) {
						confidence = c
						break
					}
				}
			}
		}
		return verdict, confidence
	}

	// Fallback: scan for keywords in the full text
	upper := strings.ToUpper(text)
	if strings.Contains(upper, "BUG_CONFIRMED") || strings.Contains(upper, "BUG CONFIRMED") {
		verdict = "BUG_CONFIRMED"
	} else if strings.Contains(upper, "NOT_REPRODUCIBLE") || strings.Contains(upper, "NOT REPRODUCIBLE") {
		verdict = "NOT_REPRODUCIBLE"
	}

	if strings.Contains(upper, "CONFIDENCE: HIGH") || strings.Contains(upper, "CONFIDENCE:HIGH") {
		confidence = "HIGH"
	} else if strings.Contains(upper, "CONFIDENCE: MEDIUM") || strings.Contains(upper, "CONFIDENCE:MEDIUM") {
		confidence = "MEDIUM"
	}

	return verdict, confidence
}
