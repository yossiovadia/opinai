package agent

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

// RepoAnalysis holds the structured result of an agent-based repo analysis.
type RepoAnalysis struct {
	Description  string            `json:"description"`
	TechStack    string            `json:"tech_stack"`
	Architecture ArchitectureInfo  `json:"architecture"`
	APISurface   APISurfaceInfo    `json:"api_surface"`
	ErrorHandling ErrorHandlingInfo `json:"error_handling"`
	Configuration ConfigInfo       `json:"configuration"`
	Testing       TestingInfo      `json:"testing"`
	Observability ObservabilityInfo `json:"observability"`
	Deployment    DeploymentInfo   `json:"deployment"`
	BugHints      BugHintsInfo     `json:"bug_reproduction_hints"`

	// Metadata about the analysis run
	Iterations int `json:"iterations"`
	ToolCalls  int `json:"tool_calls"`
}

type ArchitectureInfo struct {
	Type       string   `json:"type"`
	EntryPoint string   `json:"entry_point"`
	KeyModules []string `json:"key_modules"`
	Patterns   string   `json:"patterns"`
}

type APISurfaceInfo struct {
	Endpoints         []EndpointInfo `json:"endpoints"`
	AuthRequired      bool           `json:"auth_required"`
	AuthMethod        string         `json:"auth_method"`
	StreamingSupport  bool           `json:"streaming_support"`
	StreamingProtocol string         `json:"streaming_protocol"`
}

type EndpointInfo struct {
	Path    string `json:"path"`
	Method  string `json:"method"`
	Purpose string `json:"purpose"`
}

type ErrorHandlingInfo struct {
	ErrorFormat       string   `json:"error_format"`
	CommonStatusCodes []int    `json:"common_status_codes"`
	CustomErrorTypes  []string `json:"custom_error_types"`
}

type ConfigInfo struct {
	EnvVars     []EnvVarInfo `json:"env_vars"`
	ConfigFiles []string     `json:"config_files"`
	CLIFlags    []string     `json:"cli_flags"`
}

type EnvVarInfo struct {
	Name    string `json:"name"`
	Purpose string `json:"purpose"`
	Default string `json:"default"`
}

type TestingInfo struct {
	Framework    string `json:"framework"`
	TestDir      string `json:"test_dir"`
	TestPatterns string `json:"test_patterns"`
}

type ObservabilityInfo struct {
	HealthEndpoint  string   `json:"health_endpoint"`
	MetricsEndpoint string   `json:"metrics_endpoint"`
	MetricsFormat   string   `json:"metrics_format"`
	LoggingStyle    string   `json:"logging_style"`
	LogLevels       []string `json:"log_levels"`
}

type DeploymentInfo struct {
	Type           string   `json:"type"`
	NeedsCluster   bool     `json:"needs_cluster"`
	ExternalDeps   []string `json:"external_deps"`
	BuildCommand   string   `json:"build_command"`
	RunCommand     string   `json:"run_command"`
	InstallCommand string   `json:"install_command"`
}

type BugHintsInfo struct {
	CommonBugAreas   []string `json:"common_bug_areas"`
	TestStrategy     string   `json:"test_strategy"`
	HowToTest        string   `json:"how_to_test"`
	KnownLimitations string   `json:"known_limitations"`
}

// AnalyzeRepo runs the agent loop to deeply understand a repository.
// repoDir is the path to the cloned repository.
// maxIter caps the number of AI round-trips (default 15).
func AnalyzeRepo(repoDir string, repoName string, maxIter int) (RepoAnalysis, error) {
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		return RepoAnalysis{}, fmt.Errorf("no AI provider configured")
	}

	if maxIter <= 0 {
		maxIter = 15
	}

	systemPrompt := prompts.Render("agent_analyze.txt", map[string]string{
		"RepoDir": repoDir,
	})

	userMsg := fmt.Sprintf("Analyze the repository %s located at %s. Read the code to build a deep understanding of the project.", repoName, repoDir)

	// Analysis only needs read-only tools
	tools := []ai.ToolDef{
		{
			Name:        "read_file",
			Description: "Read a file from the repository. Path is relative to repo root.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "File path relative to repo root (e.g. 'src/main.py')",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "list_dir",
			Description: "List directory contents. Path is relative to repo root. Returns file/dir names.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"path": map[string]any{
						"type":        "string",
						"description": "Directory path relative to repo root (e.g. '.' or 'src')",
					},
				},
				"required": []string{"path"},
			},
		},
		{
			Name:        "grep",
			Description: "Search for a pattern in repository files. Returns matching lines with file paths.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"pattern": map[string]any{
						"type":        "string",
						"description": "Search pattern (regex supported)",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "Directory to search in, relative to repo root (default: '.')",
					},
					"include": map[string]any{
						"type":        "string",
						"description": "File glob pattern to include (e.g. '*.py', '*.go')",
					},
				},
				"required": []string{"pattern"},
			},
		},
	}

	state := &ToolState{
		RepoDir: repoDir,
	}

	slog.Info("agent: starting repo analysis", "repo", repoName, "repo_dir", repoDir, "max_iter", maxIter)

	handler := func(call ai.ToolCall) (string, bool) {
		return state.HandleTool(call)
	}

	finalText, iterations, toolCalls, err := ai.RunAgentLoop(
		cfg, systemPrompt, userMsg, tools, handler, maxIter, 8192,
	)
	if err != nil {
		slog.Error("agent: analysis loop error", "error", err)
		if finalText == "" {
			return RepoAnalysis{}, fmt.Errorf("analysis failed: %w", err)
		}
		// Try to parse partial result
	}

	result, parseErr := parseAnalysis(finalText)
	if parseErr != nil {
		slog.Warn("agent: could not parse structured analysis, returning raw", "error", parseErr)
		return RepoAnalysis{
			Description: truncAnalysis(finalText, 500),
			Iterations:  iterations,
			ToolCalls:   toolCalls,
		}, parseErr
	}

	result.Iterations = iterations
	result.ToolCalls = toolCalls

	slog.Info("agent: analysis complete",
		"description", truncAnalysis(result.Description, 80),
		"tech_stack", result.TechStack,
		"iterations", iterations,
		"tool_calls", toolCalls,
	)

	return result, nil
}

