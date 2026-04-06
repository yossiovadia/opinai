// Package runner implements the reproduction flow that runs inside K8s Job pods.
package runner

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yossiovadia/opinai/controller-go/internal/agent"
	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/config"
	"github.com/yossiovadia/opinai/controller-go/internal/controller"
	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

// setupRetryInfo tracks self-healing retries during setup for inclusion in reports.
var setupRetryInfo string
var pendingInstallCmd string // saved only after server health confirmed
var selectedDeployOption string // set by deployFromPlan for repro_details
var collectedRepoMemory = map[string]string{} // accumulated for postResult

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

	// Always clone the repo — needed for agent code review, even in sandbox mode
	cloneRepo(repo)

	// Classify the issue — controls whether deployment is attempted
	repoCtxForClassify := os.Getenv("OPINAI_REPO_CONTEXT")
	needsDeployment, classificationReason, recommendedOption := classifyIssue(title, body, repoCtxForClassify)
	slog.Info("issue classified", "needs_deployment", needsDeployment, "reason", classificationReason, "recommended_option", recommendedOption)

	// Step 2: Start server, use sandbox, or deploy from plan
	serverURL := ""
	verifyFix := os.Getenv("OPINAI_VERIFY_FIX") == "true"
	var serverProc *os.Process
	var allEndpointsCtx string
	var deploymentFailureReason string

	// Skip deployment if classifier says code review is sufficient
	// (unless verify-fix mode forces deployment)
	skipDeployment := !needsDeployment && !verifyFix
	if skipDeployment {
		slog.Info("skipping deployment — issue classified as code review only")
	}

	if !skipDeployment {
		serverURL = os.Getenv("SERVER_URL")
		sandboxNS := os.Getenv("OPINAI_SANDBOX_NAMESPACE")
		sandboxEndpoints := os.Getenv("OPINAI_SANDBOX_ENDPOINTS")
		deploymentPlan := os.Getenv("OPINAI_DEPLOYMENT_PLAN")

		if verifyFix {
			slog.Info("verify-fix mode — forcing full deployment and testing")
		}

		if sandboxNS != "" {
			// Sandbox already created by controller
			slog.Info("using sandbox deployment", "namespace", sandboxNS)

			// Prefer test_endpoint from the deployment plan (specifies the right service to test)
			testEndpointJSON := os.Getenv("OPINAI_TEST_ENDPOINT")
			if testEndpointJSON != "" {
				var te struct {
					Service    string `json:"service"`
					Port       int    `json:"port"`
					Protocol   string `json:"protocol"`
					HealthPath string `json:"health_path"`
				}
				if json.Unmarshal([]byte(testEndpointJSON), &te) == nil && te.Service != "" {
					proto := te.Protocol
					if proto == "" {
						proto = "http"
					}
					port := te.Port
					if port == 0 {
						port = 80
					}
					serverURL = fmt.Sprintf("%s://%s.%s.svc.cluster.local:%d", proto, te.Service, sandboxNS, port)
					os.Setenv("SERVER_URL", serverURL)
					slog.Info("using test_endpoint from deployment plan", "url", serverURL, "health", te.HealthPath)
				}
			}

			// Fallback: pick first endpoint from the service map
			if serverURL == "" && sandboxEndpoints != "" {
				var endpoints map[string]string
				json.Unmarshal([]byte(sandboxEndpoints), &endpoints)
				for _, fqdn := range endpoints {
					serverURL = "http://" + fqdn
					os.Setenv("SERVER_URL", serverURL)
					break
				}
			}

			// Build all_endpoints context for the agent
			allEndpointsJSON := os.Getenv("OPINAI_ALL_ENDPOINTS")
			if allEndpointsJSON != "" {
				var eps []struct {
					Service string `json:"service"`
					Port    int    `json:"port"`
					Purpose string `json:"purpose"`
				}
				if json.Unmarshal([]byte(allEndpointsJSON), &eps) == nil && len(eps) > 0 {
					allEndpointsCtx = "\n\nSandbox services deployed for this project:\n"
					for _, ep := range eps {
						fqdn := fmt.Sprintf("%s.%s.svc.cluster.local:%d", ep.Service, sandboxNS, ep.Port)
						allEndpointsCtx += fmt.Sprintf("- %s (%s) — %s\n", ep.Service, fqdn, ep.Purpose)
					}
					allEndpointsCtx += fmt.Sprintf("\nTest traffic should go to: %s\n", serverURL)
				}
			}
		} else if deploymentPlan != "" {
			// Deploy from plan — AI determines steps
			serverURL = deployFromPlan(title, body, deploymentPlan)
			if serverURL != "" {
				os.Setenv("SERVER_URL", serverURL)
				slog.Info("deployed from plan", "server_url", serverURL)
			} else {
				deploymentFailureReason = "Deployment from plan failed — no server URL obtained"
			}
		} else {
			// Standard: start server in pod
			serverProc, serverURL = startServer()
			if serverURL != "" {
				os.Setenv("SERVER_URL", serverURL)
			} else {
				deploymentFailureReason = "Server startup failed — could not build or run the project"
			}
		}

		// Log deployment failure and fall back to code review
		if needsDeployment && serverURL == "" && sandboxNS == "" {
			slog.Warn("deployment requested but failed — falling back to code review", "reason", deploymentFailureReason)
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

	// Emit reproduction details for dashboard display
	issueComments := os.Getenv("OPINAI_ISSUE_COMMENTS")
	reproDetails := map[string]any{
		"issue_comments": issueComments,
		"cloned":         true,
		"server_started": serverURL != "",
		"issue_classification": func() string { if needsDeployment { return "runtime" }; return "code_review" }(),
		"classification_reason": classificationReason,
		"recommended_deploy_option": recommendedOption,
		"server_url":     serverURL,
	}
	sandboxNSForDetails := os.Getenv("OPINAI_SANDBOX_NAMESPACE")
	deploymentPlanForDetails := os.Getenv("OPINAI_DEPLOYMENT_PLAN")
	if sandboxNSForDetails != "" {
		reproDetails["method"] = "sandbox-deploy"
		reproDetails["deployment_option"] = "Sandbox: " + sandboxNSForDetails
	} else if skipDeployment {
		reproDetails["method"] = "code-review"
	} else if deploymentPlanForDetails != "" {
		reproDetails["method"] = "plan-deploy"
		if selectedDeployOption != "" {
			reproDetails["deployment_option"] = selectedDeployOption
		}
	} else if serverURL != "" {
		reproDetails["method"] = "live-deploy"
	} else {
		reproDetails["method"] = "code-review"
	}
	if deploymentFailureReason != "" {
		reproDetails["deployment_failure_reason"] = deploymentFailureReason
	}
	repoCtxForDetails := os.Getenv("OPINAI_REPO_CONTEXT")
	if bc := extractMemoryValue(repoCtxForDetails, "working_install_command"); bc != "" {
		reproDetails["build_command"] = bc
	}
	if rc := extractMemoryValue(repoCtxForDetails, "working_run_command"); rc != "" {
		reproDetails["run_command"] = rc
	}
	if dt := extractMemoryValue(repoCtxForDetails, "deployment_type"); dt != "" {
		reproDetails["deployment_type"] = dt
	}
	if ts := extractMemoryValue(repoCtxForDetails, "test_strategy"); ts != "" {
		reproDetails["test_strategy"] = ts
	}
	reproJSON, _ := json.Marshal(reproDetails)
	fmt.Println("--- OPINAI REPRODUCTION_DETAILS ---")
	fmt.Println(string(reproJSON))
	fmt.Println("--- END REPRODUCTION_DETAILS ---")

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
				"This appears to be a **%s**, not a reproducible bug. Skipping reproduction.",
			issueNumber, category, verdictEnum, os.Getenv("AI_MODEL"), catLabels[category],
		)
		postComment(repo, atoi(issueNumber), comment)
		emitRepoMemory(map[string]string{
			"last_analyzed_issue": issueNumber,
			"last_verdict":       verdictEnum,
		})
		postResult(map[string]any{
			"repo": repo, "issue": atoi(issueNumber), "title": title,
			"category": category, "verdict": verdictEnum, "confidence": "HIGH",
			"report": comment, "repro_details": string(reproJSON),
			"repo_memory": collectedRepoMemory,
		})
		slog.Info("skipped reproduction", "verdict", verdictEnum)
		return
	}

	// Step 4-5: Agent investigation (with fallback to legacy flow)
	repoCtx := os.Getenv("OPINAI_REPO_CONTEXT")
	richCtx := formatRichRepoContext(repoCtx)
	var stateCtx string
	if issueState == "closed" {
		stateCtx = "\n\nThis issue is CLOSED (presumably fixed). Analyze whether the fix is correct. " +
			"If the bug no longer exists, the tests should pass. If it still exists despite being closed, that is a regression.\n"
	} else {
		stateCtx = "\n\nThis is an OPEN issue. Reproduce the bug and confirm or deny it.\n"
	}
	agentRepoCtx := richCtx + stateCtx + allEndpointsCtx

	// Include issue comments in the body passed to the agent
	issueBody := body
	if commentsJSON := os.Getenv("OPINAI_ISSUE_COMMENTS"); commentsJSON != "" {
		var comments []struct {
			Author string `json:"author"`
			Body   string `json:"body"`
		}
		if json.Unmarshal([]byte(commentsJSON), &comments) == nil && len(comments) > 0 {
			issueBody += "\n\n---\n\n## Issue Comments\n"
			for _, c := range comments {
				issueBody += fmt.Sprintf("\n**@%s:**\n%s\n", c.Author, truncStr(c.Body, 500))
			}
		}
	}

	hasRich := strings.Contains(richCtx, "## Project Analysis:")
	slog.Info("starting agent investigation", "has_rich_context", hasRich, "context_bytes", len(agentRepoCtx))
	agentResult := agent.Investigate(title, issueBody, serverURL, "/tmp/opinai-repo", agentRepoCtx, 0) // 0 = use default (200)

	var vr ai.VerdictResult
	var script string
	var testOutput string
	var resultsTable string
	retryCount := 0

	if agentResult.Verdict != "" && agentResult.Verdict != "INCONCLUSIVE" {
		// Agent produced a definitive verdict
		slog.Info("agent investigation succeeded", "verdict", agentResult.Verdict, "confidence", agentResult.Confidence)
		vr = ai.VerdictResult{
			Verdict:    agentResult.Verdict,
			Confidence: agentResult.Confidence,
			Text:       agentResult.Report,
		}
		script = agentResult.TestScript
		testOutput = agentResult.TestOutput
		if testOutput == "" {
			testOutput = agentResult.Report
		}
		resultsTable = parseResultsTable(testOutput)

		reproDetails["investigation_method"] = "agent"
		reproDetails["agent_iterations"] = agentResult.Iterations
		reproDetails["agent_tool_calls"] = agentResult.ToolCalls
		if len(agentResult.FilesRead) > 0 {
			reproDetails["files_investigated"] = agentResult.FilesRead
		}
	} else {
		// Agent failed or was inconclusive — fall back to legacy flow
		slog.Warn("agent investigation inconclusive, falling back to legacy flow", "report", truncStr(agentResult.Report, 200))
		reproDetails["investigation_method"] = "legacy"

		profileCtx := loadProfileContext()

		// Tell the AI it has server control for config-dependent bugs
		profile2 := config.LoadRepoProfile(os.Getenv("REPO"))
		runCommand := ""
		if profile2 != nil {
			runCommand, _ = profile2["run"].(string)
		}
		if runCommand != "" && serverURL != "" {
			agentRepoCtx += fmt.Sprintf(
				"\n\nYou have full control of the server. If the bug requires restarting with different "+
					"configuration (env vars, flags), you can kill the existing server and restart it.\n"+
					"Current start command: %s\n"+
					"Server binary directory: /tmp/opinai-repo\n"+
					"Environment: PYTHONUSERBASE=/tmp/pip-user HOME=/tmp/home\n", runCommand)
		}

		repoMemCtx := os.Getenv("OPINAI_REPO_CONTEXT")
		testStrategy := extractMemoryValue(repoMemCtx, "test_strategy")
		needsCluster := extractMemoryValue(repoMemCtx, "needs_cluster")
		sandboxNS2 := os.Getenv("OPINAI_SANDBOX_NAMESPACE")

		if needsCluster == "true" && sandboxNS2 == "" && serverURL == "" {
			if testStrategy == "" || testStrategy == "deploy-and-curl" {
				testStrategy = "code-review"
			}
			agentRepoCtx += "\n\nThis project requires Kubernetes but no cluster deployment is available. " +
				"Use a CODE REVIEW strategy: analyze the source code to determine if the reported bug exists. " +
				"Check the relevant source files, look for the described behavior, and give your verdict " +
				"based on code analysis rather than runtime testing.\n" +
				"The project source is at: /tmp/opinai-repo\n" +
				"You can use: find, grep, cat to examine the code.\n"
		}
		if testStrategy != "" && testStrategy != "code-review" {
			agentRepoCtx += fmt.Sprintf("\n\nRecommended test strategy: %s\n", testStrategy)
		}

		script, err = ai.GenerateTests(title, body, serverURL, profileCtx, agentRepoCtx)
		if err != nil || script == "" {
			fmt.Println("--- OPINAI VERDICT: ERROR ---")
			comment := fmt.Sprintf(
				"## OpinAI Bug Reproduction Report\n\n"+
					"**Issue:** #%s\n"+
					"**Category:** %s\n"+
					"**Verdict:** ERROR\n"+
					"**Analysis:** Skipped (AI analysis failed)\n\n"+
					"Could not generate tests for this issue.",
				issueNumber, category,
			)
			postComment(repo, atoi(issueNumber), comment)
			postResult(map[string]any{
				"repo": repo, "issue": atoi(issueNumber), "title": title,
				"category": category, "verdict": "ERROR", "confidence": "LOW",
				"report": comment, "repro_details": string(reproJSON),
				"repo_memory": collectedRepoMemory,
			})
			return
		}
		slog.Info("test script generated", "bytes", len(script))

		critiqued, critiqueErr := ai.CritiqueTest(script, title, body)
		if critiqueErr != nil {
			slog.Warn("critique failed, using original script", "error", critiqueErr)
		} else if critiqued != script {
			slog.Info("AI critique returned corrected script", "bytes", len(critiqued))
			script = critiqued
		} else {
			slog.Info("AI approved test script")
		}

		slog.Info("running tests...")
		testOutput = runTests(script)
		const maxRetries = 2

		for attempt := 0; attempt < maxRetries; attempt++ {
			rt := parseResultsTable(testOutput)
			hasResults := !strings.Contains(rt, "(no structured results)")
			if hasResults || !strings.Contains(testOutput, "[script exited with error:") {
				break
			}
			slog.Warn("test script crashed, requesting AI fix", "attempt", attempt+1)
			fixedScript, fixErr := ai.RegenerateTest(title, body, script, truncStr(testOutput, 3000))
			if fixErr != nil || fixedScript == "" {
				slog.Warn("AI could not fix crashed script", "error", fixErr)
				break
			}
			script = fixedScript
			testOutput = runTests(script)
			retryCount++
		}

		slog.Info("tests completed", "output_bytes", len(testOutput), "retries", retryCount)

		resultsTable = parseResultsTable(testOutput)
		hasStructuredResults := !strings.Contains(resultsTable, "(no structured results)")
		verdictInput := testOutput
		if !hasStructuredResults {
			verdictInput += "\n\nWARNING: The test script produced NO structured JSON test results. " +
				"This likely means the script crashed before completing its tests. " +
				"Your confidence should be LOW unless the raw output clearly proves the bug exists or not."
			slog.Warn("test script produced no structured results")
		}

		vr = ai.GetVerdict(title, body, verdictInput, issueState)
	}

	// Save the final test script into repro_details
	if script != "" {
		reproDetails["test_script"] = script
	}
	if retryCount > 0 {
		reproDetails["test_retries"] = retryCount
	}

	// Generate plain-language summary
	summary := generateSummary(title, body, vr.Verdict, vr.Confidence, vr.Text)
	if summary != "" {
		reproDetails["summary"] = summary
	}

	reproJSON, _ = json.Marshal(reproDetails)
	fmt.Println("--- OPINAI REPRODUCTION_DETAILS ---")
	fmt.Println(string(reproJSON))
	fmt.Println("--- END REPRODUCTION_DETAILS ---")

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
	serverInfo := ""
	if serverURL != "" {
		serverInfo = fmt.Sprintf("**Server:** `%s`\n", serverURL)
	}
	retryInfo := ""
	if setupRetryInfo != "" {
		retryInfo = fmt.Sprintf("**Setup:** %s\n", setupRetryInfo)
	}
	if retryCount > 0 {
		retryInfo += fmt.Sprintf("**Test retries:** %d (script crashed and was regenerated)\n", retryCount)
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
			"%s"+
			"### Verdict\n\n"+
			"%s\n\n"+
			"<details><summary>Raw test output</summary>\n\n"+
			"```\n%s\n```\n\n"+
			"</details>",
		issueNumber, category, vr.Verdict, vr.Confidence, serverInfo, retryInfo,
		os.Getenv("AI_MODEL"), formatResultsSection(resultsTable), verdictText, truncStr(testOutput, 5000),
	)

	postComment(repo, atoi(issueNumber), comment)
	emitRepoMemory(map[string]string{
		"last_analyzed_issue": issueNumber,
		"last_verdict":       vr.Verdict,
		"last_confidence":    vr.Confidence,
	})
	postResult(map[string]any{
		"repo": repo, "issue": atoi(issueNumber), "title": title,
		"category": category, "verdict": vr.Verdict, "confidence": vr.Confidence,
		"duration": "", "report": comment,
		"suggested_questions": suggestedQs, "repro_details": string(reproJSON),
		"repo_memory": collectedRepoMemory,
	})
	slog.Info("reproduction complete", "repo", repo, "issue", issueNumber)
}

