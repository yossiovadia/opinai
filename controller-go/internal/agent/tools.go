// Package agent implements an agentic investigation loop for bug reproduction.
package agent

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
)

const (
	maxFileSize    = 10240  // 10KB per file read
	maxGrepResults = 50     // max grep matches
	maxListDepth   = 3      // max directory listing depth
	maxTestRuns    = 3      // max test script executions
	testTimeout    = 60     // seconds per test run
	requestTimeout = 30     // seconds per HTTP request
)

// ToolState tracks tool usage limits and collected metadata during an investigation.
type ToolState struct {
	RepoDir   string
	ServerURL string
	TestRuns  int
	FilesRead []string
}

// ToolDefs returns the tool definitions for the agent.
func ToolDefs() []ai.ToolDef {
	return []ai.ToolDef{
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
		{
			Name:        "run_test",
			Description: "Execute a Python 3 test script. The script should print JSON lines with test results: {\"test\": \"name\", \"status\": \"pass|fail\", \"details\": \"...\"}. Maximum 3 test runs per investigation.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"script": map[string]any{
						"type":        "string",
						"description": "Complete Python 3 test script to execute",
					},
				},
				"required": []string{"script"},
			},
		},
		{
			Name:        "server_request",
			Description: "Make an HTTP request to the running server. Only requests to the server URL are allowed.",
			InputSchema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"method": map[string]any{
						"type":        "string",
						"description": "HTTP method (GET, POST, PUT, DELETE, PATCH)",
					},
					"path": map[string]any{
						"type":        "string",
						"description": "URL path (e.g. '/api/users' or '/health')",
					},
					"body": map[string]any{
						"type":        "string",
						"description": "Request body (for POST/PUT/PATCH)",
					},
					"content_type": map[string]any{
						"type":        "string",
						"description": "Content-Type header (default: application/json)",
					},
				},
				"required": []string{"method", "path"},
			},
		},
	}
}

// HandleTool executes a tool call and returns the result string and whether it's an error.
func (ts *ToolState) HandleTool(call ai.ToolCall) (string, bool) {
	switch call.Name {
	case "read_file":
		return ts.handleReadFile(call.Input)
	case "list_dir":
		return ts.handleListDir(call.Input)
	case "grep":
		return ts.handleGrep(call.Input)
	case "run_test":
		return ts.handleRunTest(call.Input)
	case "server_request":
		return ts.handleServerRequest(call.Input)
	default:
		return fmt.Sprintf("unknown tool: %s", call.Name), true
	}
}

func (ts *ToolState) handleReadFile(input map[string]any) (string, bool) {
	path, _ := input["path"].(string)
	if path == "" {
		return "path is required", true
	}

	fullPath := ts.safePath(path)
	if fullPath == "" {
		return "path is outside the repository", true
	}

	data, err := os.ReadFile(fullPath)
	if err != nil {
		return fmt.Sprintf("cannot read file: %s", err), true
	}

	content := string(data)
	if len(content) > maxFileSize {
		content = content[:maxFileSize] + fmt.Sprintf("\n... (truncated, file is %d bytes)", len(data))
	}

	ts.FilesRead = append(ts.FilesRead, path)
	return content, false
}

func (ts *ToolState) handleListDir(input map[string]any) (string, bool) {
	path, _ := input["path"].(string)
	if path == "" {
		path = "."
	}

	fullPath := ts.safePath(path)
	if fullPath == "" {
		return "path is outside the repository", true
	}

	entries, err := os.ReadDir(fullPath)
	if err != nil {
		return fmt.Sprintf("cannot list directory: %s", err), true
	}

	var lines []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() {
			name += "/"
		}
		lines = append(lines, name)
	}

	if len(lines) == 0 {
		return "(empty directory)", false
	}
	return strings.Join(lines, "\n"), false
}