// parseAnalysis extracts the RepoAnalysis JSON from the agent's final text.
func parseAnalysis(text string) (RepoAnalysis, error) {
	var result RepoAnalysis

	// Try direct parse first
	text = strings.TrimSpace(text)
	if err := json.Unmarshal([]byte(text), &result); err == nil {
		return result, nil
	}

	// Strip markdown fences if present
	lines := strings.Split(text, "\n")
	var clean []string
	for _, l := range lines {
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "```") {
			continue
		}
		clean = append(clean, l)
	}
	cleaned := strings.Join(clean, "\n")
	if err := json.Unmarshal([]byte(strings.TrimSpace(cleaned)), &result); err == nil {
		return result, nil
	}

	// Try to extract JSON object from text
	if start := strings.Index(text, "{"); start >= 0 {
		depth := 0
		for i := start; i < len(text); i++ {
			switch text[i] {
			case '{':
				depth++
			case '}':
				depth--
				if depth == 0 {
					candidate := text[start : i+1]
					if err := json.Unmarshal([]byte(candidate), &result); err == nil {
						return result, nil
					}
					return result, fmt.Errorf("found JSON block but failed to parse: %s", truncAnalysis(candidate, 200))
				}
			}
		}
	}

	return result, fmt.Errorf("no JSON object found in response")
}

// ToFlatMap converts a RepoAnalysis to a flat map[string]string for storage
// in repo_memory, preserving backward compatibility with existing keys.
func (a *RepoAnalysis) ToFlatMap() map[string]string {
	m := map[string]string{
		"description":     a.Description,
		"tech_stack":      a.TechStack,
		"deployment_type": a.Deployment.Type,
		"needs_cluster":   fmt.Sprintf("%t", a.Deployment.NeedsCluster),
		"test_strategy":   a.BugHints.TestStrategy,
		"how_to_test":     a.BugHints.HowToTest,
		"build_command":   a.Deployment.BuildCommand,
		"run_command":     a.Deployment.RunCommand,
		"install_command": a.Deployment.InstallCommand,
	}

	// Store the full analysis as JSON for rich context
	fullJSON, err := json.Marshal(a)
	if err == nil {
		m["rich_analysis"] = string(fullJSON)
	}

	return m
}

// FormatContext returns a human-readable summary of the analysis for use
// as context in bug investigation prompts.
func (a *RepoAnalysis) FormatContext() string {
	var b strings.Builder

	if a.Description != "" {
		fmt.Fprintf(&b, "Description: %s\n", a.Description)
	}
	if a.TechStack != "" {
		fmt.Fprintf(&b, "Tech Stack: %s\n", a.TechStack)
	}
	if a.Architecture.Type != "" {
		fmt.Fprintf(&b, "Architecture: %s", a.Architecture.Type)
		if a.Architecture.Patterns != "" {
			fmt.Fprintf(&b, " (%s)", a.Architecture.Patterns)
		}
		b.WriteString("\n")
	}
	if a.Architecture.EntryPoint != "" {
		fmt.Fprintf(&b, "Entry Point: %s\n", a.Architecture.EntryPoint)
	}

	if len(a.APISurface.Endpoints) > 0 {
		b.WriteString("API Endpoints:\n")
		for _, ep := range a.APISurface.Endpoints {
			fmt.Fprintf(&b, "  - %s %s — %s\n", ep.Method, ep.Path, ep.Purpose)
		}
	}
	if a.APISurface.AuthMethod != "" && a.APISurface.AuthMethod != "none" {
		fmt.Fprintf(&b, "Auth: %s\n", a.APISurface.AuthMethod)
	}
	if a.APISurface.StreamingSupport {
		fmt.Fprintf(&b, "Streaming: %s\n", a.APISurface.StreamingProtocol)
	}

	if a.ErrorHandling.ErrorFormat != "" {
		fmt.Fprintf(&b, "Error format: %s\n", a.ErrorHandling.ErrorFormat)
	}

	if len(a.Configuration.EnvVars) > 0 {
		b.WriteString("Config env vars:")
		for _, ev := range a.Configuration.EnvVars {
			fmt.Fprintf(&b, " %s", ev.Name)
		}
		b.WriteString("\n")
	}

	if a.Observability.HealthEndpoint != "" {
		fmt.Fprintf(&b, "Health: %s\n", a.Observability.HealthEndpoint)
	}
	if a.Observability.MetricsEndpoint != "" {
		fmt.Fprintf(&b, "Metrics: %s (%s)\n", a.Observability.MetricsEndpoint, a.Observability.MetricsFormat)
	}

	if a.Testing.Framework != "" {
		fmt.Fprintf(&b, "Tests: %s in %s\n", a.Testing.Framework, a.Testing.TestDir)
	}

	if a.BugHints.TestStrategy != "" {
		fmt.Fprintf(&b, "Recommended test strategy: %s\n", a.BugHints.TestStrategy)
	}
	if a.BugHints.HowToTest != "" {
		fmt.Fprintf(&b, "How to test: %s\n", a.BugHints.HowToTest)
	}

	return b.String()
}

func truncAnalysis(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
