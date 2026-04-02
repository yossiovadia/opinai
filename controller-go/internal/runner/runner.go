// Package runner implements the reproduction flow that runs inside K8s Job pods.
package runner

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/controller"
)

// setupRetryInfo tracks self-healing retries during setup for inclusion in reports.
var setupRetryInfo string

// Run executes the full reproduction flow. Called when --mode=runner.
func Run() {
	repo := os.Getenv("REPO")
	issueNumber := os.Getenv("ISSUE_NUMBER")
	if repo == "" || issueNumber == "" {
		slog.Error("REPO and ISSUE_NUMBER env vars required")
		os.Exit(1)
	}
	if os.Getenv("GITHUB_TOKEN") == "" {
		slog.Error("GITHUB_TOKEN env var required")
		os.Exit(1)
	}

	slog.Info("starting reproduction", "repo", repo, "issue", issueNumber)

	// Step 1: Fetch issue
	issue, err := controller.FetchIssueDetails(repo, atoi(issueNumber))
	if err != nil {
		slog.Error("failed to fetch issue", "error", err)
		os.Exit(1)
	}
	title := issue.Title
	body := issue.Body
	slog.Info("issue fetched", "title", title)

	// Step 2: Start server or use sandbox
	serverURL := os.Getenv("SERVER_URL")
	sandboxNS := os.Getenv("OPINAI_SANDBOX_NAMESPACE")
	sandboxEndpoints := os.Getenv("OPINAI_SANDBOX_ENDPOINTS")
	var serverProc *os.Process

	if sandboxNS != "" {
		slog.Info("using sandbox deployment", "namespace", sandboxNS)
		if sandboxEndpoints != "" {
			var endpoints map[string]string
			json.Unmarshal([]byte(sandboxEndpoints), &endpoints)
			for _, fqdn := range endpoints {
				serverURL = "http://" + fqdn
				os.Setenv("SERVER_URL", serverURL)
				break
			}
		}
	} else {
		serverProc, serverURL = startServer()
		if serverURL != "" {
			os.Setenv("SERVER_URL", serverURL)
		}
	}

	defer func() {
		if serverProc != nil {
			slog.Info("terminating server process")
			serverProc.Signal(os.Interrupt)
			time.Sleep(2 * time.Second)
			serverProc.Kill()
		}
	}()

	// Step 3: Categorize
	category := ai.Categorize(title, body)
	slog.Info("categorized", "category", category)
	fmt.Printf("--- OPINAI CATEGORY: %s ---\n", category)

	if category == "FEATURE" || category == "QUESTION" || category == "DOCS" {
		verdictEnum := "FEATURE_REQUEST"
		fmt.Printf("--- OPINAI VERDICT: %s ---\n", verdictEnum)
		catLabels := map[string]string{
			"FEATURE":  "feature request",
			"QUESTION": "question / help request",
			"DOCS":     "documentation issue",
		}
		comment := fmt.Sprintf(
			"## OpinAI Bug Reproduction Report\n\n"+
				"**Issue:** #%s\n"+
				"**Category:** %s\n"+
				"**Verdict:** %s\n"+
				"**Analysis:** AI-powered (model: %s)\n\n"+
				"This appears to be a **%s**, not a reproducible bug. Skipping reproduction.\n\n"+
				"---\n"+
				"*\"That's just, like, your opinion, man.\" -- [OpinAI](https://github.com/yossiovadia/opinai)*",
			issueNumber, category, verdictEnum, os.Getenv("AI_MODEL"), catLabels[category],
		)
		postComment(repo, atoi(issueNumber), comment)
		addLabel(repo, atoi(issueNumber))
		emitRepoMemory(map[string]string{
			"last_analyzed_issue": issueNumber,
			"last_verdict":       verdictEnum,
		})
		slog.Info("skipped reproduction", "verdict", verdictEnum)
		return
	}

	// Step 4: Generate tests
	profileCtx := loadProfileContext()
	repoCtx := os.Getenv("OPINAI_REPO_CONTEXT")
	script, err := ai.GenerateTests(title, body, serverURL, profileCtx, repoCtx)
	if err != nil || script == "" {
		fmt.Println("--- OPINAI VERDICT: ERROR ---")
		comment := fmt.Sprintf(
			"## OpinAI Bug Reproduction Report\n\n"+
				"**Issue:** #%s\n"+
				"**Category:** %s\n"+
				"**Verdict:** ERROR\n"+
				"**Analysis:** Skipped (AI analysis failed)\n\n"+
				"Could not generate tests for this issue.\n\n"+
				"---\n"+
				"*\"That's just, like, your opinion, man.\" -- [OpinAI](https://github.com/yossiovadia/opinai)*",
			issueNumber, category,
		)
		postComment(repo, atoi(issueNumber), comment)
		addLabel(repo, atoi(issueNumber))
		return
	}
	slog.Info("test script generated", "bytes", len(script))

	// Step 5: Run tests
	slog.Info("running tests...")
	testOutput := runTests(script)
	slog.Info("tests completed", "output_bytes", len(testOutput))

	// Step 6: Get verdict
	vr := ai.GetVerdict(title, body, testOutput)
	slog.Info("verdict", "verdict", vr.Verdict, "confidence", vr.Confidence)
	fmt.Printf("--- OPINAI VERDICT: %s ---\n", vr.Verdict)
	fmt.Printf("--- OPINAI CONFIDENCE: %s ---\n", vr.Confidence)

	// Step 7: Build report
	resultsTable := parseResultsTable(testOutput)
	serverInfo := ""
	if serverURL != "" {
		serverInfo = fmt.Sprintf("**Server:** `%s`\n", serverURL)
	}
	retryInfo := ""
	if setupRetryInfo != "" {
		retryInfo = fmt.Sprintf("**Setup:** %s\n", setupRetryInfo)
	}
	verdictText := vr.Text
	if verdictText == "" {
		verdictText = "AI verdict unavailable."
	}

	comment := fmt.Sprintf(
		"## OpinAI Bug Reproduction Report\n\n"+
			"**Issue:** #%s\n"+
			"**Category:** %s\n"+
			"**Verdict:** %s\n"+
			"**Confidence:** %s\n"+
			"%s"+
			"%s"+
			"**Analysis:** AI-powered (model: %s)\n\n"+
			"### Results\n\n"+
			"| Test | Status | Details |\n"+
			"|------|--------|---------|\n"+
			"%s\n"+
			"### Verdict\n\n"+
			"%s\n\n"+
			"<details><summary>Raw test output</summary>\n\n"+
			"```\n%s\n```\n\n"+
			"</details>\n\n"+
			"---\n"+
			"*\"That's just, like, your opinion, man.\" -- [OpinAI](https://github.com/yossiovadia/opinai)*",
		issueNumber, category, vr.Verdict, vr.Confidence, serverInfo, retryInfo,
		os.Getenv("AI_MODEL"), resultsTable, verdictText, truncStr(testOutput, 5000),
	)

	postComment(repo, atoi(issueNumber), comment)
	addLabel(repo, atoi(issueNumber))
	emitRepoMemory(map[string]string{
		"last_analyzed_issue": issueNumber,
		"last_verdict":       vr.Verdict,
		"last_confidence":    vr.Confidence,
	})
	slog.Info("reproduction complete", "repo", repo, "issue", issueNumber)
}

