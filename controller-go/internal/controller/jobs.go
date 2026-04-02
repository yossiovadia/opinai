package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"

	"github.com/yossiovadia/opinai/controller-go/internal/database"
	"github.com/yossiovadia/opinai/controller-go/internal/sandbox"
)

// JobManager handles K8s Job creation and result harvesting.
type JobManager struct {
	client    kubernetes.Interface
	namespace string
	image     string
	recorded  map[string]bool // tracks jobs already harvested this session
	sandbox   *sandbox.Manager
}

// NewJobManager creates a new JobManager.
func NewJobManager(client kubernetes.Interface, namespace, image string) *JobManager {
	return &JobManager{
		client:    client,
		namespace: namespace,
		image:     image,
		recorded:  make(map[string]bool),
		sandbox:   sandbox.NewManager(client, namespace),
	}
}

// JobName generates the K8s job name for a repo+issue.
func JobName(repo string, issue int) string {
	safe := strings.ToLower(strings.ReplaceAll(repo, "/", "-"))
	return fmt.Sprintf("opinai-%s-%d", safe, issue)
}

// CreateReproductionJob creates a K8s Job to reproduce an issue.
func (jm *JobManager) CreateReproductionJob(repo string, issueNumber int, issueTitle string) error {
	name := JobName(repo, issueNumber)
	ctx := context.Background()

	// Check if job already exists
	_, err := jm.client.BatchV1().Jobs(jm.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		slog.Info("job already exists, skipping", "job", name)
		return nil
	}

	repoSafe := strings.ToLower(strings.ReplaceAll(repo, "/", "-"))
	backoff := int32(0)
	ttl := int32(3600)

	// Build repo context from DB
	repoContext := buildRepoContext(repo)
	hasKnowledge := "false"
	if mem, _ := database.GetRepoMemory(repo, strPtr("description")); len(mem) > 0 {
		hasKnowledge = "true"
	}

	// Collect REPO_PROFILE_* env vars
	profileEnvs := collectProfileEnvVars()

	// Check if repo needs K8s sandbox deployment
	sandboxNS, sandboxEndpointsJSON, deploymentPlanJSON := jm.trySandboxDeploy(repo, issueNumber, issueTitle)

	// Build env vars list
	env := []corev1.EnvVar{
		{Name: "REPO", Value: repo},
		{Name: "ISSUE_NUMBER", Value: fmt.Sprintf("%d", issueNumber)},
		{Name: "OPINAI_AUTO_POST", Value: "false"},
		{Name: "OPINAI_VERIFY_FIX", Value: os.Getenv("_OPINAI_VERIFY_FIX_PENDING")},
		{Name: "OPINAI_REPO_CONTEXT", Value: repoContext},
		{Name: "OPINAI_HAS_KNOWLEDGE", Value: hasKnowledge},
		{Name: "OPINAI_SANDBOX_NAMESPACE", Value: sandboxNS},
		{Name: "OPINAI_SANDBOX_ENDPOINTS", Value: sandboxEndpointsJSON},
		{Name: "OPINAI_DEPLOYMENT_PLAN", Value: truncateStr(deploymentPlanJSON, 30000)},
		{Name: "GOOGLE_APPLICATION_CREDENTIALS", Value: "/var/run/secrets/gcp/credentials.json"},
	}
	env = append(env, secretEnvVar("AI_PROVIDER", "opinai-credentials", "AI_PROVIDER")...)
	env = append(env, secretEnvVar("AI_PROJECT", "opinai-credentials", "AI_PROJECT")...)
	env = append(env, secretEnvVar("AI_REGION", "opinai-credentials", "AI_REGION")...)
	env = append(env, secretEnvVar("AI_MODEL", "opinai-credentials", "AI_MODEL")...)
	env = append(env, profileEnvs...)

	// Title truncated for annotation (max 253 chars)
	titleTrunc := issueTitle
	if len(titleTrunc) > 253 {
		titleTrunc = titleTrunc[:253]
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: jm.namespace,
			Labels: map[string]string{
				"app":          "opinai-runner",
				"opinai/repo":  repoSafe,
				"opinai/issue": fmt.Sprintf("%d", issueNumber),
			},
			Annotations: map[string]string{
				"opinai/title":             titleTrunc,
				"opinai/repo-full":         repo,
				"opinai/sandbox-namespace": sandboxNS,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			TTLSecondsAfterFinished: &ttl,
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					ServiceAccountName: "opinai-controller",
					RestartPolicy:      corev1.RestartPolicyNever,
					Volumes: []corev1.Volume{
						{
							Name: "gcp-credentials",
							VolumeSource: corev1.VolumeSource{
								Secret: &corev1.SecretVolumeSource{
									SecretName: "opinai-gcp-credentials",
									Optional:   boolPtr(true),
								},
							},
						},
					},
					Containers: []corev1.Container{
						{
							Name:            "runner",
							Image:           jm.image,
							ImagePullPolicy: corev1.PullAlways,
							Command:         []string{"/app/opinai-go", "--mode=runner"},
							Env:             env,
							EnvFrom: []corev1.EnvFromSource{
								{SecretRef: &corev1.SecretEnvSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: "opinai-credentials"},
								}},
								{ConfigMapRef: &corev1.ConfigMapEnvSource{
									LocalObjectReference: corev1.LocalObjectReference{Name: "opinai-config"},
									Optional:             boolPtr(true),
								}},
							},
							VolumeMounts: []corev1.VolumeMount{
								{Name: "gcp-credentials", MountPath: "/var/run/secrets/gcp", ReadOnly: true},
							},
							Resources: corev1.ResourceRequirements{
								Requests: corev1.ResourceList{
									corev1.ResourceCPU:    mustParseQuantity("100m"),
									corev1.ResourceMemory: mustParseQuantity("256Mi"),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    mustParseQuantity("500m"),
									corev1.ResourceMemory: mustParseQuantity("512Mi"),
								},
							},
						},
					},
				},
			},
		},
	}

	_, err = jm.client.BatchV1().Jobs(jm.namespace).Create(ctx, job, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create job %s: %w", name, err)
	}

	database.MarkProcessed(repo, issueNumber, name)
	slog.Info("created job", "job", name, "repo", repo, "issue", issueNumber, "title", issueTitle)
	return nil
}

