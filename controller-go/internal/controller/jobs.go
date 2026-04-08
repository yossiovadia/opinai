package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	"github.com/yossiovadia/opinai/controller-go/internal/config"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
	"github.com/yossiovadia/opinai/controller-go/internal/hostprofile"
	"github.com/yossiovadia/opinai/controller-go/internal/sandbox"
)

// BroadcastEvent matches dashboard.WSEvent.
type BroadcastEvent struct {
	Type string `json:"type"`
	Data any    `json:"data,omitempty"`
}

// Broadcaster is the interface for pushing WebSocket events.
type Broadcaster interface {
	Broadcast(event BroadcastEvent)
}

// JobManager handles K8s Job creation and result harvesting.
type JobManager struct {
	client      kubernetes.Interface
	namespace   string
	image       string
	mu          sync.Mutex // protects recorded map
	recorded    map[string]bool
	sandbox     *sandbox.Manager
	ws          Broadcaster
	onComplete  func(repo string) // called when a job completes, to retry pending issues
	hostProfile *hostprofile.HostProfile
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

// SetHostProfile sets the detected host profile for hardware-aware image selection.
func (jm *JobManager) SetHostProfile(p *hostprofile.HostProfile) {
	jm.hostProfile = p
}

// SetSandboxDynamicClient sets the dynamic K8s client on the sandbox manager for CRD/BuildConfig support.
func (jm *JobManager) SetSandboxDynamicClient(dc dynamic.Interface) {
	if jm.sandbox != nil {
		jm.sandbox.SetDynamicClient(dc)
	}
}

// SetBroadcaster sets the WebSocket hub for push notifications.
func (jm *JobManager) SetBroadcaster(b Broadcaster) {
	jm.ws = b
}

// SetOnComplete sets a callback invoked when a job finishes, to retry pending issues.
func (jm *JobManager) SetOnComplete(fn func(repo string)) {
	jm.onComplete = fn
}

func (jm *JobManager) broadcast(eventType string, data any) {
	if jm.ws == nil {
		return
	}
	jm.ws.Broadcast(BroadcastEvent{Type: eventType, Data: data})
}

// JobInfo describes an active K8s reproduction job for the dashboard.
type JobInfo struct {
	Repo      string `json:"repo"`
	Issue     int    `json:"issue"`
	Status    string `json:"status"`
	CreatedAt string `json:"created_at"`
	PodName   string `json:"pod_name"`
}

// ListJobs returns all opinai-runner jobs with their current status.
func (jm *JobManager) ListJobs() []JobInfo {
	ctx := context.Background()
	jobs, err := jm.client.BatchV1().Jobs(jm.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=opinai-runner",
	})
	if err != nil {
		slog.Warn("failed to list jobs for dashboard", "error", err)
		return nil
	}

	var result []JobInfo
	for _, job := range jobs.Items {
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
		issue := 0
		fmt.Sscanf(labels["opinai/issue"], "%d", &issue)

		status := "Pending"
		if job.Status.Active > 0 {
			status = "Running"
		} else if job.Status.Succeeded > 0 {
			status = "Completed"
		} else if job.Status.Failed > 0 {
			status = "Failed"
		}

		createdAt := ""
		if job.CreationTimestamp.Time.Year() > 2000 {
			createdAt = job.CreationTimestamp.Format("2006-01-02T15:04:05Z")
		}

		// Find pod name
		podName := ""
		pods, err := jm.client.CoreV1().Pods(jm.namespace).List(ctx, metav1.ListOptions{
			LabelSelector: "job-name=" + job.Name,
		})
		if err == nil && len(pods.Items) > 0 {
			podName = pods.Items[0].Name
		}

		result = append(result, JobInfo{
			Repo:      repo,
			Issue:     issue,
			Status:    status,
			CreatedAt: createdAt,
			PodName:   podName,
		})
	}
	return result
}

// MaxConcurrentJobs is the total number of reproduction Jobs that can run simultaneously.
var MaxConcurrentJobs = 3

// countRunningJobs returns total active jobs and whether the given repo has one running.
func (jm *JobManager) countRunningJobs(repo string) (total int, repoRunning bool) {
	repoSafe := strings.ToLower(strings.ReplaceAll(repo, "/", "-"))
	ctx := context.Background()
	jobs, err := jm.client.BatchV1().Jobs(jm.namespace).List(ctx, metav1.ListOptions{
		LabelSelector: "app=opinai-runner",
	})
	if err != nil {
		return 0, false
	}
	for _, job := range jobs.Items {
		if job.Status.Active > 0 {
			total++
			if job.Labels != nil && job.Labels["opinai/repo"] == repoSafe {
				repoRunning = true
			}
		}
	}
	return
}