// --- helpers ---

func startServer() (*os.Process, string) {
	profile := loadProfile()
	if profile == nil {
		return nil, ""
	}

	buildCmd, _ := profile["build"].(string)
	runCmd, _ := profile["run"].(string)
	healthURL, _ := profile["health"].(string)

	// Clone repo
	repo := os.Getenv("REPO")
	cloneDir := "/tmp/opinai-repo"
	slog.Info("cloning repo", "repo", repo)
	cmd := exec.Command("git", "clone", "--depth=1", "https://github.com/"+repo+".git", cloneDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		slog.Warn("clone failed", "error", err)
		return nil, ""
	}

	// Analyze README on first run
	analyzeReadme(cloneDir)

	// Build with self-healing retries
	var buildRetry RetryResult
	if buildCmd != "" {
		resolved := strings.ReplaceAll(buildCmd, "pip install", "python3 -m pip install --user")
		slog.Info("installing", "cmd", resolved)
		var err error
		_, buildRetry, err = runWithRetry(resolved, cloneDir, buildEnv())
		if err != nil {
			slog.Warn("build failed after retries", "error", err, "retries", buildRetry.Retries)
			// Continue anyway — the AI tests may still be useful
		} else if buildRetry.Retries > 0 {
			slog.Info("build succeeded after retries", "retries", buildRetry.Retries, "fixes", buildRetry.FixesApplied)
		}
	}
	// Store retry info for report
	setupRetryInfo = formatRetryInfo(buildRetry)

	if runCmd == "" {
		return nil, ""
	}

	// Start server with retry on initial failures
	slog.Info("starting server", "cmd", runCmd)
	cmd = exec.Command("sh", "-c", runCmd)
	cmd.Dir = cloneDir
	cmd.Env = buildEnv()
	if err := cmd.Start(); err != nil {
		// Try common fixes for server start
		slog.Warn("server start failed, attempting fix", "error", err)
		fixedCmd := runCmd
		if strings.Contains(err.Error(), "not found") {
			fixedCmd = "export PATH=/tmp/pip-user/bin:$PATH && " + runCmd
		}
		cmd = exec.Command("sh", "-c", fixedCmd)
		cmd.Dir = cloneDir
		cmd.Env = buildEnv()
		if err2 := cmd.Start(); err2 != nil {
			slog.Warn("server start failed after fix", "error", err2)
			return nil, ""
		}
		if setupRetryInfo != "" {
			setupRetryInfo += "; "
		}
		setupRetryInfo += "fixed server start command (PATH)"
	}

	// Derive server URL
	serverURL := "http://localhost:8000"
	if healthURL != "" {
		parts := strings.SplitN(healthURL, "/", 4)
		if len(parts) >= 3 {
			serverURL = strings.Join(parts[:3], "/")
		}
	} else {
		healthURL = "http://localhost:8000/health"
	}

	// Wait for health
	slog.Info("waiting for server health", "url", healthURL)
	for i := 0; i < 30; i++ {
		checkCmd := exec.Command("curl", "-sf", healthURL)
		if checkCmd.Run() == nil {
			slog.Info("server healthy", "seconds", i)
			return cmd.Process, serverURL
		}
		if cmd.ProcessState != nil {
			slog.Warn("server exited early")
			return nil, ""
		}
		time.Sleep(time.Second)
	}
	slog.Warn("server did not become healthy within 30s")
	return cmd.Process, serverURL
}