// HarvestCompletedJobs scans finished Jobs, extracts results, stores in DB.
func (jm *JobManager) HarvestCompletedJobs() {
	ctx := context.Background()
	jobs, err := jm.client.BatchV1().Jobs(jm.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=opinai-runner",
	})
	if err != nil {
		slog.Error("failed to list jobs", "error", err)
		return
	}

	for _, job := range jobs.Items {
		name := job.Name
		finished := job.Status.Succeeded > 0 || job.Status.Failed > 0
		if !finished {
			continue
		}
		if jm.recorded[name] {
			continue
		}
		jm.recorded[name] = true

		annotations := job.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		labels := job.Labels
		if labels == nil {
			labels = map[string]string{}
		}

		repo := annotations["opinai/repo-full"]
		if repo == "" {
			repo = labels["opinai/repo"]
		}
		issueStr := labels["opinai/issue"]
		title := annotations["opinai/title"]

		succeeded := job.Status.Succeeded > 0
		if succeeded {
			slog.Info("job succeeded", "job", name)
		} else {
			slog.Warn("job failed", "job", name)
		}

		// Compute duration
		duration := ""
		if job.Status.StartTime != nil && job.Status.CompletionTime != nil {
			delta := job.Status.CompletionTime.Sub(job.Status.StartTime.Time)
			secs := int(delta.Seconds())
			if secs >= 60 {
				duration = fmt.Sprintf("%dm %ds", secs/60, secs%60)
			} else {
				duration = fmt.Sprintf("%ds", secs)
			}
		}

		// Read pod logs
		podLogs := jm.readPodLogs(ctx, name)

		// Parse markers
		category := parseMarker(podLogs, "--- OPINAI CATEGORY:", "BUG")
		confidence := parseMarker(podLogs, "--- OPINAI CONFIDENCE:", "")
		verdict := parseVerdictMarker(podLogs, category)
		suggestedComment := extractBlock(podLogs, "--- OPINAI SUGGESTED COMMENT ---", "--- END SUGGESTED COMMENT ---")

		// Parse and store repo memory
		storeRepoMemory(repo, podLogs)

		// Determine issue number
		issue := 0
		fmt.Sscanf(issueStr, "%d", &issue)

		// Timestamp
		ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		if job.Status.CompletionTime != nil {
			ts = job.Status.CompletionTime.Format("2006-01-02T15:04:05Z")
		}

		// Report: prefer suggested comment, fall back to last 3000 chars of logs
		report := suggestedComment
		if report == "" && len(podLogs) > 3000 {
			report = podLogs[len(podLogs)-3000:]
		} else if report == "" {
			report = podLogs
		}
		if report == "" {
			report = "(no logs)"
		}

		database.AddRun(database.Run{
			Repo:       repo,
			Issue:      issue,
			Title:      title,
			Verdict:    verdict,
			Category:   category,
			Confidence: confidence,
			AIPowered:  true,
			Duration:   duration,
			Posted:     false,
			Report:     report,
			CreatedAt:  ts,
		})

		// Teardown sandbox if one was used
		sbNS := annotations["opinai/sandbox-namespace"]
		if sbNS != "" && jm.sandbox != nil {
			if jm.sandbox.TeardownSandbox(sbNS) {
				slog.Info("torn down sandbox after job", "namespace", sbNS, "job", name)
			}
		}
	}
}

