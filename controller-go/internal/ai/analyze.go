package ai

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

// AnalyzeDeployment generates deployment options for a repo.
// richAnalysis is optional context from the agent's deep code analysis.
func AnalyzeDeployment(repo, readme string, files map[string]string, clusterState map[string][]string, profileJSON, richAnalysis string) (map[string]any, error) {
	cfg := LoadConfig()
	if !cfg.Available() {
		return nil, fmt.Errorf("no AI provider configured")
	}

	filesSummary := ""
	for path, content := range files {
		c := content
		if len(c) > 2000 {
			c = c[:2000]
		}
		filesSummary += fmt.Sprintf("--- %s ---\n%s\n", path, c)
	}

	crds := strings.Join(clusterState["crds"], ", ")
	if crds == "" {
		crds = "none"
	}
	operators := strings.Join(clusterState["operators"], ", ")
	if operators == "" {
		operators = "none"
	}
	namespaces := strings.Join(clusterState["namespaces"], ", ")

	prompt := prompts.Render("analyze_deployment.txt", map[string]string{
		"Repo": repo, "ProfileJSON": profileJSON,
		"Readme": readme, "FilesSummary": filesSummary,
		"CRDs": crds, "Operators": operators, "Namespaces": namespaces,
		"RichAnalysis": richAnalysis,
	})

	content, err := callWithConfig(cfg, prompt, 8192)
	if err != nil {
		return nil, err
	}
	if content == "" {
		return nil, fmt.Errorf("AI returned empty response")
	}

	return ParseAIJSON(content)
}

// ParseAIJSON robustly parses JSON from AI responses. 3-tier:
// 1. Direct parse
// 2. Repair truncated JSON (close open braces/brackets)
// 3. Extract individual option objects
func ParseAIJSON(raw string) (map[string]any, error) {
	text := strings.TrimSpace(raw)

	// Strip markdown code fences
	if strings.HasPrefix(text, "```") {
		var lines []string
		for _, line := range strings.Split(text, "\n") {
			if !strings.HasPrefix(strings.TrimSpace(line), "```") {
				lines = append(lines, line)
			}
		}
		text = strings.TrimSpace(strings.Join(lines, "\n"))
	}

	// Tier 1: direct parse
	var result map[string]any
	if err := json.Unmarshal([]byte(text), &result); err == nil {
		return result, nil
	}

	// Tier 2: repair truncated JSON
	repaired := strings.TrimRight(text, ", \t\n\r")
	openBraces := strings.Count(repaired, "{") - strings.Count(repaired, "}")
	openBrackets := strings.Count(repaired, "[") - strings.Count(repaired, "]")
	if openBrackets > 0 {
		repaired += strings.Repeat("]", openBrackets)
	}
	if openBraces > 0 {
		repaired += strings.Repeat("}", openBraces)
	}
	if err := json.Unmarshal([]byte(repaired), &result); err == nil {
		result["_warning"] = "Response was truncated — some options may be incomplete"
		return result, nil
	}

	// Tier 3: extract complete option objects
	options := extractOptionBlocks(text)
	return map[string]any{
		"options":  options,
		"_warning": fmt.Sprintf("AI response could not be fully parsed. Extracted %d option(s).", len(options)),
	}, nil
}

func extractOptionBlocks(text string) []map[string]any {
	var options []map[string]any
	depth := 0
	start := -1
	for i, ch := range text {
		if ch == '{' {
			if depth == 1 && start == -1 {
				start = i
			}
			depth++
		} else if ch == '}' {
			depth--
			if depth == 1 && start >= 0 {
				candidate := text[start : i+1]
				var obj map[string]any
				if err := json.Unmarshal([]byte(candidate), &obj); err == nil {
					if _, ok := obj["id"]; ok {
						options = append(options, obj)
					} else if _, ok := obj["name"]; ok {
						options = append(options, obj)
					}
				}
				start = -1
			}
		}
	}
	return options
}