// --- helpers ---

// cloneRepo clones the repo to /tmp/opinai-repo. Safe to call multiple times
// (skips if already cloned).
func cloneRepo(repo string) bool {
	cloneDir := "/tmp/opinai-repo"
	if info, err := os.Stat(cloneDir); err == nil && info.IsDir() {
		slog.Info("repo already cloned", "dir", cloneDir)
		return true
	}
	slog.Info("cloning repo", "repo", repo)
	cloneCtx, cloneCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cloneCancel()
	cmd := exec.CommandContext(cloneCtx, "git", "clone", "--depth=1", "https://github.com/"+repo+".git", cloneDir)
	cmd.Stdout = os.Stderr
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		slog.Warn("clone failed", "error", err)
		return false
	}
	return true
}

func startServer() (*os.Process, string) {
	repo := os.Getenv("REPO")
	cloneDir := "/tmp/opinai-repo"

	// Ensure repo is cloned (may already be done by Run)
	if !cloneRepo(repo) {
		return nil, ""
	}

	// ALWAYS analyze — determines deployment type and install command
	readmeAnalysis := analyzeReadme(cloneDir)

	// Set up writable container environment
	setupContainerEnv()

	// Resolve build + run commands from multiple sources (ascending priority)
	// Sources: profile → env var → AI analysis (this run) → repo memory
	profile := config.LoadRepoProfile(os.Getenv("REPO"))
	buildCmd := ""
	runCmd := ""
	healthURL := ""
	if profile != nil {
		buildCmd, _ = profile["build"].(string)
		runCmd, _ = profile["run"].(string)
		healthURL, _ = profile["health"].(string)
	}
	if envCmd := os.Getenv("OPINAI_INSTALL_COMMAND"); envCmd != "" {
		buildCmd = envCmd
	}
	if readmeAnalysis != nil {
		if v := readmeAnalysis["install_command"]; v != "" {
			buildCmd = v
		}
		// AI-generated build/run commands override profile
		if v := readmeAnalysis["build_command"]; v != "" {
			buildCmd = v
		}
		if v := readmeAnalysis["run_command"]; v != "" && v != "none" {
			runCmd = v
		} else if v == "none" {
			runCmd = "" // explicitly no server
		}
	}
	repoCtx := os.Getenv("OPINAI_REPO_CONTEXT")
	if v := extractMemoryValue(repoCtx, "install_command"); v != "" {
		buildCmd = v
	}
	if v := extractMemoryValue(repoCtx, "working_install_command"); v != "" {
		buildCmd = v
	}
	if v := extractMemoryValue(repoCtx, "working_run_command"); v != "" {
		runCmd = v
	}

	slog.Info("resolved commands", "build", truncStr(buildCmd, 80), "run", truncStr(runCmd, 80))

	// Install chain (ascending priority — last match wins):
	// profile build → env var → README analysis → repo memory analyzed → repo memory working
	{
		repoCtx := os.Getenv("OPINAI_REPO_CONTEXT")
		installCmd := buildCmd

		if planCmd := os.Getenv("OPINAI_INSTALL_COMMAND"); planCmd != "" {
			installCmd = planCmd
			slog.Info("using deployment plan install command", "cmd", installCmd)
		}
		// From README analysis on THIS run (not yet in repo memory)
		if readmeAnalysis != nil {
			if cmd, ok := readmeAnalysis["install_command"]; ok && cmd != "" {
				installCmd = cmd
				slog.Info("using install command from README analysis", "cmd", installCmd)
			}
		}
		// From previous run's analysis (in repo memory via controller)
		if analyzed := extractMemoryValue(repoCtx, "install_command"); analyzed != "" {
			installCmd = analyzed
			slog.Info("using AI-analyzed install command from memory", "cmd", installCmd)
		}
		// Proven working command (highest priority)
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
						slog.Info("AI fix worked — will save after server health check")
						pendingInstallCmd = fixedCmd // track for later save
						setupRetryInfo = "AI fixed install: " + truncStr(fixedCmd, 80)
					} else {
						slog.Warn("AI fix also failed, trying fresh generation", "error", err2)

						// Step 4: generate from scratch by reading repo files
						freshCmd := generateInstallCommand(cloneDir)
						if freshCmd != "" && freshCmd != installCmd && freshCmd != fixedCmd {
							slog.Info("trying AI-generated fresh install", "cmd", freshCmd)
							_, err3 := runInEnv(freshCmd, cloneDir)
							if err3 == nil {
								slog.Info("fresh install worked — will save after health check")
								pendingInstallCmd = freshCmd
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
							slog.Info("fresh install worked — will save after health check")
							pendingInstallCmd = freshCmd
							setupRetryInfo = "AI generated install: " + truncStr(freshCmd, 80)
						}
					}
				}
			}
		}
	}

	if runCmd == "" {
		return nil, ""
	}

	// Start server
	slog.Info("starting server", "cmd", runCmd)
	cmd := exec.Command("sh", "-c", runCmd)
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
			// Only save working_install_command AFTER server is confirmed healthy
			if pendingInstallCmd != "" {
				emitRepoMemory(map[string]string{"working_install_command": pendingInstallCmd})
				slog.Info("saved working install command (server healthy)", "cmd", truncStr(pendingInstallCmd, 80))
			}
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
	tmpFile := "/tmp/opinai_test.py"
	os.WriteFile(tmpFile, []byte(script), 0o644)
	defer os.Remove(tmpFile)

	cmd := exec.Command("python3", tmpFile)
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