// CreateVerifyFixJob creates a Job with OPINAI_VERIFY_FIX=true.
func (jm *JobManager) CreateVerifyFixJob(repo string, issueNumber int, issueTitle string) error {
	// Delete any existing job for this issue first (force re-run)
	name := JobName(repo, issueNumber)
	bg := metav1.DeletePropagationBackground
	jm.client.BatchV1().Jobs(jm.namespace).Delete(
		context.Background(), name, metav1.DeleteOptions{PropagationPolicy: &bg},
	)
	// Wait briefly for deletion
	time.Sleep(2 * time.Second)

	// Override the OPINAI_AUTO_POST env to include verify flag
	// We reuse CreateReproductionJob but need to inject the extra env var
	// The simplest approach: set a temp env var that CreateReproductionJob picks up
	os.Setenv("_OPINAI_VERIFY_FIX_PENDING", "true")
	defer os.Unsetenv("_OPINAI_VERIFY_FIX_PENDING")

	return jm.CreateReproductionJob(repo, issueNumber, issueTitle)
}

// trySandboxDeploy checks if the repo needs K8s sandbox deployment and creates one if so.
// Returns (sandboxNS, endpointsJSON, planJSON). On failure, returns empty strings (fallback to code analysis).
func (jm *JobManager) trySandboxDeploy(repo string, issueNumber int, issueTitle string) (string, string, string) {
	if jm.sandbox == nil {
		return "", "", ""
	}

	// Check repo profile for k8s=true
	profile := loadRepoProfileForJob(repo)
	isK8s := false
	if profile != nil {
		if b, ok := profile["k8s"].(bool); ok {
			isK8s = b
		}
	}

	// Get deployment plan from DB
	plan, err := database.GetDeploymentPlan(repo)
	if err != nil || plan == nil {
		if isK8s {
			slog.Info("K8s repo has no deployment plan — job will do code analysis", "repo", repo)
		}
		return "", "", ""
	}

	planJSON := plan.PlanJSON
	if !isK8s {
		// Non-K8s repo: pass the plan for AI reference but don't create sandbox
		return "", "", planJSON
	}

	// Parse plan options
	var planData struct {
		Options []struct {
			ID          string           `json:"id"`
			Name        string           `json:"name"`
			Recommended bool             `json:"recommended"`
			Steps       []map[string]any `json:"steps"`
		} `json:"options"`
	}
	if err := json.Unmarshal([]byte(planJSON), &planData); err != nil || len(planData.Options) == 0 {
		slog.Warn("failed to parse deployment plan", "repo", repo, "error", err)
		return "", "", planJSON
	}

	// Pick recommended option (or first)
	selected := 0
	for i, opt := range planData.Options {
		if opt.Recommended {
			selected = i
			break
		}
	}
	opt := planData.Options[selected]
	slog.Info("selected deployment option for sandbox", "repo", repo, "option", opt.Name)

	// Create sandbox namespace
	sandboxNS, err := jm.sandbox.CreateSandbox(repo, issueNumber)
	if err != nil {
		slog.Error("sandbox creation failed — falling back to code analysis", "repo", repo, "error", err)
		return "", "", planJSON
	}

	// Deploy the selected option
	result, err := jm.sandbox.DeployInSandbox(sandboxNS, opt.Steps)
	if err != nil {
		slog.Error("sandbox deployment failed — tearing down and falling back", "namespace", sandboxNS, "error", err)
		jm.sandbox.TeardownSandbox(sandboxNS)
		return "", "", planJSON
	}

	success, _ := result["success"].(bool)
	if !success {
		errs, _ := result["errors"].([]string)
		slog.Warn("sandbox deployment had failures — tearing down", "namespace", sandboxNS, "errors", errs)
		jm.sandbox.TeardownSandbox(sandboxNS)
		return "", "", planJSON
	}

	// Get endpoints
	endpoints, _ := result["endpoints"].(map[string]string)
	endpointsJSON, _ := json.Marshal(endpoints)

	slog.Info("sandbox deployed successfully", "namespace", sandboxNS, "endpoints", len(endpoints))
	return sandboxNS, string(endpointsJSON), planJSON
}