// JobName generates the K8s job name for a repo+issue.
func JobName(repo string, issue int) string {
	safe := strings.ToLower(strings.ReplaceAll(repo, "/", "-"))
	return fmt.Sprintf("opinai-%s-%d", safe, issue)
}

func (jm *JobManager) createJob(repo string, issueNumber int, issueTitle string, verifyFix bool) error {
	name := JobName(repo, issueNumber)
	ctx := context.Background()

	// Concurrency control: 1 job per repo, max 3 total
	totalActive, repoActive := jm.countRunningJobs(repo)
	if repoActive {
		slog.Info("repo already has a running job — skipping, will retry next cycle", "repo", repo, "issue", issueNumber)
		return nil
	}
	if totalActive >= MaxConcurrentJobs {
		slog.Info("max concurrent jobs reached — skipping, will retry next cycle", "repo", repo, "issue", issueNumber, "active", totalActive, "max", MaxConcurrentJobs)
		return nil
	}

	// Check if job already exists — only skip if it's still active
	existing, err := jm.client.BatchV1().Jobs(jm.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if existing.Status.Active > 0 {
			slog.Info("job already exists and is active, skipping", "job", name)
			return nil
		}
		// Job exists but is finished — delete it to allow re-creation
		slog.Info("deleting completed job to allow re-creation", "job", name)
		bg := metav1.DeletePropagationBackground
		jm.client.BatchV1().Jobs(jm.namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &bg})
		// Brief wait for deletion to propagate
		time.Sleep(2 * time.Second)
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

	// Fetch issue comments for agent context
	commentsJSON := ""
	if comments, err := FetchIssueComments(repo, issueNumber); err == nil && len(comments) > 0 {
		if b, err := json.Marshal(comments); err == nil {
			commentsJSON = string(b)
			if len(commentsJSON) > 4096 {
				commentsJSON = commentsJSON[:4096]
			}
		}
	}

	// Fetch linked PRs/issues referenced in the issue body
	linkedJSON := ""
	if details, err := FetchIssueDetails(repo, issueNumber); err == nil {
		linked := FetchLinkedResources(details.Body + "\n" + commentsJSON)
		if len(linked) > 0 {
			if b, err := json.Marshal(linked); err == nil {
				linkedJSON = string(b)
			}
		}
	}

	// Select runner image based on project requirements vs host capabilities
	imgSel := jm.selectRunnerImage(repo)

	// Check if repo needs K8s sandbox deployment
	sandboxNS, sandboxEndpointsJSON, deploymentPlanJSON, testEndpointJSON, allEndpointsJSON := jm.trySandboxDeploy(repo, issueNumber, issueTitle)

	// Extract install command, resources, and timeout from deployment plan
	planRes := extractPlanResources(deploymentPlanJSON)
	activeDeadline := int64(planRes.TimeoutMinutes * 60)

	// Apply image selection (overrides default image, adjusts resources)
	runnerImage := jm.image
	if imgSel.Image != "" {
		runnerImage = imgSel.Image
	}
	if imgSel.CPUReq != "" {
		planRes.CPUReq = imgSel.CPUReq
	}
	if imgSel.CPULim != "" {
		planRes.CPULim = imgSel.CPULim
	}
	if imgSel.MemReq != "" {
		planRes.MemReq = imgSel.MemReq
	}
	if imgSel.MemLim != "" {
		planRes.MemLim = imgSel.MemLim
	}

	// Build env vars list
	env := []corev1.EnvVar{
		{Name: "REPO", Value: repo},
		{Name: "ISSUE_NUMBER", Value: fmt.Sprintf("%d", issueNumber)},
		{Name: "OPINAI_AUTO_POST", Value: "false"},
		{Name: "OPINAI_VERIFY_FIX", Value: fmt.Sprintf("%t", verifyFix)},
		{Name: "OPINAI_REPO_CONTEXT", Value: repoContext},
		{Name: "OPINAI_HAS_KNOWLEDGE", Value: hasKnowledge},
		{Name: "OPINAI_INSTALL_COMMAND", Value: planRes.InstallCommand},
		{Name: "OPINAI_SANDBOX_NAMESPACE", Value: sandboxNS},
		{Name: "OPINAI_SANDBOX_ENDPOINTS", Value: sandboxEndpointsJSON},
		{Name: "OPINAI_TEST_ENDPOINT", Value: testEndpointJSON},
		{Name: "OPINAI_ALL_ENDPOINTS", Value: allEndpointsJSON},
		{Name: "OPINAI_DEPLOYMENT_PLAN", Value: truncateStr(deploymentPlanJSON, 30000)},
		{Name: "OPINAI_ISSUE_COMMENTS", Value: commentsJSON},
		{Name: "OPINAI_LINKED_RESOURCES", Value: linkedJSON},
		{Name: "GOOGLE_APPLICATION_CREDENTIALS", Value: "/var/run/secrets/gcp/credentials.json"},
		{Name: "OPINAI_CONTROLLER_URL", Value: controllerURL(jm.namespace)},
		{Name: "OPINAI_RUNNER_IMAGE", Value: runnerImage},
	}
	if !imgSel.Feasible {
		env = append(env, corev1.EnvVar{Name: "OPINAI_FEASIBILITY_REASON", Value: imgSel.Reason})
		slog.Warn("project infeasible for full reproduction", "repo", repo, "reason", imgSel.Reason)
	} else if imgSel.Image != jm.image {
		slog.Info("selected runner image", "repo", repo, "image", runnerImage, "reason", imgSel.Reason)
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
			ActiveDeadlineSeconds:   &activeDeadline,
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
							Image:           runnerImage,
							ImagePullPolicy: imagePullPolicy(runnerImage),
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
									corev1.ResourceCPU:    mustParseQuantity(planRes.CPUReq),
									corev1.ResourceMemory: mustParseQuantity(planRes.MemReq),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    mustParseQuantity(planRes.CPULim),
									corev1.ResourceMemory: mustParseQuantity(planRes.MemLim),
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
	jm.broadcast("job_created", map[string]any{"repo": repo, "issue": issueNumber})
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

	for i := range jobs.Items {
		job := &jobs.Items[i]
		if job.Status.Succeeded > 0 || job.Status.Failed > 0 {
			jm.harvestSingleJob(job)
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

	return jm.createJob(repo, issueNumber, issueTitle, true)
}

// CreateReproductionJob creates a K8s Job to reproduce an issue.
func (jm *JobManager) CreateReproductionJob(repo string, issueNumber int, issueTitle string) error {
	return jm.createJob(repo, issueNumber, issueTitle, false)
}

// PRReviewJobName generates the K8s job name for a PR review.
func PRReviewJobName(repo string, prNumber int) string {
	safe := strings.ToLower(strings.ReplaceAll(repo, "/", "-"))
	return fmt.Sprintf("opinai-pr-%s-%d", safe, prNumber)
}

// CreatePRReviewJob creates a K8s Job to review a pull request.
func (jm *JobManager) CreatePRReviewJob(repo string, prNumber int, title string) error {
	name := PRReviewJobName(repo, prNumber)
	ctx := context.Background()

	// Concurrency control: same limits as reproduction jobs
	totalActive, repoActive := jm.countRunningJobs(repo)
	if repoActive {
		slog.Info("repo already has a running job — skipping PR review", "repo", repo, "pr", prNumber)
		return nil
	}
	if totalActive >= MaxConcurrentJobs {
		slog.Info("max concurrent jobs reached — skipping PR review", "repo", repo, "pr", prNumber, "active", totalActive)
		return nil
	}

	// Delete existing finished job if present
	existing, err := jm.client.BatchV1().Jobs(jm.namespace).Get(ctx, name, metav1.GetOptions{})
	if err == nil {
		if existing.Status.Active > 0 {
			slog.Info("PR review job already active, skipping", "job", name)
			return nil
		}
		bg := metav1.DeletePropagationBackground
		jm.client.BatchV1().Jobs(jm.namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &bg})
		time.Sleep(2 * time.Second)
	}

	repoSafe := strings.ToLower(strings.ReplaceAll(repo, "/", "-"))
	backoff := int32(0)
	ttl := int32(3600)
	activeDeadline := int64(600) // 10 minutes

	// Fetch PR diff and details for the runner
	prDiff := ""
	prBody := ""
	prAuthor := ""
	prHeadRef := ""
	if pr, err := FetchPRDetails(repo, prNumber); err == nil {
		prBody = pr.Body
		prAuthor = pr.User.Login
		prHeadRef = pr.Head.Ref
	}
	if diff, err := FetchPRDiff(repo, prNumber); err == nil {
		prDiff = diff
		if len(prDiff) > 60000 {
			prDiff = prDiff[:60000] + "\n... (diff truncated)"
		}
	}

	// Build changed files list
	changedFilesJSON := ""
	if files, err := FetchPRChangedFiles(repo, prNumber); err == nil {
		// Filter to source files, skip vendor/generated
		var filtered []PRChangedFile
		for _, f := range files {
			if isSourceFile(f.Filename) {
				filtered = append(filtered, f)
			}
		}
		if b, err := json.Marshal(filtered); err == nil {
			changedFilesJSON = string(b)
			if len(changedFilesJSON) > 30000 {
				changedFilesJSON = changedFilesJSON[:30000]
			}
		}
	}

	// Fetch linked issue investigations from PR body
	linkedJSON := ""
	linked := FetchLinkedResources(prBody)
	if len(linked) > 0 {
		if b, err := json.Marshal(linked); err == nil {
			linkedJSON = string(b)
		}
	}

	// Build repo context
	repoContext := buildRepoContext(repo)

	// Select runner image
	imgSel := jm.selectRunnerImage(repo)
	runnerImage := jm.image
	if imgSel.Image != "" {
		runnerImage = imgSel.Image
	}

	titleTrunc := title
	if len(titleTrunc) > 253 {
		titleTrunc = titleTrunc[:253]
	}

	env := []corev1.EnvVar{
		{Name: "REPO", Value: repo},
		{Name: "OPINAI_MODE", Value: "pr-review"},
		{Name: "OPINAI_PR_NUMBER", Value: fmt.Sprintf("%d", prNumber)},
		{Name: "OPINAI_PR_TITLE", Value: truncateStr(title, 1000)},
		{Name: "OPINAI_PR_BODY", Value: truncateStr(prBody, 4096)},
		{Name: "OPINAI_PR_DIFF", Value: prDiff},
		{Name: "OPINAI_PR_AUTHOR", Value: prAuthor},
		{Name: "OPINAI_PR_HEAD_REF", Value: prHeadRef},
		{Name: "OPINAI_PR_CHANGED_FILES", Value: changedFilesJSON},
		{Name: "OPINAI_REPO_CONTEXT", Value: repoContext},
		{Name: "OPINAI_LINKED_RESOURCES", Value: linkedJSON},
		{Name: "OPINAI_CONTROLLER_URL", Value: controllerURL(jm.namespace)},
		{Name: "OPINAI_RUNNER_IMAGE", Value: runnerImage},
		{Name: "GOOGLE_APPLICATION_CREDENTIALS", Value: "/var/run/secrets/gcp/credentials.json"},
	}
	env = append(env, secretEnvVar("AI_PROVIDER", "opinai-credentials", "AI_PROVIDER")...)
	env = append(env, secretEnvVar("AI_PROJECT", "opinai-credentials", "AI_PROJECT")...)
	env = append(env, secretEnvVar("AI_REGION", "opinai-credentials", "AI_REGION")...)
	env = append(env, secretEnvVar("AI_MODEL", "opinai-credentials", "AI_MODEL")...)
	env = append(env, collectProfileEnvVars()...)

	planRes := extractPlanResources("")
	if imgSel.CPUReq != "" {
		planRes.CPUReq = imgSel.CPUReq
	}
	if imgSel.CPULim != "" {
		planRes.CPULim = imgSel.CPULim
	}
	if imgSel.MemReq != "" {
		planRes.MemReq = imgSel.MemReq
	}
	if imgSel.MemLim != "" {
		planRes.MemLim = imgSel.MemLim
	}

	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: jm.namespace,
			Labels: map[string]string{
				"app":          "opinai-runner",
				"opinai/repo":  repoSafe,
				"opinai/pr":    fmt.Sprintf("%d", prNumber),
				"opinai/type":  "pr-review",
			},
			Annotations: map[string]string{
				"opinai/title":     titleTrunc,
				"opinai/repo-full": repo,
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            &backoff,
			ActiveDeadlineSeconds:   &activeDeadline,
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
							Image:           runnerImage,
							ImagePullPolicy: imagePullPolicy(runnerImage),
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
									corev1.ResourceCPU:    mustParseQuantity(planRes.CPUReq),
									corev1.ResourceMemory: mustParseQuantity(planRes.MemReq),
								},
								Limits: corev1.ResourceList{
									corev1.ResourceCPU:    mustParseQuantity(planRes.CPULim),
									corev1.ResourceMemory: mustParseQuantity(planRes.MemLim),
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
		return fmt.Errorf("create PR review job %s: %w", name, err)
	}

	jm.broadcast("pr_review_created", map[string]any{"repo": repo, "pr": prNumber})
	slog.Info("created PR review job", "job", name, "repo", repo, "pr", prNumber, "title", title)
	return nil
}

// isSourceFile returns true if the filename looks like source code (not vendor/generated).
func isSourceFile(name string) bool {
	lower := strings.ToLower(name)
	// Skip vendor, generated, and binary files
	for _, skip := range []string{"vendor/", "node_modules/", ".min.", ".bundle.", "package-lock.json", "yarn.lock", "go.sum"} {
		if strings.Contains(lower, skip) {
			return false
		}
	}
	// Accept common source extensions
	for _, ext := range []string{".go", ".py", ".js", ".ts", ".jsx", ".tsx", ".rs", ".java", ".rb", ".c", ".cpp", ".h", ".yaml", ".yml", ".toml", ".json", ".sql", ".sh", ".md"} {
		if strings.HasSuffix(lower, ext) {
			return true
		}
	}
	return false
}

// StartWatcher watches K8s Jobs for real-time result harvesting.
func (jm *JobManager) StartWatcher() {
	slog.Info("job watcher started", "namespace", jm.namespace)
	for {
		jm.runWatch()
		slog.Warn("job watch disconnected — reconnecting in 5s")
		time.Sleep(5 * time.Second)
	}
}

func (jm *JobManager) runWatch() {
	ctx := context.Background()
	watcher, err := jm.client.BatchV1().Jobs(jm.namespace).Watch(ctx, metav1.ListOptions{
		LabelSelector: "app=opinai-runner",
	})
	if err != nil {
		slog.Error("failed to start job watch", "error", err)
		return
	}
	defer watcher.Stop()

	for event := range watcher.ResultChan() {
		job, ok := event.Object.(*batchv1.Job)
		if !ok {
			continue
		}
		finished := job.Status.Succeeded > 0 || job.Status.Failed > 0
		if !finished {
			continue
		}
		jm.mu.Lock()
		already := jm.recorded[job.Name]
		jm.mu.Unlock()
		if already {
			slog.Debug("skipping already-recorded job", "job", job.Name)
			continue
		}
		slog.Info("watcher: job finished", "job", job.Name)
		jm.harvestSingleJob(job)
	}
}

// ClearRecorded removes a job from the recorded map so it can be re-harvested.
func (jm *JobManager) ClearRecorded(repo string, issue int) {
	name := JobName(repo, issue)
	jm.mu.Lock()
	delete(jm.recorded, name)
	jm.mu.Unlock()
	slog.Info("cleared recorded entry for rerun", "job", name)
}

// MarkRecorded marks a job as already recorded so the harvester skips it.
// Call this when the runner has already posted results via the callback API.
func (jm *JobManager) MarkRecorded(repo string, issue int) {
	name := JobName(repo, issue)
	jm.mu.Lock()
	jm.recorded[name] = true
	jm.mu.Unlock()
	slog.Debug("marked job as recorded via callback", "job", name)
}

// harvestSingleJob extracts results from a single completed job.
func (jm *JobManager) harvestSingleJob(job *batchv1.Job) {
	name := job.Name
	jm.mu.Lock()
	already := jm.recorded[name]
	jm.mu.Unlock()
	if already {
		slog.Debug("skipping already-recorded job", "job", name)
		return
	}

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
	title := annotations["opinai/title"]
	isPRReview := labels["opinai/type"] == "pr-review"

	if job.Status.Succeeded > 0 {
		slog.Info("job succeeded", "job", name)
	} else {
		slog.Warn("job failed", "job", name)
	}

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

	podLogs := jm.readPodLogs(context.Background(), name)

	ts := time.Now().UTC().Format("2006-01-02T15:04:05Z")
	if job.Status.CompletionTime != nil {
		ts = job.Status.CompletionTime.Format("2006-01-02T15:04:05Z")
	}

	if isPRReview {
		jm.harvestPRReviewJob(job, repo, title, duration, podLogs, ts)
		return
	}

	category := parseMarker(podLogs, "--- OPINAI CATEGORY:", "BUG")
	confidence := parseMarker(podLogs, "--- OPINAI CONFIDENCE:", "")
	verdict := parseVerdictMarker(podLogs, category)
	suggestedComment := extractBlock(podLogs, "--- OPINAI SUGGESTED COMMENT ---", "--- END SUGGESTED COMMENT ---")
	suggestedQuestions := extractBlock(podLogs, "--- OPINAI SUGGESTED_QUESTIONS ---", "--- END SUGGESTED_QUESTIONS ---")
	reproDetails := extractBlock(podLogs, "--- OPINAI REPRODUCTION_DETAILS ---", "--- END REPRODUCTION_DETAILS ---")
	storeRepoMemory(repo, podLogs)

	issueStr := labels["opinai/issue"]
	issue := 0
	fmt.Sscanf(issueStr, "%d", &issue)

	report := suggestedComment
	if report == "" && len(podLogs) > 3000 {
		report = podLogs[len(podLogs)-3000:]
	} else if report == "" {
		report = podLogs
	}
	if report == "" {
		report = "(no logs)"
	}

	_, dbErr := database.AddRun(database.Run{
		Repo: repo, Issue: issue, Title: title,
		Verdict: verdict, Category: category, Confidence: confidence,
		AIPowered: true, Duration: duration, Posted: false,
		Report: report, SuggestedQuestions: suggestedQuestions, ReproDetails: reproDetails, CreatedAt: ts,
	})
	if dbErr != nil {
		slog.Error("failed to store run in DB — will retry on next harvest", "job", name, "error", dbErr)
		return
	}
	jm.mu.Lock()
	jm.recorded[name] = true
	jm.mu.Unlock()
	database.RemovePending(repo, issue)
	jm.broadcast("job_completed", map[string]any{"repo": repo, "issue": issue, "verdict": verdict})

	sbNS := annotations["opinai/sandbox-namespace"]
	if sbNS != "" && jm.sandbox != nil {
		if jm.sandbox.TeardownSandbox(sbNS) {
			slog.Info("torn down sandbox after job", "namespace", sbNS, "job", name)
		}
	}

	// Delete completed K8s Job to keep the dashboard clean
	if err := jm.DeleteJob(name); err != nil {
		slog.Warn("failed to delete completed job", "job", name, "error", err)
	}

	// NOTE: retry is triggered by the runner callback (/api/internal/result), not here.
	// The callback arrives first and is more reliable. Having both causes duplicate retries.
}

// harvestPRReviewJob extracts PR review results from a completed job's logs.
func (jm *JobManager) harvestPRReviewJob(job *batchv1.Job, repo, title, duration, podLogs, ts string) {
	name := job.Name
	labels := job.Labels
	if labels == nil {
		labels = map[string]string{}
	}

	prNumber := 0
	fmt.Sscanf(labels["opinai/pr"], "%d", &prNumber)

	verdict := parsePRVerdict(podLogs)
	risk := parsePRRisk(podLogs)
	reviewText := extractBlock(podLogs, "--- OPINAI PR REVIEW ---", "--- END PR REVIEW ---")
	if reviewText == "" && len(podLogs) > 3000 {
		reviewText = podLogs[len(podLogs)-3000:]
	} else if reviewText == "" {
		reviewText = podLogs
	}
	if reviewText == "" {
		reviewText = "(no review output)"
	}

	author := ""
	// Try to extract author from logs
	for _, line := range strings.Split(podLogs, "\n") {
		if strings.Contains(line, "--- OPINAI PR AUTHOR:") {
			parts := strings.SplitN(line, ":", 2)
			if len(parts) > 1 {
				author = strings.TrimSpace(strings.Trim(parts[1], " -"))
			}
		}
	}

	_, dbErr := database.AddPRReview(database.PRReview{
		Repo: repo, PRNumber: prNumber, Title: title,
		Author: author, Verdict: verdict, Risk: risk,
		ReviewText: reviewText, Posted: false, Duration: duration, CreatedAt: ts,
	})
	if dbErr != nil {
		slog.Error("failed to store PR review in DB", "job", name, "error", dbErr)
		return
	}
	jm.mu.Lock()
	jm.recorded[name] = true
	jm.mu.Unlock()
	jm.broadcast("pr_review_completed", map[string]any{"repo": repo, "pr": prNumber, "verdict": verdict, "risk": risk})

	if err := jm.DeleteJob(name); err != nil {
		slog.Warn("failed to delete completed PR review job", "job", name, "error", err)
	}

	slog.Info("harvested PR review", "job", name, "repo", repo, "pr", prNumber, "verdict", verdict, "risk", risk)
}

func parsePRVerdict(logs string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "--- OPINAI PR VERDICT:") {
			upper := strings.ToUpper(line)
			for _, v := range []string{"APPROVE", "CHANGES_REQUESTED", "COMMENT"} {
				if strings.Contains(upper, v) {
					return v
				}
			}
		}
	}
	// Fallback keyword scan
	upper := strings.ToUpper(logs)
	if strings.Contains(upper, "CHANGES_REQUESTED") {
		return "CHANGES_REQUESTED"
	}
	if strings.Contains(upper, "===PR_VERDICT===") {
		block := logs[strings.Index(upper, "===PR_VERDICT==="):]
		if strings.Contains(strings.ToUpper(block), "APPROVE") {
			return "APPROVE"
		}
		if strings.Contains(strings.ToUpper(block), "CHANGES_REQUESTED") {
			return "CHANGES_REQUESTED"
		}
	}
	return "COMMENT"
}