func runTests(script string) string {
	tmpFile := "/tmp/opinai_test.sh"
	content := "#!/usr/bin/env bash\nset -euo pipefail\n\n" + script
	os.WriteFile(tmpFile, []byte(content), 0o755)
	defer os.Remove(tmpFile)

	cmd := exec.Command("/bin/bash", tmpFile)
	cmd.Env = buildEnv()
	out, err := cmd.CombinedOutput()
	output := string(out)
	if err != nil {
		output += fmt.Sprintf("\n[script exited with error: %s]\n", err)
	}
	return output
}

func postComment(repo string, issue int, body string) {
	safe := ai.Sanitize(body)
	autoPost := strings.EqualFold(os.Getenv("OPINAI_AUTO_POST"), "true")

	fmt.Println("--- OPINAI SUGGESTED COMMENT ---")
	fmt.Println(safe)
	fmt.Println("--- END SUGGESTED COMMENT ---")

	if !autoPost {
		slog.Info("auto-post disabled — comment saved for review")
		return
	}

	if err := controller.PostComment(repo, issue, safe); err != nil {
		slog.Error("failed to post comment", "error", err)
	}
}

func addLabel(repo string, issue int) {
	label := os.Getenv("DONE_LABEL")
	if label == "" {
		label = "opinai-done"
	}
	controller.AddLabel(repo, issue, label)
}