func loadRepoProfileForJob(repo string) map[string]any {
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

func truncateStr(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// DeleteJob deletes a Job by name.
func (jm *JobManager) DeleteJob(name string) error {
	bg := metav1.DeletePropagationBackground
	return jm.client.BatchV1().Jobs(jm.namespace).Delete(
		context.Background(), name, metav1.DeleteOptions{PropagationPolicy: &bg},
	)
}

// CleanupOrphanedJobs deletes jobs whose repo is no longer monitored.
func (jm *JobManager) CleanupOrphanedJobs(monitoredRepos []string) {
	ctx := context.Background()
	jobs, err := jm.client.BatchV1().Jobs(jm.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=opinai-runner",
	})
	if err != nil {
		return
	}
	monitored := make(map[string]bool, len(monitoredRepos))
	for _, r := range monitoredRepos {
		monitored[r] = true
	}
	for _, job := range jobs.Items {
		repo := ""
		if job.Annotations != nil {
			repo = job.Annotations["opinai/repo-full"]
		}
		if repo != "" && !monitored[repo] {
			jm.DeleteJob(job.Name)
			slog.Info("deleted orphaned job", "job", job.Name, "repo", repo)
		}
	}
}

// --- helpers ---

func (jm *JobManager) readPodLogs(ctx context.Context, jobName string) string {
	pods, err := jm.client.CoreV1().Pods(jm.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "job-name=" + jobName,
	})
	if err != nil || len(pods.Items) == 0 {
		return ""
	}
	tailLines := int64(200)
	logReq := jm.client.CoreV1().Pods(jm.namespace).GetLogs(pods.Items[0].Name, &corev1.PodLogOptions{
		TailLines: &tailLines,
	})
	stream, err := logReq.Stream(ctx)
	if err != nil {
		return ""
	}
	defer stream.Close()
	data, _ := io.ReadAll(stream)
	return string(data)
}

func parseMarker(logs, prefix, fallback string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, prefix) {
			upper := strings.ToUpper(line)
			// Extract value after the prefix marker
			switch {
			case strings.Contains(upper, "BUG") && prefix == "--- OPINAI CATEGORY:":
				return "BUG"
			case strings.Contains(upper, "FEATURE"):
				return "FEATURE"
			case strings.Contains(upper, "QUESTION"):
				return "QUESTION"
			case strings.Contains(upper, "DOCS"):
				return "DOCS"
			case strings.Contains(upper, "HIGH"):
				return "HIGH"
			case strings.Contains(upper, "MEDIUM"):
				return "MEDIUM"
			case strings.Contains(upper, "LOW"):
				return "LOW"
			}
			// Try to extract after colon
			parts := strings.SplitN(line, ":", 2)
			if len(parts) > 1 {
				val := strings.TrimSpace(strings.Trim(parts[1], " -"))
				if val != "" {
					return val
				}
			}
		}
	}
	return fallback
}

