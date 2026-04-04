package ai

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

// AnalyzeDeployment generates deployment options for a repo.
// richAnalysis is optional context from the agent's deep code analysis.
// cloneDir is optional path to a local clone for rendering Helm/kustomize manifests.
func AnalyzeDeployment(repo, readme string, files map[string]string, clusterState map[string][]string, profileJSON, richAnalysis, cloneDir string) (map[string]any, error) {
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

	// Try to render actual K8s manifests from the cloned repo
	renderedManifests := renderProjectManifests(cloneDir)

	// Extract install/deploy instructions from project docs
	installDocs := extractInstallDocs(cloneDir)

	prompt := prompts.Render("analyze_deployment.txt", map[string]string{
		"Repo": repo, "ProfileJSON": profileJSON,
		"Readme": readme, "FilesSummary": filesSummary,
		"CRDs": crds, "Operators": operators, "Namespaces": namespaces,
		"RichAnalysis": richAnalysis, "RenderedManifests": renderedManifests,
		"InstallDocs": installDocs,
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

// renderProjectManifests tries to render K8s manifests from a cloned repo.
// Tries Helm template, then kustomize, then raw YAML files. Returns empty if nothing found.
func renderProjectManifests(cloneDir string) string {
	if cloneDir == "" {
		return ""
	}

	// Try Helm chart
	helmDirs := []string{"deploy", "chart", "charts", "helm", "deploy/helm", "."}
	for _, dir := range helmDirs {
		chartFile := filepath.Join(cloneDir, dir, "Chart.yaml")
		if _, err := os.Stat(chartFile); err == nil {
			chartDir := filepath.Join(cloneDir, dir)
			// Run helm dependency build first (best-effort)
			depCmd := exec.Command("helm", "dependency", "build", chartDir)
			depCmd.Dir = cloneDir
			depCmd.CombinedOutput()

			cmd := exec.Command("helm", "template", "opinai-render", chartDir)
			cmd.Dir = cloneDir
			out, err := cmd.CombinedOutput()
			if err == nil && len(out) > 0 {
				slog.Info("rendered Helm chart for deployment analysis", "dir", dir, "bytes", len(out))
				return truncateManifests(string(out))
			}
			slog.Warn("helm template failed", "dir", dir, "error", err, "output", truncate(string(out), 200))
		}
	}

	// Try kustomize
	kustomizeDirs := []string{"config/default", "config/manager", "deploy", "kustomize", "."}
	for _, dir := range kustomizeDirs {
		kFile := filepath.Join(cloneDir, dir, "kustomization.yaml")
		if _, err := os.Stat(kFile); err != nil {
			kFile = filepath.Join(cloneDir, dir, "kustomization.yml")
			if _, err := os.Stat(kFile); err != nil {
				continue
			}
		}
		cmd := exec.Command("kubectl", "kustomize", filepath.Join(cloneDir, dir))
		out, err := cmd.CombinedOutput()
		if err == nil && len(out) > 0 {
			slog.Info("rendered kustomize manifests for deployment analysis", "dir", dir, "bytes", len(out))
			return truncateManifests(string(out))
		}
		slog.Warn("kubectl kustomize failed", "dir", dir, "error", err)
	}

	// Fall back to raw YAML files
	rawDirs := []string{"deploy", "manifests", "k8s", "config"}
	var collected strings.Builder
	for _, dir := range rawDirs {
		dirPath := filepath.Join(cloneDir, dir)
		entries, err := os.ReadDir(dirPath)
		if err != nil {
			continue
		}
		for _, entry := range entries {
			if entry.IsDir() {
				continue
			}
			name := entry.Name()
			if !strings.HasSuffix(name, ".yaml") && !strings.HasSuffix(name, ".yml") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(dirPath, name))
			if err != nil {
				continue
			}
			collected.WriteString(fmt.Sprintf("--- %s/%s ---\n", dir, name))
			collected.Write(data)
			collected.WriteString("\n")
			if collected.Len() > 8192 {
				break
			}
		}
		if collected.Len() > 8192 {
			break
		}
	}
	if collected.Len() > 0 {
		slog.Info("collected raw YAML manifests for deployment analysis", "bytes", collected.Len())
		return truncateManifests(collected.String())
	}
	return ""
}

func truncateManifests(s string) string {
	const maxLen = 8192
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "\n... (truncated)"
}

// extractInstallDocs reads install/deploy instructions from project documentation
// and Makefile targets. Returns concatenated text, truncated to 6KB.
func extractInstallDocs(cloneDir string) string {
	if cloneDir == "" {
		return ""
	}

	var docs strings.Builder

	// Read documentation files that commonly contain install instructions
	docFiles := []string{
		"CONTRIBUTING.md", "DEVELOPMENT.md", "INSTALL.md",
		"deploy/README.md", "docs/install.md", "docs/deployment.md",
		"docs/getting-started.md", "docs/development.md",
	}
	for _, f := range docFiles {
		data, err := os.ReadFile(filepath.Join(cloneDir, f))
		if err != nil {
			continue
		}
		content := string(data)
		if len(content) > 3000 {
			content = content[:3000]
		}
		docs.WriteString(fmt.Sprintf("--- %s ---\n%s\n\n", f, content))
		if docs.Len() > 5000 {
			break
		}
	}

	// Extract deploy/install targets from Makefile
	makefile, err := os.ReadFile(filepath.Join(cloneDir, "Makefile"))
	if err == nil {
		targets := extractMakeTargets(string(makefile), []string{
			"deploy", "install", "undeploy", "run", "docker-build",
			"manifests", "generate", "build",
		})
		if targets != "" {
			docs.WriteString("--- Makefile targets ---\n")
			docs.WriteString(targets)
			docs.WriteString("\n")
		}
	}

	if docs.Len() == 0 {
		return ""
	}

	result := docs.String()
	if len(result) > 6144 {
		result = result[:6144] + "\n... (truncated)"
	}
	slog.Info("extracted install docs for deployment analysis", "bytes", len(result))
	return result
}

// extractMakeTargets extracts specific target definitions from a Makefile.
func extractMakeTargets(makefile string, targets []string) string {
	targetSet := make(map[string]bool, len(targets))
	for _, t := range targets {
		targetSet[t] = true
	}

	var result strings.Builder
	lines := strings.Split(makefile, "\n")
	capturing := false
	for _, line := range lines {
		// Check if this line starts a target we want
		if !strings.HasPrefix(line, "\t") && strings.Contains(line, ":") {
			name := strings.TrimSpace(strings.Split(line, ":")[0])
			// Strip .PHONY prefix or similar
			name = strings.TrimPrefix(name, ".PHONY")
			name = strings.TrimSpace(name)
			if targetSet[name] {
				capturing = true
				result.WriteString(line)
				result.WriteString("\n")
				continue
			}
			capturing = false
		}
		if capturing && (strings.HasPrefix(line, "\t") || line == "") {
			result.WriteString(line)
			result.WriteString("\n")
		} else {
			capturing = false
		}
	}
	return result.String()
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