func emitRepoMemory(data map[string]string) {
	b, _ := json.Marshal(data)
	fmt.Println("--- OPINAI REPO MEMORY ---")
	fmt.Println(string(b))
	fmt.Println("--- END REPO MEMORY ---")
}

func loadProfile() map[string]any {
	repo := os.Getenv("REPO")
	r := strings.NewReplacer("/", "_", "-", "_", ".", "_")
	key := "REPO_PROFILE_" + r.Replace(repo)
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	var profile map[string]any
	json.Unmarshal([]byte(raw), &profile)
	return profile
}

func loadProfileContext() string {
	profile := loadProfile()
	if profile == nil {
		return ""
	}
	gpu := "No"
	if b, ok := profile["gpu"].(bool); ok && b {
		gpu = "Yes"
	}
	k8s := "No"
	if b, ok := profile["k8s"].(bool); ok && b {
		k8s = "Yes"
	}
	return fmt.Sprintf("\nProject Profile:\n"+
		"- Type: %v\n- Install: %v\n- Run: %v\n- Health check: %v\n"+
		"- Needs GPU: %s\n- Needs Kubernetes: %s\n- Dependencies: %v\n"+
		"\nUse this to properly install and start the server before testing.\n",
		profile["type"], profile["build"], profile["run"], profile["health"], gpu, k8s, profile["deps"])
}

func analyzeReadme(cloneDir string) {
	if os.Getenv("OPINAI_HAS_KNOWLEDGE") == "true" {
		return
	}
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		return
	}

	readmePath := cloneDir + "/README.md"
	data, err := os.ReadFile(readmePath)
	if err != nil {
		readmePath = cloneDir + "/readme.md"
		data, err = os.ReadFile(readmePath)
		if err != nil {
			return
		}
	}
	readme := string(data)
	if len(readme) > 3000 {
		readme = readme[:3000]
	}

	slog.Info("analyzing README for first-time knowledge")
	prompt := fmt.Sprintf(
		"Analyze this project README from %s.\n\n%s\n\n"+
			"Provide a brief JSON summary (no markdown fences, just raw JSON):\n"+
			"{\n  \"description\": \"what this project does\",\n"+
			"  \"tech_stack\": \"languages and frameworks\",\n"+
			"  \"how_to_test\": \"how to test bugs\",\n"+
			"  \"deployment_needs\": \"infrastructure needed\"\n}",
		os.Getenv("REPO"), readme,
	)
	content, err := ai.Call(prompt, 1024)
	if err != nil || content == "" {
		return
	}
	// Strip fences
	content = strings.TrimSpace(content)
	lines := strings.Split(content, "\n")
	var clean []string
	for _, l := range lines {
		if !strings.HasPrefix(strings.TrimSpace(l), "```") {
			clean = append(clean, l)
		}
	}
	content = strings.Join(clean, "\n")

	var result map[string]string
	if json.Unmarshal([]byte(content), &result) == nil {
		emitRepoMemory(result)
		slog.Info("emitted README knowledge")
	} else {
		emitRepoMemory(map[string]string{"description": truncStr(content, 500)})
	}
}

func parseResultsTable(output string) string {
	var table string
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var r struct {
			Test    string `json:"test"`
			Status  string `json:"status"`
			Details string `json:"details"`
		}
		if json.Unmarshal([]byte(line), &r) == nil {
			icon := strings.ToUpper(r.Status)
			if r.Status == "pass" {
				icon = "PASS"
			} else if r.Status == "fail" {
				icon = "FAIL"
			}
			table += fmt.Sprintf("| %s | %s | %s |\n", r.Test, icon, r.Details)
		}
	}
	if table == "" {
		table = "| (no structured results) | - | - |\n"
	}
	return table
}

func buildEnv() []string {
	env := os.Environ()
	// Ensure pip user bin is on PATH
	env = append(env, "PYTHONUSERBASE=/tmp/pip-user")
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=/tmp/pip-user/bin:/usr/local/bin:/root/.local/bin:" + kv[5:]
			break
		}
	}
	return env
}

func atoi(s string) int {
	n := 0
	fmt.Sscanf(s, "%d", &n)
	return n
}

func truncStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