func parseVerdictMarker(logs, category string) string {
	verdicts := []string{"BUG_CONFIRMED", "NOT_A_BUG", "NOT_REPRODUCIBLE", "FEATURE_REQUEST", "ERROR", "BUG_FIXED", "BUG_REGRESSION"}
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "--- OPINAI VERDICT:") {
			upper := strings.ToUpper(line)
			for _, v := range verdicts {
				if strings.Contains(upper, v) {
					return v
				}
			}
		}
	}
	// Fallback
	if category == "FEATURE" || category == "QUESTION" || category == "DOCS" {
		return "FEATURE_REQUEST"
	}
	lower := strings.ToLower(logs)
	if strings.Contains(lower, "bug_regression") || strings.Contains(lower, "bug regression") {
		return "BUG_REGRESSION"
	}
	if strings.Contains(lower, "bug_fixed") || strings.Contains(lower, "bug fixed") {
		return "BUG_FIXED"
	}
	if strings.Contains(lower, "bug confirmed") || strings.Contains(lower, "bug_confirmed") {
		return "BUG_CONFIRMED"
	}
	if strings.Contains(lower, "not a bug") || strings.Contains(lower, "not_a_bug") || strings.Contains(lower, "all tests passed") {
		return "NOT_A_BUG"
	}
	if strings.Contains(lower, "not reproducible") || strings.Contains(lower, "not_reproducible") {
		return "NOT_REPRODUCIBLE"
	}
	return "ERROR"
}

func extractBlock(text, startMarker, endMarker string) string {
	start := strings.Index(text, startMarker)
	if start < 0 {
		return ""
	}
	start += len(startMarker)
	end := strings.Index(text[start:], endMarker)
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(text[start : start+end])
}

func storeRepoMemory(repo, logs string) {
	memStart := "--- OPINAI REPO MEMORY ---"
	memEnd := "--- END REPO MEMORY ---"
	pos := 0
	for {
		s := strings.Index(logs[pos:], memStart)
		if s < 0 {
			break
		}
		s += pos + len(memStart)
		e := strings.Index(logs[s:], memEnd)
		if e < 0 {
			break
		}
		raw := strings.TrimSpace(logs[s : s+e])
		pos = s + e + len(memEnd)

		var data map[string]string
		if err := json.Unmarshal([]byte(raw), &data); err != nil {
			slog.Warn("failed to parse repo memory JSON", "error", err)
			continue
		}
		for k, v := range data {
			if v != "" {
				database.SetRepoMemory(repo, k, v)
			}
		}
		slog.Info("stored repo memory", "repo", repo, "keys", len(data))
	}
}

func buildRepoContext(repo string) string {
	var parts []string
	if mem, _ := database.GetRepoMemory(repo, nil); len(mem) > 0 {
		parts = append(parts, "## What OpinAI knows about this project:")
		for k, v := range mem {
			parts = append(parts, fmt.Sprintf("- %s: %s", k, v))
		}
	}
	if runs, _ := database.GetRuns(repo, 10); len(runs) > 0 {
		parts = append(parts, "\n## Previous reproduction attempts:")
		for _, run := range runs {
			line := fmt.Sprintf("- Issue #%d: %s (%s)", run.Issue, run.Verdict, run.Category)
			parts = append(parts, line)
			if run.Report != "" {
				summary := run.Report
				if len(summary) > 200 {
					summary = summary[:200]
				}
				parts = append(parts, "  Summary: "+summary)
			}
		}
	}
	return strings.Join(parts, "\n")
}

func collectProfileEnvVars() []corev1.EnvVar {
	var envs []corev1.EnvVar
	for _, kv := range os.Environ() {
		parts := strings.SplitN(kv, "=", 2)
		if len(parts) == 2 && strings.HasPrefix(parts[0], "REPO_PROFILE_") {
			envs = append(envs, corev1.EnvVar{Name: parts[0], Value: parts[1]})
		}
	}
	return envs
}

func secretEnvVar(name, secretName, key string) []corev1.EnvVar {
	optional := true
	return []corev1.EnvVar{{
		Name: name,
		ValueFrom: &corev1.EnvVarSource{
			SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: secretName},
				Key:                  key,
				Optional:             &optional,
			},
		},
	}}
}

func boolPtr(b bool) *bool { return &b }
func strPtr(s string) *string { return &s }

func mustParseQuantity(s string) resource.Quantity {
	q, err := resource.ParseQuantity(s)
	if err != nil {
		panic(err)
	}
	return q
}