func parsePRRisk(logs string) string {
	for _, line := range strings.Split(logs, "\n") {
		if strings.Contains(line, "--- OPINAI PR RISK:") || strings.Contains(line, "risk:") {
			upper := strings.ToUpper(line)
			for _, r := range []string{"CRITICAL", "HIGH", "MEDIUM", "LOW"} {
				if strings.Contains(upper, r) {
					return r
				}
			}
		}
	}
	// Fallback from verdict block
	if idx := strings.Index(strings.ToUpper(logs), "===PR_VERDICT==="); idx >= 0 {
		block := strings.ToUpper(logs[idx:])
		for _, r := range []string{"CRITICAL", "HIGH", "MEDIUM", "LOW"} {
			if strings.Contains(block, "RISK: "+r) || strings.Contains(block, "RISK:"+r) {
				return r
			}
		}
	}
	return "LOW"
}

// trySandboxDeploy checks if the repo needs K8s sandbox deployment and creates one if so.
// Returns (sandboxNS, endpointsJSON, planJSON, testEndpointJSON, allEndpointsJSON).
func (jm *JobManager) trySandboxDeploy(repo string, issueNumber int, issueTitle string) (string, string, string, string, string) {
	if jm.sandbox == nil {
		return "", "", "", "", ""
	}

	profile := config.LoadRepoProfile(repo)
	isK8s := false
	if profile != nil {
		if b, ok := profile["k8s"].(bool); ok {
			isK8s = b
		}
	}

	// Also check repo_memory for needs_cluster (set by agent rich_analysis)
	if !isK8s {
		if mem, _ := database.GetRepoMemory(repo, strPtr("needs_cluster")); mem["needs_cluster"] == "true" {
			isK8s = true
			slog.Info("repo needs K8s (detected from repo_memory)", "repo", repo)
		}
	}

	plan, err := database.GetDeploymentPlan(repo)
	if err != nil || plan == nil {
		if isK8s {
			slog.Info("K8s repo has no deployment plan — job will do code analysis", "repo", repo)
		}
		return "", "", "", "", ""
	}

	planJSON := plan.PlanJSON
	if !isK8s {
		return "", "", planJSON, "", ""
	}

	var planData struct {
		Options []deployOption `json:"options"`
	}
	if err := json.Unmarshal([]byte(planJSON), &planData); err != nil || len(planData.Options) == 0 {
		slog.Warn("failed to parse deployment plan", "repo", repo, "error", err)
		return "", "", planJSON, "", ""
	}

	ordered := orderDeployOptions(repo, planData.Options)

	// Multi-attempt: try each option until one succeeds
	for attempt, opt := range ordered {
		slog.Info("trying deployment option", "repo", repo, "option", opt.Name, "attempt", attempt+1, "total", len(ordered))

		sandboxNS, err := jm.sandbox.CreateSandbox(repo, issueNumber, extractSandboxQuotas(planJSON))
		if err != nil {
			slog.Error("sandbox creation failed", "repo", repo, "error", err)
			return "", "", planJSON, "", ""
		}

		result, err := jm.sandbox.DeployInSandbox(sandboxNS, opt.Steps)
		if err == nil {
			success, _ := result["success"].(bool)
			if success {
				endpoints, _ := result["endpoints"].(map[string]string)
				endpointsJSON, _ := json.Marshal(endpoints)
				database.SetRepoMemory(repo, "working_deploy_option", opt.ID)
				slog.Info("sandbox deployed successfully", "namespace", sandboxNS, "option", opt.Name)
				return sandboxNS, string(endpointsJSON), planJSON, string(opt.TestEndpoint), string(opt.AllEndpoints)
			}
		}

		// Save failure for self-healing
		errMsg := ""
		if err != nil {
			errMsg = err.Error()
		} else if errs, ok := result["errors"].([]string); ok && len(errs) > 0 {
			errMsg = strings.Join(errs, "; ")
		}
		database.SetRepoMemory(repo, fmt.Sprintf("deploy_option_%s_error", opt.ID), errMsg)
		slog.Warn("deployment option failed", "option", opt.Name, "error", errMsg)
		jm.sandbox.TeardownSandbox(sandboxNS)
		// Clean up /tmp deploy clones between attempts
		sandbox.CleanDeployClones(repo)
		// Wait for namespace deletion before trying next option
		time.Sleep(5 * time.Second)
	}

	slog.Warn("all deployment options failed — falling back to code analysis", "repo", repo)
	return "", "", planJSON, "", ""
}

