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
	issueState := issue.State // "open" or "closed"
	if issueState == "" {
		issueState = "open"
	}
	slog.Info("issue fetched", "title", title, "state", issueState)

	// Step 2: Start server, use sandbox, or deploy from plan
	serverURL := os.Getenv("SERVER_URL")
	sandboxNS := os.Getenv("OPINAI_SANDBOX_NAMESPACE")
	sandboxEndpoints := os.Getenv("OPINAI_SANDBOX_ENDPOINTS")
	deploymentPlan := os.Getenv("OPINAI_DEPLOYMENT_PLAN")
	verifyFix := os.Getenv("OPINAI_VERIFY_FIX") == "true"
	var serverProc *os.Process

	if verifyFix {
		slog.Info("verify-fix mode — forcing full deployment and testing")
	}

	if sandboxNS != "" {
		// Sandbox already created by controller
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
	} else if deploymentPlan != "" && (isK8sProject() || verifyFix) {
		// K8s project with deployment plan — ask AI which option to use
		serverURL = deployFromPlan(title, body, deploymentPlan)
		if serverURL != "" {
			os.Setenv("SERVER_URL", serverURL)
			slog.Info("deployed from plan", "server_url", serverURL)
		}
	} else {
		// Standard: start server in pod
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

	if !verifyFix && (category == "FEATURE" || category == "QUESTION" || category == "DOCS") {
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

	// Add issue state context
	var stateCtx string
	if issueState == "closed" {
		stateCtx = "\n\nThis issue is CLOSED (presumably fixed). Analyze whether the fix is correct. " +
			"If the bug no longer exists, the tests should pass. If it still exists despite being closed, that is a regression.\n"
	} else {
		stateCtx = "\n\nThis is an OPEN issue. Reproduce the bug and confirm or deny it.\n"
	}
	repoCtx = repoCtx + stateCtx

	// Tell the AI it has server control for config-dependent bugs
	profile2 := loadProfile()
	runCommand := ""
	if profile2 != nil {
		runCommand, _ = profile2["run"].(string)
	}
	if runCommand != "" && serverURL != "" {
		repoCtx += fmt.Sprintf(
			"\n\nYou have full control of the server. If the bug requires restarting with different "+
				"configuration (env vars, flags), you can kill the existing server and restart it.\n"+
				"Current start command: %s\n"+
				"Server binary directory: /tmp/opinai-repo\n"+
				"Environment: PYTHONUSERBASE=/tmp/pip-user HOME=/tmp/home\n", runCommand)
	}

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
	vr := ai.GetVerdict(title, body, testOutput, issueState)
	slog.Info("verdict", "verdict", vr.Verdict, "confidence", vr.Confidence)
	fmt.Printf("--- OPINAI VERDICT: %s ---\n", vr.Verdict)
	fmt.Printf("--- OPINAI CONFIDENCE: %s ---\n", vr.Confidence)

	// Step 6b: Generate suggested follow-up questions
	suggestedQs := generateSuggestedQuestions(title, body, vr.Text)
	if suggestedQs != "" {
		fmt.Println("--- OPINAI SUGGESTED_QUESTIONS ---")
		fmt.Println(suggestedQs)
		fmt.Println("--- END SUGGESTED_QUESTIONS ---")
	}

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

	// Set up writable container environment BEFORE any commands
	setupContainerEnv()

	// Install chain:
	// 1. Resolve command: working > analyzed > env > profile
	// 2. Try it
	// 3. If fails: AI self-heal fix
	// 4. If self-heal fails: generateInstallCommand (reads repo files, asks AI for minimal)
	// 5. If that works: save as working_install_command
	{
		repoCtx := os.Getenv("OPINAI_REPO_CONTEXT")
		installCmd := buildCmd

		if planCmd := os.Getenv("OPINAI_INSTALL_COMMAND"); planCmd != "" {
			installCmd = planCmd
			slog.Info("using deployment plan install command", "cmd", installCmd)
		}
		if analyzed := extractMemoryValue(repoCtx, "install_command"); analyzed != "" {
			installCmd = analyzed
			slog.Info("using AI-analyzed install command", "cmd", installCmd)
		}
		if saved := extractMemoryValue(repoCtx, "working_install_command"); saved != "" {
			installCmd = saved
			slog.Info("using saved working install command", "cmd", installCmd)
		}

		if installCmd != "" {
			slog.Info("installing", "cmd", installCmd)
			out, err := runInEnv(installCmd, cloneDir)
			if err != nil {
				slog.Warn("install failed, trying AI self-heal", "error", err)

				// Step 3: AI self-heal
				fixedCmd := askAIForFix(installCmd, out)
				if fixedCmd != "" && fixedCmd != installCmd {
					slog.Info("trying AI-suggested fix", "cmd", fixedCmd)
					_, err2 := runInEnv(fixedCmd, cloneDir)
					if err2 == nil {
						slog.Info("AI fix worked — saving")
						emitRepoMemory(map[string]string{"working_install_command": fixedCmd})
						setupRetryInfo = "AI fixed install: " + truncStr(fixedCmd, 80)
					} else {
						slog.Warn("AI fix also failed, trying fresh generation", "error", err2)

						// Step 4: generate from scratch by reading repo files
						freshCmd := generateInstallCommand(cloneDir)
						if freshCmd != "" && freshCmd != installCmd && freshCmd != fixedCmd {
							slog.Info("trying AI-generated fresh install", "cmd", freshCmd)
							_, err3 := runInEnv(freshCmd, cloneDir)
							if err3 == nil {
								slog.Info("fresh install worked — saving")
								emitRepoMemory(map[string]string{"working_install_command": freshCmd})
								setupRetryInfo = "AI generated install: " + truncStr(freshCmd, 80)
							} else {
								slog.Warn("all install attempts failed", "error", err3)
							}
						}
					}
				} else {
					// No self-heal suggestion — try fresh generation directly
					freshCmd := generateInstallCommand(cloneDir)
					if freshCmd != "" && freshCmd != installCmd {
						slog.Info("trying AI-generated fresh install", "cmd", freshCmd)
						_, err3 := runInEnv(freshCmd, cloneDir)
						if err3 == nil {
							slog.Info("fresh install worked — saving")
							emitRepoMemory(map[string]string{"working_install_command": freshCmd})
							setupRetryInfo = "AI generated install: " + truncStr(freshCmd, 80)
						}
					}
				}
			} else if !strings.Contains(repoCtx, "working_install_command:") {
				emitRepoMemory(map[string]string{"working_install_command": installCmd})
			}
		}
	}

	if runCmd == "" {
		return nil, ""
	}

	// Check for saved working run command
	if saved := extractMemoryValue(os.Getenv("OPINAI_REPO_CONTEXT"), "working_run_command"); saved != "" {
		runCmd = saved
		slog.Info("using saved working run command", "cmd", runCmd)
	}

	// Start server
	slog.Info("starting server", "cmd", runCmd)
	cmd = exec.Command("sh", "-c", runCmd)
	cmd.Dir = cloneDir
	cmd.Env = containerEnv
	if err := cmd.Start(); err != nil {
		slog.Warn("server start failed, asking AI for fix", "error", err)
		fixedCmd := askAIForFix(runCmd, err.Error())
		if fixedCmd != "" {
			cmd = exec.Command("sh", "-c", fixedCmd)
			cmd.Dir = cloneDir
			cmd.Env = containerEnv
			if err2 := cmd.Start(); err2 != nil {
				slog.Warn("server start still failed", "error", err2)
				return nil, ""
			}
			emitRepoMemory(map[string]string{"working_run_command": fixedCmd})
			setupRetryInfo += "AI fixed server start"
		} else {
			return nil, ""
		}
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
	if containerEnv != nil {
		cmd.Env = containerEnv
	} else {
		cmd.Env = os.Environ()
	}
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
		if os.Getenv("OPINAI_AUTO_POST") == "" {
			slog.Info("auto-post is OFF (default safe mode) — comment saved for review, not posted to GitHub")
		} else {
			slog.Info("auto-post disabled — comment saved for review")
		}
		return
	}

	if err := controller.PostComment(repo, issue, safe); err != nil {
		slog.Error("failed to post comment", "error", err)
	}
}

func addLabel(repo string, issue int) {
	// No-op: tracking is done via database only, no GitHub labels
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

	// Read README
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

	// Read dependency files for install analysis
	depsInfo := ""
	for _, depFile := range []string{"pyproject.toml", "setup.py", "requirements.txt", "setup.cfg", "go.mod", "package.json"} {
		depData, err := os.ReadFile(cloneDir + "/" + depFile)
		if err == nil {
			content := string(depData)
			if len(content) > 1500 {
				content = content[:1500]
			}
			depsInfo += fmt.Sprintf("\n--- %s ---\n%s\n", depFile, content)
		}
	}

	slog.Info("analyzing README + deps for first-time knowledge")
	prompt := fmt.Sprintf(
		"Analyze this project from %s.\n\nREADME:\n%s\n\nDependency files:%s\n\n"+
			"Provide a JSON summary (no markdown fences, just raw JSON):\n"+
			"{\n"+
			"  \"description\": \"what this project does in 1-2 sentences\",\n"+
			"  \"tech_stack\": \"languages and frameworks\",\n"+
			"  \"how_to_test\": \"how to test bugs in this project\",\n"+
			"  \"deployment_needs\": \"infrastructure needed\",\n"+
			"  \"install_command\": \"the MINIMAL shell command to install this for API testing in a rootless container with 512Mi RAM, no GPU. "+
			"Use --user --break-system-packages for pip. Use --no-deps if heavy deps (torch, tensorflow, vllm) "+
			"are listed but not needed for echo/test mode. Example: pip install --user --no-deps myapp && pip install --user fastapi uvicorn\"\n"+
			"}",
		os.Getenv("REPO"), readme, depsInfo,
	)
	content, err := ai.Call(prompt, 1500)
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
		slog.Info("emitted README knowledge", "keys", len(result))
		if cmd, ok := result["install_command"]; ok && cmd != "" {
			slog.Info("AI generated install command from analysis", "cmd", cmd)
		}
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

// containerEnv is the pre-configured environment for all subprocess calls.
var containerEnv []string

// setupContainerEnv creates writable directories and builds a clean environment.
func setupContainerEnv() {
	os.MkdirAll("/tmp/pip-user/bin", 0o755)
	os.MkdirAll("/tmp/pip-cache", 0o755)
	os.MkdirAll("/tmp/home", 0o755)

	// Build env with writable paths
	env := os.Environ()
	set := func(key, val string) {
		prefix := key + "="
		for i, kv := range env {
			if strings.HasPrefix(kv, prefix) {
				env[i] = prefix + val
				return
			}
		}
		env = append(env, prefix+val)
	}

	set("PYTHONUSERBASE", "/tmp/pip-user")
	set("PIP_CACHE_DIR", "/tmp/pip-cache")
	set("HOME", "/tmp/home")
	set("PATH", "/tmp/pip-user/bin:/usr/local/bin:/usr/bin:/bin:"+os.Getenv("PATH"))

	containerEnv = env
	slog.Info("container env configured", "HOME", "/tmp/home", "PYTHONUSERBASE", "/tmp/pip-user")
}

// runInEnv executes a shell command with the container env.
func runInEnv(command, workDir string) (string, error) {
	cmd := exec.Command("sh", "-c", command)
	cmd.Dir = workDir
	cmd.Env = containerEnv
	out, err := cmd.CombinedOutput()
	return string(out), err
}

// extractMemoryValue reads a "key: value" line from the repo context string.
func extractMemoryValue(ctx, key string) string {
	prefix := key + ":"
	for _, line := range strings.Split(ctx, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "- "+prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, "- "+prefix))
		}
		if strings.HasPrefix(line, prefix) {
			return strings.TrimSpace(strings.TrimPrefix(line, prefix))
		}
	}
	return ""
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