func emitRepoMemory(data map[string]string) {
	b, _ := json.Marshal(data)
	fmt.Println("--- OPINAI REPO MEMORY ---")
	fmt.Println(string(b))
	fmt.Println("--- END REPO MEMORY ---")
	// Also collect for direct callback
	for k, v := range data {
		if v != "" {
			collectedRepoMemory[k] = v
		}
	}
}

// postResult sends the reproduction result directly to the controller API.
// Falls back silently if the controller URL is not set or the POST fails —
// the log-scraping harvester will pick up the result as a fallback.
func postResult(result map[string]any) {
	controllerURL := os.Getenv("OPINAI_CONTROLLER_URL")
	if controllerURL == "" {
		return
	}
	b, _ := json.Marshal(result)
	resp, err := http.Post(controllerURL+"/api/internal/result", "application/json", strings.NewReader(string(b)))
	if err != nil {
		slog.Warn("failed to POST result to controller", "error", err)
		return
	}
	resp.Body.Close()
	if resp.StatusCode == 200 {
		slog.Info("result posted to controller successfully")
	} else {
		slog.Warn("controller returned non-200 for result", "status", resp.StatusCode)
	}
}

func loadProfileContext() string {
	profile := config.LoadRepoProfile(os.Getenv("REPO"))
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

func analyzeReadme(cloneDir string) map[string]string {
	if os.Getenv("OPINAI_HAS_KNOWLEDGE") == "true" {
		return nil
	}
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		return nil
	}

	// Read README
	readmePath := cloneDir + "/README.md"
	data, err := os.ReadFile(readmePath)
	if err != nil {
		readmePath = cloneDir + "/readme.md"
		data, err = os.ReadFile(readmePath)
		if err != nil {
			return nil
		}
	}
	readme := string(data)
	if len(readme) > 3000 {
		readme = readme[:3000]
	}

	// Read dependency files
	depsInfo := ""
	for _, depFile := range []string{
		"pyproject.toml", "setup.py", "requirements.txt", "setup.cfg",
		"go.mod", "package.json", "Cargo.toml", "Gemfile",
	} {
		depData, err := os.ReadFile(filepath.Join(cloneDir, depFile))
		if err == nil {
			c := string(depData)
			if len(c) > 1500 {
				c = c[:1500]
			}
			depsInfo += fmt.Sprintf("\n--- %s ---\n%s\n", depFile, c)
		}
	}

	// Scan for deployment/infrastructure indicators
	deployIndicators := []string{
		"Dockerfile", "docker-compose.yml", "docker-compose.yaml",
		"Makefile", "Taskfile.yml",
		"kustomization.yaml", "kustomization.yml",
		"deployment/kustomization.yaml", "deploy/kustomization.yaml",
		"config/manager/kustomization.yaml",
		"helm/Chart.yaml", "charts/Chart.yaml", "chart/Chart.yaml",
		"scripts/deploy.sh", "scripts/install.sh", "deploy.sh",
		"config/crd", "config/rbac", "config/samples",
		"bundle/manifests", "operator.yaml",
		"skaffold.yaml", "tilt_config.json",
	}
	var foundIndicators []string
	for _, ind := range deployIndicators {
		target := filepath.Join(cloneDir, ind)
		if info, err := os.Stat(target); err == nil {
			kind := "file"
			if info.IsDir() {
				kind = "dir"
			}
			foundIndicators = append(foundIndicators, ind+" ("+kind+")")
		}
	}

	// Read key deployment files content (first 1000 chars each)
	deployFileContents := ""
	for _, df := range []string{
		"Makefile", "Dockerfile", "kustomization.yaml",
		"deployment/kustomization.yaml", "config/manager/kustomization.yaml",
	} {
		dfData, err := os.ReadFile(filepath.Join(cloneDir, df))
		if err == nil {
			c := string(dfData)
			if len(c) > 1000 {
				c = c[:1000]
			}
			deployFileContents += fmt.Sprintf("\n--- %s ---\n%s\n", df, c)
		}
	}

	indicatorsStr := "none found"
	if len(foundIndicators) > 0 {
		indicatorsStr = strings.Join(foundIndicators, ", ")
	}

	slog.Info("analyzing project", "repo", os.Getenv("REPO"), "deploy_indicators", len(foundIndicators))
	prompt := prompts.Render("analyze_readme.txt", map[string]string{
		"Repo": os.Getenv("REPO"), "Readme": readme, "DepsInfo": depsInfo,
		"Indicators": indicatorsStr, "DeployContents": deployFileContents,
	})
	content, err := ai.Call(prompt, 1500)
	if err != nil || content == "" {
		return nil
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
		return result
	}

	// Try to extract JSON object from response text (AI may wrap it in explanation)
	if start := strings.Index(content, "{"); start >= 0 {
		if end := strings.LastIndex(content, "}"); end > start {
			candidate := content[start : end+1]
			if json.Unmarshal([]byte(candidate), &result) == nil {
				emitRepoMemory(result)
				slog.Info("emitted README knowledge (extracted from text)", "keys", len(result))
				return result
			}
		}
	}

	slog.Warn("could not parse AI analysis as JSON, storing raw description only")
	emitRepoMemory(map[string]string{"description": truncStr(content, 500)})
	return nil
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
	prompt := prompts.Render("suggested_questions.txt", map[string]string{
		"Title": title, "Body": truncStr(body, 500), "Verdict": truncStr(verdictText, 500),
	})
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

// classifyIssue makes a quick AI call to determine whether an issue needs runtime
// testing or code review only. Returns (needsDeployment, reason, recommendedOption).
func classifyIssue(title, body, repoContext string) (bool, string, string) {
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		return false, "no AI provider", ""
	}
	ctx := ""
	if repoContext != "" {
		ctx = "\n\nProject context:\n" + truncStr(repoContext, 500)
	}

	// Extract deployment option names from the deployment plan if available
	optionsCtx := ""
	if planJSON := os.Getenv("OPINAI_DEPLOYMENT_PLAN"); planJSON != "" {
		var plan struct {
			Options []struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"options"`
		}
		if json.Unmarshal([]byte(planJSON), &plan) == nil && len(plan.Options) > 0 {
			optionsCtx = "\n\nAvailable deployment options:"
			for _, opt := range plan.Options {
				optionsCtx += fmt.Sprintf("\n- %s (%s)", opt.ID, opt.Name)
			}
		}
	}

	prompt := fmt.Sprintf(`Given this issue, classify whether reproducing the bug requires a RUNNING SERVER (API behavior, HTTP responses, timing, concurrency, runtime errors, integration failures) or CODE REVIEW ONLY (logic bugs, missing null checks, wrong conditions, config parsing, type errors, documentation).

Reply with exactly one line:
NEEDS_DEPLOYMENT [option-id]: <brief reason>
or
CODE_REVIEW: <brief reason>

If deployment is needed and deployment options are listed below, include the best option id in brackets. If no options are listed, omit the brackets.%s

Issue Title: %s

Issue Description:
%s%s`, optionsCtx, title, truncStr(body, 800), ctx)

	reply, err := ai.Call(prompt, 128)
	if err != nil {
		slog.Warn("issue classification failed", "error", err)
		return false, "classification unavailable", ""
	}
	reply = strings.TrimSpace(reply)
	upper := strings.ToUpper(reply)
	if strings.HasPrefix(upper, "NEEDS_DEPLOYMENT") {
		rest := strings.TrimPrefix(reply, reply[:len("NEEDS_DEPLOYMENT")])
		rest = strings.TrimSpace(rest)
		// Parse optional [option-id]
		recommendedOption := ""
		if strings.HasPrefix(rest, "[") {
			if end := strings.Index(rest, "]"); end > 0 {
				recommendedOption = rest[1:end]
				rest = strings.TrimSpace(rest[end+1:])
			}
		}
		rest = strings.TrimPrefix(rest, ":")
		reason := strings.TrimSpace(rest)
		return true, reason, recommendedOption
	}
	if strings.HasPrefix(upper, "CODE_REVIEW:") {
		reason := strings.TrimSpace(reply[len("CODE_REVIEW:"):])
		return false, reason, ""
	}
	return false, reply, ""
}

// generateSummary creates a plain-language summary of the investigation for non-technical readers.
func generateSummary(title, body, verdict, confidence, report string) string {
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		return ""
	}
	prompt := fmt.Sprintf(`You are OpinAI. Summarize your investigation in 3-5 paragraphs of plain language.
Explain what you did, what you found, and why you reached this verdict.
Write as if briefing a senior engineer who hasn't read the code.
Be specific about files, functions, and evidence.
No code blocks, no JSON, no curl commands — just clear narrative.

Issue Title: %s

Issue Description:
%s

Verdict: %s (Confidence: %s)

Investigation Report:
%s`, title, truncStr(body, 1000), verdict, confidence, truncStr(report, 3000))

	reply, err := ai.Call(prompt, 1024)
	if err != nil {
		slog.Warn("failed to generate summary", "error", err)
		return ""
	}
	slog.Info("generated plain-language summary", "bytes", len(reply))
	return ai.Sanitize(reply)
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

	prompt := prompts.Render("generate_install.txt", map[string]string{
		"Repo": os.Getenv("REPO"), "DepsInfo": depsInfo,
	})

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

// formatRichRepoContext enhances the raw repo context with structured information
// from the rich_analysis key if available, falling back to the raw context.
func formatRichRepoContext(rawCtx string) string {
	richJSON := extractMemoryValue(rawCtx, "rich_analysis")
	if richJSON == "" {
		return rawCtx
	}

	var analysis agent.RepoAnalysis
	if err := json.Unmarshal([]byte(richJSON), &analysis); err != nil {
		return rawCtx
	}

	formatted := analysis.FormatContext()
	if formatted == "" {
		return rawCtx
	}

	// Prepend rich context, then include the raw context for any keys
	// not covered by the structured analysis (e.g. working_install_command)
	return "## Project Analysis:\n" + formatted + "\n## Additional context:\n" + rawCtx
}

func deployFromPlan(issueTitle, issueBody, planJSON string) string {
	var plan struct {
		Options []struct {
			ID          any    `json:"id"`
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
	selectedDeployOption = opt.Name
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
	ID          any    `json:"id"`
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

	prompt := prompts.Render("select_deployment.txt", map[string]string{
		"Title": title, "Body": body, "Options": optText,
	})

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

func formatResultsSection(resultsTable string) string {
	if strings.Contains(resultsTable, "(no structured results)") || strings.TrimSpace(resultsTable) == "" {
		return "" // Skip the section entirely for agent-mode investigations
	}
	return "### Results\n\n" +
		"| Test | Status | Details |\n" +
		"|------|--------|---------|\n" +
		resultsTable + "\n"
}