type deployOption struct {
	ID            string           `json:"id"`
	Name          string           `json:"name"`
	Recommended   bool             `json:"recommended"`
	Steps         []map[string]any `json:"steps"`
	TestEndpoint  json.RawMessage  `json:"test_endpoint"`
	AllEndpoints  json.RawMessage  `json:"all_endpoints"`
}

func orderDeployOptions(repo string, options []deployOption) []deployOption {
	mem, _ := database.GetRepoMemory(repo, strPtr("working_deploy_option"))
	workingID := ""
	if v, ok := mem["working_deploy_option"]; ok {
		workingID = v
	}

	var first, rest []deployOption
	for _, opt := range options {
		if opt.ID == workingID {
			first = append([]deployOption{opt}, first...)
		} else if opt.Recommended {
			first = append(first, opt)
		} else {
			rest = append(rest, opt)
		}
	}
	return append(first, rest...)
}

// extractPlanResources reads install_command and resource_requirements from a deployment plan.
// Returns (installCmd, cpuReq, memReq, cpuLim, memLim) with sensible defaults.
// PlanResources holds extracted deployment plan configuration for Job pods.
type PlanResources struct {
	InstallCommand string
	CPUReq, MemReq string
	CPULim, MemLim string
	TimeoutMinutes int
}