func generateSuggestedQuestions(title, body, verdictText string) string {
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		return ""
	}
	prompt := "Based on this issue and reproduction results, suggest 5 specific follow-up questions " +
		"a developer would want to ask. Make them specific to THIS issue, not generic.\n\n" +
		"Issue title: " + title + "\nIssue body: " + truncStr(body, 500) + "\n" +
		"Reproduction verdict: " + truncStr(verdictText, 500) + "\n\n" +
		"Format as a JSON array of strings. Output ONLY the JSON array, nothing else."
	reply, err := ai.Call(prompt, 512)
	if err != nil || reply == "" {
		return ""
	}
	// Validate it's valid JSON array
	reply = strings.TrimSpace(reply)
	if !strings.HasPrefix(reply, "[") {
		// Try to extract array from response
		if idx := strings.Index(reply, "["); idx >= 0 {
			if end := strings.LastIndex(reply, "]"); end > idx {
				reply = reply[idx : end+1]
			}
		}
	}
	var arr []string
	if json.Unmarshal([]byte(reply), &arr) != nil {
		return ""
	}
	return reply
}

// generateInstallCommand asks the AI to create an install command on-the-spot
// by reading the project's dependency files.
func generateInstallCommand(cloneDir string) string {
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		return ""
	}

	// Read key dependency files
	var depsInfo string
	for _, f := range []string{"pyproject.toml", "setup.py", "requirements.txt", "setup.cfg", "go.mod", "package.json", "Makefile"} {
		data, err := os.ReadFile(cloneDir + "/" + f)
		if err == nil {
			content := string(data)
			if len(content) > 1000 {
				content = content[:1000]
			}
			depsInfo += fmt.Sprintf("--- %s ---\n%s\n", f, content)
		}
	}
	if depsInfo == "" {
		return ""
	}

	prompt := fmt.Sprintf(
		"Generate the MINIMAL install command for this project to run for API testing.\n\n"+
			"Project: %s\nFiles:\n%s\n\n"+
			"Constraints: rootless container, 512Mi RAM, NO GPU, python3 + pip available.\n"+
			"Use --user --break-system-packages for pip.\n"+
			"Use --no-deps if heavy deps (torch, tensorflow, vllm, cuda) are listed but not needed for testing.\n\n"+
			"Respond with ONLY the shell command on a single line. No explanation.",
		os.Getenv("REPO"), depsInfo,
	)

	reply, err := ai.Call(prompt, 256)
	if err != nil || reply == "" {
		return ""
	}

	for _, line := range strings.Split(strings.TrimSpace(reply), "\n") {
		line = strings.TrimSpace(line)
		if line != "" && !strings.HasPrefix(line, "```") && !strings.HasPrefix(line, "#") && len(line) > 3 {
			slog.Info("AI generated install command on-the-spot", "cmd", line)
			emitRepoMemory(map[string]string{"install_command": line})
			return line
		}
	}
	return ""
}