func (ts *ToolState) handleGrep(input map[string]any) (string, bool) {
	pattern, _ := input["pattern"].(string)
	if pattern == "" {
		return "pattern is required", true
	}

	searchPath, _ := input["path"].(string)
	if searchPath == "" {
		searchPath = "."
	}
	fullPath := ts.safePath(searchPath)
	if fullPath == "" {
		return "path is outside the repository", true
	}

	args := []string{"-rn", "--max-count=5"}
	if include, ok := input["include"].(string); ok && include != "" {
		args = append(args, "--include="+include)
	}
	args = append(args, pattern, fullPath)

	cmd := exec.Command("grep", args...)
	out, err := cmd.CombinedOutput()
	output := string(out)

	// grep returns exit 1 for no matches — not an error
	if err != nil && output == "" {
		return "(no matches found)", false
	}

	// Trim output to maxGrepResults lines
	lines := strings.Split(output, "\n")
	if len(lines) > maxGrepResults {
		lines = lines[:maxGrepResults]
		output = strings.Join(lines, "\n") + fmt.Sprintf("\n... (%d+ matches, showing first %d)", len(lines), maxGrepResults)
	}

	// Strip the repo dir prefix from output for cleaner display
	output = strings.ReplaceAll(output, ts.RepoDir+"/", "")

	return output, false
}

func (ts *ToolState) handleRunTest(input map[string]any) (string, bool) {
	script, _ := input["script"].(string)
	if script == "" {
		return "script is required", true
	}

	if ts.TestRuns >= maxTestRuns {
		return fmt.Sprintf("maximum test runs reached (%d). Deliver your verdict with the results you have.", maxTestRuns), true
	}
	ts.TestRuns++

	tmpFile := "/tmp/opinai_agent_test.py"
	if err := os.WriteFile(tmpFile, []byte(script), 0o644); err != nil {
		return fmt.Sprintf("cannot write test file: %s", err), true
	}
	defer os.Remove(tmpFile)

	cmd := exec.Command("python3", tmpFile)
	cmd.Env = os.Environ()
	// Add server URL to env so scripts can use it
	if ts.ServerURL != "" {
		cmd.Env = append(cmd.Env, "SERVER_URL="+ts.ServerURL)
	}

	done := make(chan error, 1)
	var out []byte
	go func() {
		var err error
		out, err = cmd.CombinedOutput()
		done <- err
	}()

	select {
	case err := <-done:
		output := string(out)
		if err != nil {
			output += fmt.Sprintf("\n[script exited with error: %s]", err)
		}
		slog.Info("agent test run complete", "run", ts.TestRuns, "output_bytes", len(output))
		return output, false
	case <-time.After(time.Duration(testTimeout) * time.Second):
		if cmd.Process != nil {
			cmd.Process.Kill()
		}
		return fmt.Sprintf("test script timed out after %d seconds", testTimeout), true
	}
}

func (ts *ToolState) handleServerRequest(input map[string]any) (string, bool) {
	if ts.ServerURL == "" {
		return "no server is running — use code review and run_test instead", true
	}

	method, _ := input["method"].(string)
	path, _ := input["path"].(string)
	body, _ := input["body"].(string)
	contentType, _ := input["content_type"].(string)
	if contentType == "" {
		contentType = "application/json"
	}

	if method == "" || path == "" {
		return "method and path are required", true
	}

	url := ts.ServerURL + path

	var bodyReader *strings.Reader
	if body != "" {
		bodyReader = strings.NewReader(body)
	} else {
		bodyReader = strings.NewReader("")
	}

	req, err := http.NewRequest(strings.ToUpper(method), url, bodyReader)
	if err != nil {
		return fmt.Sprintf("invalid request: %s", err), true
	}
	if body != "" {
		req.Header.Set("Content-Type", contentType)
	}

	client := &http.Client{Timeout: time.Duration(requestTimeout) * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Sprintf("request failed: %s", err), true
	}
	defer resp.Body.Close()

	respBytes, _ := io.ReadAll(io.LimitReader(resp.Body, maxFileSize+1))
	respStr := string(respBytes)
	if len(respBytes) > maxFileSize {
		respStr = respStr[:maxFileSize] + "\n... (response truncated)"
	}

	result := fmt.Sprintf("HTTP %d %s\n\n%s", resp.StatusCode, resp.Status, respStr)
	return result, false
}

// safePath resolves a path relative to the repo dir and ensures it doesn't escape.
func (ts *ToolState) safePath(path string) string {
	// Clean and join
	cleaned := filepath.Clean(path)
	if filepath.IsAbs(cleaned) {
		// Allow absolute paths only if they're under repo dir
		if !strings.HasPrefix(cleaned, ts.RepoDir) {
			return ""
		}
		return cleaned
	}

	full := filepath.Join(ts.RepoDir, cleaned)
	// Verify it's still under repo dir after resolution
	abs, err := filepath.Abs(full)
	if err != nil {
		return ""
	}
	if !strings.HasPrefix(abs, ts.RepoDir) {
		return ""
	}
	return abs
}