func extractPlanResources(planJSON string) PlanResources {
	r := PlanResources{
		CPUReq: "200m", MemReq: "512Mi",
		CPULim: "500m", MemLim: "1Gi",
		TimeoutMinutes: 10,
	}
	if planJSON == "" {
		return r
	}
	var plan struct {
		InstallCommand       string            `json:"install_command"`
		ResourceRequirements map[string]string  `json:"resource_requirements"`
		JobTimeoutMinutes    int               `json:"job_timeout_minutes"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return r
	}
	if plan.InstallCommand != "" {
		r.InstallCommand = plan.InstallCommand
	}
	if plan.JobTimeoutMinutes > 0 {
		r.TimeoutMinutes = plan.JobTimeoutMinutes
	}
	if plan.ResourceRequirements != nil {
		if v := plan.ResourceRequirements["cpu"]; v != "" {
			r.CPUReq = v
		}
		if v := plan.ResourceRequirements["memory"]; v != "" {
			r.MemReq = v
			r.MemLim = v
		}
	}
	slog.Info("job resources", "cpu", r.CPUReq+"/"+r.CPULim, "mem", r.MemReq+"/"+r.MemLim, "timeout", r.TimeoutMinutes)
	return r
}


// extractSandboxQuotas reads sandbox_resource_requirements from the deployment plan.
func extractSandboxQuotas(planJSON string) sandbox.SandboxQuotas {
	if planJSON == "" {
		return sandbox.SandboxQuotas{}
	}
	var plan struct {
		SandboxResources map[string]string `json:"sandbox_resource_requirements"`
		Resources        map[string]string `json:"resource_requirements"`
		TimeoutMinutes   int               `json:"job_timeout_minutes"`
	}
	if err := json.Unmarshal([]byte(planJSON), &plan); err != nil {
		return sandbox.SandboxQuotas{}
	}
	// Prefer sandbox-specific resources, fall back to general resource_requirements
	res := plan.SandboxResources
	if res == nil {
		res = plan.Resources
	}
	q := sandbox.SandboxQuotas{
		TimeoutMinutes: plan.TimeoutMinutes,
	}
	// Note: resource_requirements from the plan are PER-CONTAINER estimates,
	// not total quota. We ignore them for quota and use generous defaults.
	// The LimitRange handles per-container defaults.
	_ = res
	return q
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
// If the monitored list is empty, no cleanup is performed — an empty list
// means repos haven't loaded yet, not that all repos were removed.
func (jm *JobManager) CleanupOrphanedJobs(monitoredRepos []string) {
	if len(monitoredRepos) == 0 {
		slog.Debug("skipping orphan cleanup — monitored repos list is empty")
		return
	}
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

// selectRunnerImage reads runtime_requirements from repo_memory and selects the best
// runner image based on host capabilities. Returns a default selection if no requirements found.
func (jm *JobManager) selectRunnerImage(repo string) hostprofile.ImageSelection {
	mem, _ := database.GetRepoMemory(repo, strPtr("runtime_requirements"))
	reqJSON := mem["runtime_requirements"]
	req := hostprofile.ParseRequirements(reqJSON)
	return hostprofile.SelectImage(req, jm.hostProfile, jm.image)
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
	verdicts := []string{"BUG_CONFIRMED", "NOT_A_BUG", "NOT_REPRODUCIBLE", "FEATURE_REQUEST", "ERROR", "INCONCLUSIVE", "BUG_FIXED", "BUG_REGRESSION"}
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
	if strings.Contains(lower, "inconclusive") {
		return "INCONCLUSIVE"
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
			// Truncate very large values to keep the env var within K8s limits.
			// rich_analysis needs ~8KB to stay valid JSON, other values are small.
			if len(v) > 16384 {
				v = v[:16384]
			}
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

// HasSandbox returns true if the sandbox manager is available.
func (jm *JobManager) HasSandbox() bool { return jm.sandbox != nil }

// CleanupSandboxes deletes sandboxes older than maxAge seconds.
func (jm *JobManager) CleanupSandboxes(maxAge int) int {
	if jm.sandbox == nil {
		return 0
	}
	return jm.sandbox.AutoCleanup(maxAge)
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

// imagePullPolicy returns Never for images without a registry prefix (local/Kind),
// Always for registry-hosted images.
func imagePullPolicy(image string) corev1.PullPolicy {
	if strings.Contains(image, "/") {
		return corev1.PullAlways
	}
	return corev1.PullNever
}

// controllerURL returns the URL for runner pods to reach the controller.
// If OPINAI_CONTROLLER_URL is set, use that (for local/Kind deployments).
// Otherwise, use the in-cluster service URL.
func controllerURL(namespace string) string {
	if url := os.Getenv("OPINAI_CONTROLLER_URL"); url != "" {
		return url
	}
	return fmt.Sprintf("http://opinai-controller.%s.svc:8080", namespace)
}