func isK8sProject() bool {
	profile := loadProfile()
	if profile == nil {
		return false
	}
	if b, ok := profile["k8s"].(bool); ok {
		return b
	}
	return false
}

func deployFromPlan(issueTitle, issueBody, planJSON string) string {
	var plan struct {
		Options []struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			Description string `json:"description"`
			BestFor     string `json:"best_for"`
			Recommended bool   `json:"recommended"`
			Steps       []struct {
				Type        string `json:"type"`
				Content     string `json:"content"`
				Required    bool   `json:"required"`
				Description string `json:"description"`
			} `json:"steps"`
		} `json:"options"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil || len(plan.Options) == 0 {
		slog.Warn("no deployment options in plan")
		return ""
	}

	// Ask AI which option is best for this issue
	selected := selectDeploymentOption(issueTitle, issueBody, plan.Options)
	if selected < 0 || selected >= len(plan.Options) {
		selected = 0
		for i, opt := range plan.Options {
			if opt.Recommended {
				selected = i
				break
			}
		}
	}

	opt := plan.Options[selected]
	slog.Info("selected deployment option", "option", opt.Name, "id", opt.ID)
	fmt.Printf("--- OPINAI SELECTED DEPLOYMENT: %s ---\n", opt.ID)

	// Since the runner can't create K8s namespaces (no RBAC), log the selection
	// and let the controller handle actual deployment on rerun.
	// If controller already created sandbox (OPINAI_SANDBOX_NAMESPACE), we'd use it above.
	// For now, emit the selection and attempt pip-based deployment as fallback.
	slog.Info("K8s deployment requires controller sandbox — falling back to local setup")
	proc, url := startServer()
	if proc != nil {
		// Store for cleanup — but we can't easily pass this back, so just return the URL
		go func() {
			time.Sleep(10 * time.Minute)
			proc.Kill()
		}()
	}
	return url
}

func selectDeploymentOption(title, body string, options []struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	BestFor     string `json:"best_for"`
	Recommended bool   `json:"recommended"`
	Steps       []struct {
		Type        string `json:"type"`
		Content     string `json:"content"`
		Required    bool   `json:"required"`
		Description string `json:"description"`
	} `json:"steps"`
}) int {
	cfg := ai.LoadConfig()
	if !cfg.Available() || len(options) <= 1 {
		return 0
	}

	var optText string
	for i, opt := range options {
		optText += fmt.Sprintf("- %d) %s (%s): %s (best for: %s)\n", i, opt.ID, opt.Name, opt.Description, opt.BestFor)
	}

	prompt := "You are OpinAI. A user filed this bug:\n\n" +
		"Title: " + title + "\nBody: " + body + "\n\n" +
		"Available deployment options:\n" + optText + "\n" +
		"Which option number (0-indexed) is best for this bug? " +
		"Respond with ONLY the number."

	reply, err := ai.Call(prompt, 64)
	if err != nil || reply == "" {
		return -1
	}
	n := 0
	fmt.Sscanf(strings.TrimSpace(reply), "%d", &n)
	if n >= 0 && n < len(options) {
		return n
	}
	return -1
}
