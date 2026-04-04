// Package sandbox manages temporary K8s namespaces for deployment testing.
package sandbox

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	sigYAML "sigs.k8s.io/yaml"

	authv1 "k8s.io/api/authorization/v1"
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const (
	SandboxPrefix   = "opinai-sandbox-"
	ManagedLabelKey = "opinai.dev/managed"
	MaxAgeSeconds   = 1800 // 30 minutes
)

// AllowedKinds lists K8s resource kinds that can be created in sandboxes.
var AllowedKinds = map[string]bool{
	"Deployment": true, "StatefulSet": true, "Service": true,
	"ConfigMap": true, "Secret": true, "ServiceAccount": true,
	"PersistentVolumeClaim": true, "Job": true, "CronJob": true,
	"Role": true, "RoleBinding": true, "NetworkPolicy": true,
	"Ingress": true, "HorizontalPodAutoscaler": true, "Route": true,
}

// SkippedKinds are silently skipped (cluster-scoped or already handled).
var SkippedKinds = map[string]bool{
	"Namespace": true, "ClusterRole": true, "ClusterRoleBinding": true,
}

// Manager handles sandbox namespace lifecycle.
type Manager struct {
	client    kubernetes.Interface
	dynClient dynamic.Interface
	disco     discovery.DiscoveryInterface
	namespace string // controller namespace (for network policy)
}

// NewManager creates a sandbox manager.
func NewManager(client kubernetes.Interface, controllerNS string) *Manager {
	m := &Manager{client: client, namespace: controllerNS}
	// Initialize dynamic client from the typed client's REST config.
	// If this fails, CRD deployment is unavailable but built-in kinds still work.
	if restClient, ok := client.(*kubernetes.Clientset); ok {
		m.disco = restClient.Discovery()
	}
	return m
}

// SetDynamicClient sets the dynamic K8s client for CRD resource deployment.
func (m *Manager) SetDynamicClient(dc dynamic.Interface) {
	m.dynClient = dc
}

// ValidateName returns true if the namespace starts with the sandbox prefix.
func ValidateName(ns string) bool {
	return strings.HasPrefix(ns, SandboxPrefix)
}

// SandboxQuotas holds configurable resource quotas for a sandbox namespace.
// Zero values mean "use default".
type SandboxQuotas struct {
	CPUReq         string // e.g. "1"
	MemReq         string // e.g. "1Gi"
	CPULim         string // e.g. "2"
	MemLim         string // e.g. "2Gi"
	Pods           int    // max pods, default 10
	TimeoutMinutes int    // sandbox max age, default 30
}

func (q SandboxQuotas) cpuReq() string { if q.CPUReq != "" { return q.CPUReq }; return "1" }
func (q SandboxQuotas) memReq() string { if q.MemReq != "" { return q.MemReq }; return "1Gi" }
func (q SandboxQuotas) cpuLim() string { if q.CPULim != "" { return q.CPULim }; return "2" }
func (q SandboxQuotas) memLim() string { if q.MemLim != "" { return q.MemLim }; return "2Gi" }
func (q SandboxQuotas) pods() string          { if q.Pods > 0 { return fmt.Sprintf("%d", q.Pods) }; return "10" }
func (q SandboxQuotas) timeoutMinutes() int   { if q.TimeoutMinutes > 0 { return q.TimeoutMinutes }; return 30 }

// CreateSandbox creates an isolated namespace with quotas and network policy.
// Pass nil for quotas to use defaults.
func (m *Manager) CreateSandbox(repo string, issue int, quotas ...SandboxQuotas) (string, error) {
	var q SandboxQuotas
	if len(quotas) > 0 {
		q = quotas[0]
	}
	repoShort := repo
	if idx := strings.LastIndex(repo, "/"); idx >= 0 {
		repoShort = repo[idx+1:]
	}
	if len(repoShort) > 20 {
		repoShort = repoShort[:20]
	}
	repoShort = strings.ToLower(strings.ReplaceAll(repoShort, ".", "-"))
	// Use random suffix for uniqueness even within the same second
	randBytes := make([]byte, 3)
	rand.Read(randBytes)
	suffix := hex.EncodeToString(randBytes)

	ns := fmt.Sprintf("%s%s-%d-%s", SandboxPrefix, repoShort, issue, suffix)
	if len(ns) > 63 {
		ns = ns[:63]
	}
	ns = strings.TrimRight(ns, "-")

	ctx := context.Background()

	// Create namespace
	_, err := m.client.CoreV1().Namespaces().Create(ctx, &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: ns,
			Labels: map[string]string{
				ManagedLabelKey:  "true",
				"opinai.dev/repo":  strings.ReplaceAll(repo, "/", "-"),
				"opinai.dev/issue": fmt.Sprintf("%d", issue),
			},
			Annotations: map[string]string{
				"opinai.dev/created-at":      time.Now().UTC().Format(time.RFC3339),
				"opinai.dev/repo-full":       repo,
				"opinai.dev/timeout-minutes": fmt.Sprintf("%d", q.timeoutMinutes()),
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create namespace %s: %w", ns, err)
	}
	slog.Info("created sandbox namespace", "namespace", ns)

	// ResourceQuota
	_, err = m.client.CoreV1().ResourceQuotas(ns).Create(ctx, &corev1.ResourceQuota{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "opinai-quota",
			Labels: map[string]string{ManagedLabelKey: "true"},
		},
		Spec: corev1.ResourceQuotaSpec{
			Hard: corev1.ResourceList{
				corev1.ResourceRequestsCPU:              resource.MustParse(q.cpuReq()),
				corev1.ResourceRequestsMemory:           resource.MustParse(q.memReq()),
				corev1.ResourceLimitsCPU:                resource.MustParse(q.cpuLim()),
				corev1.ResourceLimitsMemory:             resource.MustParse(q.memLim()),
				corev1.ResourcePods:                     resource.MustParse(q.pods()),
				corev1.ResourceServices:                 resource.MustParse("5"),
				corev1.ResourcePersistentVolumeClaims:   resource.MustParse("3"),
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		slog.Warn("failed to create resource quota", "namespace", ns, "error", err)
	}

	// NetworkPolicy — ingress is restricted, egress allows DNS + HTTP/HTTPS + intra-namespace
	protocolUDP := corev1.ProtocolUDP
	protocolTCP := corev1.ProtocolTCP
	port53 := intstr.FromInt(53)
	port80 := intstr.FromInt(80)
	port443 := intstr.FromInt(443)
	_, err = m.client.NetworkingV1().NetworkPolicies(ns).Create(ctx, &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:   "opinai-sandbox-policy",
			Labels: map[string]string{ManagedLabelKey: "true"},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{{
				From: []networkingv1.NetworkPolicyPeer{
					{NamespaceSelector: &metav1.LabelSelector{
						MatchLabels: map[string]string{"kubernetes.io/metadata.name": m.namespace},
					}},
					{PodSelector: &metav1.LabelSelector{}},
				},
			}},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				// Intra-namespace traffic
				{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}},
				// DNS
				{Ports: []networkingv1.NetworkPolicyPort{
					{Port: &port53, Protocol: &protocolUDP},
					{Port: &port53, Protocol: &protocolTCP},
				}},
				// HTTP/HTTPS egress (for pulling images, helm charts, external registries)
				{Ports: []networkingv1.NetworkPolicyPort{
					{Port: &port80, Protocol: &protocolTCP},
					{Port: &port443, Protocol: &protocolTCP},
				}},
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		slog.Warn("failed to create network policy", "namespace", ns, "error", err)
	}

	slog.Info("sandbox ready", "namespace", ns)
	return ns, nil
}

// DeployInSandbox executes deployment steps in a sandbox.
func (m *Manager) DeployInSandbox(ns string, steps []map[string]any) (map[string]any, error) {
	if !ValidateName(ns) {
		return nil, fmt.Errorf("invalid sandbox name: %s", ns)
	}

	// Verify managed label
	ctx := context.Background()
	nsObj, err := m.client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	if nsObj.Labels[ManagedLabelKey] != "true" {
		return nil, fmt.Errorf("namespace %s is not managed by OpinAI", ns)
	}

	// Clone target repo for command steps (once, reused for all steps)
	repo := ""
	if nsObj.Annotations != nil {
		repo = nsObj.Annotations["opinai.dev/repo-full"]
	}
	cloneDir, cloneOK := cloneRepoForDeploy(repo)
	if cloneDir != "" {
		defer os.RemoveAll(cloneDir)
	}

	result := map[string]any{
		"success":         true,
		"steps_completed": 0,
		"steps_total":     len(steps),
		"errors":          []string{},
		"endpoints":       map[string]string{},
	}

	lastStepWasManifest := false
	for i, step := range steps {
		stepType, _ := step["type"].(string)
		content, _ := step["content"].(string)
		required := true
		if r, ok := step["required"].(bool); ok {
			required = r
		}
		desc, _ := step["description"].(string)
		if desc == "" {
			desc = fmt.Sprintf("Step %d", i+1)
		}

		var stepErr error
		switch stepType {
		case "build":
			// Build a container image via OpenShift BuildConfig
			buildName, _ := step["name"].(string)
			if buildName == "" {
				buildName = "opinai-build"
			}
			dockerfile := content
			if dockerfile == "" {
				dockerfile = "Dockerfile"
			}
			builtImage, buildErr := m.runBuild(ns, repo, buildName, dockerfile)
			if buildErr != nil {
				stepErr = buildErr
			} else {
				slog.Info("build step completed", "image", builtImage, "name", buildName)
				// Store the built image so subsequent manifest steps can reference it
				if result["built_images"] == nil {
					result["built_images"] = map[string]string{}
				}
				result["built_images"].(map[string]string)[buildName] = builtImage
			}
			lastStepWasManifest = false
		case "manifest":
			// Replace image placeholders with built images
			if builtImages, ok := result["built_images"].(map[string]string); ok && len(builtImages) > 0 {
				for _, img := range builtImages {
					content = strings.ReplaceAll(content, "IMAGE_PLACEHOLDER", img)
				}
			}
			stepErr = applyManifests(m.client, m.dynClient, m.disco, ns, content)
			lastStepWasManifest = true
		case "wait":
			timeout := 120
			if t, ok := step["timeout_seconds"].(float64); ok {
				timeout = int(t)
			}
			if !waitForReady(m.client, ns, content, timeout) {
				stepErr = fmt.Errorf("timeout waiting for %s", content)
			}
			lastStepWasManifest = false
		case "shell", "command":
			// Allow K8s API to propagate resources from prior manifest steps
			if lastStepWasManifest {
				time.Sleep(2 * time.Second)
				lastStepWasManifest = false
			}
			if !cloneOK {
				slog.Warn("skipping command step — repo clone failed", "step", i+1, "desc", desc)
				break
			}
			// Note: /tmp clones are cleaned between options by CleanDeployClones, not per-step
			slog.Info("executing command step", "step", i+1, "desc", desc)
			cmdStr, workDir := stripCdPrefix(content, cloneDir)
			// Auto-run helm dependency build before helm install/upgrade
			if isHelmCommand(cmdStr) {
				helmDep := buildHelmDepCommand(workDir)
				if helmDep != "" {
					slog.Info("auto-running helm dependency build", "cmd", helmDep)
					depCmd := exec.Command("sh", "-c", helmDep)
					depCmd.Dir = workDir
					depCmd.Env = commandEnv(ns)
					depCmd.CombinedOutput() // best-effort
				}
			}
			cmdStr = injectNamespace(cmdStr, ns)
			cmd := exec.Command("sh", "-c", cmdStr)
			cmd.Dir = workDir
			cmd.Env = commandEnv(ns)
			out, err := cmd.CombinedOutput()
			if err != nil {
				stepErr = fmt.Errorf("%s: %s", err, string(out))
			} else {
				slog.Info("command step output", "output", truncLog(string(out), 200))
			}
		}

		if stepErr != nil {
			errs := result["errors"].([]string)
			errs = append(errs, fmt.Sprintf("Step %d (%s): %s", i+1, desc, stepErr))
			result["errors"] = errs
			if required {
				result["success"] = false
				break
			}
		}
		result["steps_completed"] = i + 1
		slog.Info("deployment step complete", "step", i+1, "total", len(steps), "desc", desc)
	}

	// Wait for all workloads to become healthy before returning success
	if result["success"] == true {
		if !m.WaitForAllHealthy(ns, 120) {
			errs := result["errors"].([]string)
			errs = append(errs, "some workloads did not become healthy within timeout")
			result["errors"] = errs
			slog.Warn("sandbox: not all workloads healthy after deployment", "namespace", ns)
		}
	}

	result["endpoints"] = m.GetEndpoints(ns)
	return result, nil
}

// GetEndpoints lists services in the namespace and returns name→FQDN map.
func (m *Manager) GetEndpoints(ns string) map[string]string {
	if !ValidateName(ns) {
		return nil
	}
	ctx := context.Background()
	svcs, err := m.client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	result := make(map[string]string, len(svcs.Items))
	for _, svc := range svcs.Items {
		result[svc.Name] = fmt.Sprintf("%s.%s.svc.cluster.local", svc.Name, ns)
	}
	return result
}

// TeardownSandbox deletes a namespace if it has the correct prefix and label.
func (m *Manager) TeardownSandbox(ns string) bool {
	if !ValidateName(ns) {
		slog.Warn("refusing to teardown: invalid prefix", "namespace", ns)
		return false
	}
	ctx := context.Background()
	nsObj, err := m.client.CoreV1().Namespaces().Get(ctx, ns, metav1.GetOptions{})
	if errors.IsNotFound(err) {
		return true
	}
	if err != nil {
		return false
	}
	if nsObj.Labels[ManagedLabelKey] != "true" {
		slog.Warn("refusing to teardown: missing managed label", "namespace", ns)
		return false
	}
	err = m.client.CoreV1().Namespaces().Delete(ctx, ns, metav1.DeleteOptions{})
	if err != nil {
		slog.Error("failed to delete namespace", "namespace", ns, "error", err)
		return false
	}
	slog.Info("torn down sandbox", "namespace", ns)
	return true
}

// SandboxInfo describes an active sandbox.
type SandboxInfo struct {
	Namespace  string `json:"namespace"`
	Repo       string `json:"repo"`
	Issue      string `json:"issue"`
	CreatedAt  string `json:"created_at"`
	AgeSeconds int    `json:"age_seconds"`
}

// ListSandboxes returns active sandbox namespaces.
func (m *Manager) ListSandboxes() []SandboxInfo {
	ctx := context.Background()
	nsList, err := m.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: ManagedLabelKey + "=true",
	})
	if err != nil {
		slog.Error("failed to list sandboxes", "error", err)
		return nil
	}

	var result []SandboxInfo
	for _, ns := range nsList.Items {
		if !ValidateName(ns.Name) {
			continue
		}
		annotations := ns.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		labels := ns.Labels
		if labels == nil {
			labels = map[string]string{}
		}

		createdAt := annotations["opinai.dev/created-at"]
		age := 0
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			age = int(time.Since(t).Seconds())
		}

		result = append(result, SandboxInfo{
			Namespace:  ns.Name,
			Repo:       annotations["opinai.dev/repo-full"],
			Issue:      labels["opinai.dev/issue"],
			CreatedAt:  createdAt,
			AgeSeconds: age,
		})
	}
	return result
}

// WaitForAllHealthy waits until all Deployments, StatefulSets, and Services in the
// namespace are ready. Uses exponential backoff (1s, 2s, 4s, ..., max 15s).
func (m *Manager) WaitForAllHealthy(ns string, timeout int) bool {
	if !ValidateName(ns) {
		return false
	}
	ctx := context.Background()
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)
	backoff := time.Second

	for time.Now().Before(deadline) {
		allReady := true

		// Check Deployments
		deps, err := m.client.AppsV1().Deployments(ns).List(ctx, metav1.ListOptions{})
		if err == nil {
			for _, dep := range deps.Items {
				desired := int32(1)
				if dep.Spec.Replicas != nil {
					desired = *dep.Spec.Replicas
				}
				if dep.Status.ReadyReplicas < desired {
					allReady = false
					break
				}
			}
		}

		// Check StatefulSets
		if allReady {
			stsList, err := m.client.AppsV1().StatefulSets(ns).List(ctx, metav1.ListOptions{})
			if err == nil {
				for _, sts := range stsList.Items {
					desired := int32(1)
					if sts.Spec.Replicas != nil {
						desired = *sts.Spec.Replicas
					}
					if sts.Status.ReadyReplicas < desired {
						allReady = false
						break
					}
				}
			}
		}

		// Check Services have ready endpoints
		if allReady {
			svcs, err := m.client.CoreV1().Services(ns).List(ctx, metav1.ListOptions{})
			if err == nil {
				for _, svc := range svcs.Items {
					// Skip headless/ExternalName services
					if svc.Spec.ClusterIP == "None" || svc.Spec.Type == corev1.ServiceTypeExternalName {
						continue
					}
					ep, err := m.client.CoreV1().Endpoints(ns).Get(ctx, svc.Name, metav1.GetOptions{})
					if err != nil {
						allReady = false
						break
					}
					hasAddr := false
					for _, subset := range ep.Subsets {
						if len(subset.Addresses) > 0 {
							hasAddr = true
							break
						}
					}
					if !hasAddr {
						allReady = false
						break
					}
				}
			}
		}

		if allReady {
			slog.Info("sandbox: all workloads healthy", "namespace", ns)
			return true
		}

		time.Sleep(backoff)
		backoff *= 2
		if backoff > 15*time.Second {
			backoff = 15 * time.Second
		}
	}
	return false
}

// AutoCleanup deletes sandboxes older than maxAge seconds. Returns count deleted.
// Per-namespace timeout annotations override the default maxAge.
func (m *Manager) AutoCleanup(maxAge int) int {
	ctx := context.Background()
	nsList, err := m.client.CoreV1().Namespaces().List(ctx, metav1.ListOptions{
		LabelSelector: ManagedLabelKey + "=true",
	})
	if err != nil {
		return 0
	}
	count := 0
	for _, ns := range nsList.Items {
		if !ValidateName(ns.Name) {
			continue
		}
		annotations := ns.Annotations
		if annotations == nil {
			annotations = map[string]string{}
		}
		createdAt := annotations["opinai.dev/created-at"]
		age := 0
		if t, err := time.Parse(time.RFC3339, createdAt); err == nil {
			age = int(time.Since(t).Seconds())
		}
		// Per-namespace timeout overrides the default
		nsMaxAge := maxAge
		if tmStr := annotations["opinai.dev/timeout-minutes"]; tmStr != "" {
			var tm int
			if n, _ := fmt.Sscanf(tmStr, "%d", &tm); n == 1 && tm > 0 {
				nsMaxAge = tm * 60
			}
		}
		if age > nsMaxAge {
			if m.TeardownSandbox(ns.Name) {
				count++
			}
		}
	}
	if count > 0 {
		slog.Info("auto-cleaned sandboxes", "count", count)
	}
	return count
}

// --- build helpers ---

// runBuild creates an OpenShift BuildConfig + ImageStream via the dynamic client,
// starts a build, and waits for it to complete. Returns the output image reference.
func (m *Manager) runBuild(ns, repo, name, dockerfile string) (string, error) {
	if m.dynClient == nil {
		return "", fmt.Errorf("dynamic client not available — cannot create BuildConfig")
	}

	ctx := context.Background()
	imageRef := fmt.Sprintf("image-registry.openshift-image-registry.svc:5000/%s/%s:latest", ns, name)

	// Create ImageStream
	is := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "image.openshift.io/v1",
		"kind":       "ImageStream",
		"metadata":   map[string]any{"name": name, "namespace": ns},
	}}
	isGVR := schema.GroupVersionResource{Group: "image.openshift.io", Version: "v1", Resource: "imagestreams"}
	_, err := m.dynClient.Resource(isGVR).Namespace(ns).Create(ctx, is, metav1.CreateOptions{})
	if err != nil && !errors.IsAlreadyExists(err) {
		return "", fmt.Errorf("create ImageStream: %w", err)
	}

	// Determine git URI
	gitURI := "https://github.com/" + repo + ".git"
	token := os.Getenv("GITHUB_TOKEN")
	if token != "" {
		gitURI = "https://x-access-token:" + token + "@github.com/" + repo + ".git"
	}

	// Create BuildConfig
	bc := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "build.openshift.io/v1",
		"kind":       "BuildConfig",
		"metadata":   map[string]any{"name": name, "namespace": ns},
		"spec": map[string]any{
			"source": map[string]any{
				"type": "Git",
				"git":  map[string]any{"uri": gitURI},
			},
			"strategy": map[string]any{
				"type": "Docker",
				"dockerStrategy": map[string]any{
					"dockerfilePath": dockerfile,
				},
			},
			"output": map[string]any{
				"to": map[string]any{
					"kind": "ImageStreamTag",
					"name": name + ":latest",
				},
			},
		},
	}}
	bcGVR := schema.GroupVersionResource{Group: "build.openshift.io", Version: "v1", Resource: "buildconfigs"}
	_, err = m.dynClient.Resource(bcGVR).Namespace(ns).Create(ctx, bc, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("create BuildConfig: %w", err)
	}

	// Start a build
	build := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "build.openshift.io/v1",
		"kind":       "Build",
		"metadata": map[string]any{
			"name":      name + "-1",
			"namespace": ns,
			"labels":    map[string]any{"buildconfig": name},
		},
		"spec": map[string]any{
			"source": map[string]any{
				"type": "Git",
				"git":  map[string]any{"uri": gitURI},
			},
			"strategy": map[string]any{
				"type": "Docker",
				"dockerStrategy": map[string]any{
					"dockerfilePath": dockerfile,
				},
			},
			"output": map[string]any{
				"to": map[string]any{
					"kind": "ImageStreamTag",
					"name": name + ":latest",
				},
			},
		},
	}}
	buildGVR := schema.GroupVersionResource{Group: "build.openshift.io", Version: "v1", Resource: "builds"}
	_, err = m.dynClient.Resource(buildGVR).Namespace(ns).Create(ctx, build, metav1.CreateOptions{})
	if err != nil {
		return "", fmt.Errorf("start build: %w", err)
	}

	// Wait for build to complete (up to 10 minutes)
	slog.Info("waiting for build to complete", "build", name+"-1", "namespace", ns)
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		obj, err := m.dynClient.Resource(buildGVR).Namespace(ns).Get(ctx, name+"-1", metav1.GetOptions{})
		if err == nil {
			phase, _, _ := unstructured.NestedString(obj.Object, "status", "phase")
			switch phase {
			case "Complete":
				slog.Info("build completed", "build", name+"-1", "image", imageRef)
				return imageRef, nil
			case "Failed", "Error", "Cancelled":
				msg, _, _ := unstructured.NestedString(obj.Object, "status", "message")
				return "", fmt.Errorf("build %s failed: %s — %s", name+"-1", phase, msg)
			}
		}
		time.Sleep(10 * time.Second)
	}
	return "", fmt.Errorf("build %s timed out after 10 minutes", name+"-1")
}

// --- manifest helpers ---

// applyManifests handles multi-document YAML/JSON content.
func applyManifests(client kubernetes.Interface, dynClient dynamic.Interface, disco discovery.DiscoveryInterface, ns, content string) error {
	content = strings.TrimSpace(content)
	if content == "" {
		return fmt.Errorf("empty manifest content")
	}

	// Split multi-document YAML on "---" separator
	docs := splitYAMLDocs(content)
	for i, doc := range docs {
		doc = strings.TrimSpace(doc)
		if doc == "" || doc == "---" {
			continue
		}
		if err := applySingleManifest(client, dynClient, disco, ns, doc); err != nil {
			return fmt.Errorf("document %d: %w", i+1, err)
		}
	}
	return nil
}

// splitYAMLDocs splits multi-document YAML on "---" lines.
func splitYAMLDocs(content string) []string {
	// If it looks like single JSON object, don't split
	trimmed := strings.TrimSpace(content)
	if strings.HasPrefix(trimmed, "{") {
		return []string{content}
	}

	var docs []string
	var current strings.Builder
	for _, line := range strings.Split(content, "\n") {
		if strings.TrimSpace(line) == "---" {
			if current.Len() > 0 {
				docs = append(docs, current.String())
				current.Reset()
			}
			continue
		}
		current.WriteString(line)
		current.WriteString("\n")
	}
	if current.Len() > 0 {
		docs = append(docs, current.String())
	}
	if len(docs) == 0 {
		docs = []string{content}
	}
	return docs
}

// applySingleManifest parses a single YAML or JSON manifest and creates the resource.
func applySingleManifest(client kubernetes.Interface, dynClient dynamic.Interface, disco discovery.DiscoveryInterface, ns, content string) error {
	// Parse content — try JSON first, then YAML
	var doc map[string]any
	content = strings.TrimSpace(content)

	if err := json.Unmarshal([]byte(content), &doc); err != nil {
		// Try YAML → JSON conversion
		jsonBytes, yamlErr := sigYAML.YAMLToJSON([]byte(content))
		if yamlErr != nil {
			return fmt.Errorf("invalid manifest (not valid JSON or YAML): json=%v, yaml=%v", err, yamlErr)
		}
		if err2 := json.Unmarshal(jsonBytes, &doc); err2 != nil {
			return fmt.Errorf("YAML converted but failed to parse as JSON: %w", err2)
		}
	}

	if doc == nil {
		return nil // empty document
	}

	kind, _ := doc["kind"].(string)
	if kind == "" {
		return fmt.Errorf("manifest missing 'kind' field")
	}
	if SkippedKinds[kind] {
		slog.Info("skipping resource — cluster-scoped or already exists", "kind", kind)
		return nil
	}
	if !AllowedKinds[kind] {
		if dynClient == nil || disco == nil {
			slog.Warn("skipping CRD resource — dynamic client not available", "kind", kind)
			return nil
		}
		return applyDynamic(dynClient, disco, ns, doc, kind)
	}

	// RBAC pre-check for sensitive kinds
	if !checkCanCreate(client, ns, kind) {
		slog.Warn("skipping resource — insufficient RBAC permissions", "kind", kind, "namespace", ns)
		return nil
	}

	// Force namespace and managed label
	meta, _ := doc["metadata"].(map[string]any)
	if meta == nil {
		meta = map[string]any{}
		doc["metadata"] = meta
	}
	meta["namespace"] = ns
	labels, _ := meta["labels"].(map[string]any)
	if labels == nil {
		labels = map[string]any{}
		meta["labels"] = labels
	}
	labels[ManagedLabelKey] = "true"

	data, _ := json.Marshal(doc)
	ctx := context.Background()

	switch kind {
	case "Deployment":
		var dep appsv1.Deployment
		json.Unmarshal(data, &dep)
		_, err := client.AppsV1().Deployments(ns).Create(ctx, &dep, metav1.CreateOptions{})
		return err
	case "StatefulSet":
		var sts appsv1.StatefulSet
		json.Unmarshal(data, &sts)
		_, err := client.AppsV1().StatefulSets(ns).Create(ctx, &sts, metav1.CreateOptions{})
		return err
	case "Service":
		var svc corev1.Service
		json.Unmarshal(data, &svc)
		_, err := client.CoreV1().Services(ns).Create(ctx, &svc, metav1.CreateOptions{})
		return err
	case "ConfigMap":
		var cm corev1.ConfigMap
		json.Unmarshal(data, &cm)
		_, err := client.CoreV1().ConfigMaps(ns).Create(ctx, &cm, metav1.CreateOptions{})
		return err
	case "Secret":
		var sec corev1.Secret
		json.Unmarshal(data, &sec)
		_, err := client.CoreV1().Secrets(ns).Create(ctx, &sec, metav1.CreateOptions{})
		return err
	case "ServiceAccount":
		var sa corev1.ServiceAccount
		json.Unmarshal(data, &sa)
		_, err := client.CoreV1().ServiceAccounts(ns).Create(ctx, &sa, metav1.CreateOptions{})
		return err
	case "PersistentVolumeClaim":
		var pvc corev1.PersistentVolumeClaim
		json.Unmarshal(data, &pvc)
		_, err := client.CoreV1().PersistentVolumeClaims(ns).Create(ctx, &pvc, metav1.CreateOptions{})
		return err
	default:
		return fmt.Errorf("kind %q not yet supported for apply", kind)
	}
}

// cloneRepoForDeploy clones the target repo into a temp directory for command steps.
// Returns (cloneDir, success). On failure returns ("", false) — command steps should be skipped.
func cloneRepoForDeploy(repo string) (string, bool) {
	if repo == "" {
		return "", false
	}

	repoShort := repo
	if idx := strings.LastIndex(repo, "/"); idx >= 0 {
		repoShort = repo[idx+1:]
	}
	cloneDir := filepath.Join(os.TempDir(), "opinai-deploy-"+repoShort)

	// Remove any previous clone
	os.RemoveAll(cloneDir)

	slog.Info("cloning repo for deployment steps", "repo", repo, "dir", cloneDir)
	cloneCtx, cloneCancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cloneCancel()
	cmd := exec.CommandContext(cloneCtx, "git", "clone", "--depth=1", "https://github.com/"+repo+".git", cloneDir)
	out, err := cmd.CombinedOutput()
	if err != nil {
		slog.Warn("repo clone failed — command steps will be skipped", "repo", repo, "error", err, "output", truncLog(string(out), 200))
		return "", false
	}
	slog.Info("repo cloned for deployment", "dir", cloneDir)
	return cloneDir, true
}

// CleanDeployClones removes /tmp/opinai-deploy-* directories left from prior attempts.
func CleanDeployClones(repo string) {
	entries, err := filepath.Glob(filepath.Join(os.TempDir(), "opinai-deploy-*"))
	if err == nil {
		for _, e := range entries {
			os.RemoveAll(e)
		}
	}
	cleanTmpClones(repo)
}

// cleanTmpClones removes stale repo clones from /tmp that AI-generated command steps
// may have created (e.g. "git clone ... /tmp/<repo-name>").
func cleanTmpClones(repo string) {
	if repo == "" {
		return
	}
	repoShort := repo
	if idx := strings.LastIndex(repo, "/"); idx >= 0 {
		repoShort = repo[idx+1:]
	}
	// Clean common clone targets AI might use
	for _, pattern := range []string{
		filepath.Join(os.TempDir(), repoShort),
		filepath.Join(os.TempDir(), strings.ToLower(repoShort)),
	} {
		if info, err := os.Stat(pattern); err == nil && info.IsDir() {
			os.RemoveAll(pattern)
			slog.Info("cleaned stale tmp clone", "path", pattern)
		}
	}
}

// commandEnv builds the environment for command step execution.
func commandEnv(namespace string) []string {
	env := os.Environ()
	// Ensure commands target the sandbox namespace
	env = append(env, "NAMESPACE="+namespace)
	// Add pip paths in case commands need Python
	env = append(env, "PYTHONUSERBASE=/tmp/pip-user")
	// Update PATH
	for i, kv := range env {
		if strings.HasPrefix(kv, "PATH=") {
			env[i] = "PATH=/tmp/pip-user/bin:/usr/local/bin:" + kv[5:]
			return env
		}
	}
	return env
}

// stripCdPrefix removes a leading "cd <dir> &&" from a command and resolves the workDir.
// If the cd target is a subdirectory of cloneDir, uses that as workDir.
// Otherwise uses cloneDir as workDir and strips the cd entirely.
func stripCdPrefix(cmd, cloneDir string) (string, string) {
	trimmed := strings.TrimSpace(cmd)

	// Match patterns: "cd something &&", "cd something;", "cd something\n"
	for _, sep := range []string{" && ", " &&\n", "; ", ";\n"} {
		if idx := strings.Index(trimmed, sep); idx > 0 {
			prefix := trimmed[:idx]
			rest := strings.TrimSpace(trimmed[idx+len(sep):])

			if strings.HasPrefix(prefix, "cd ") {
				target := strings.TrimSpace(prefix[3:])
				// Remove quotes
				target = strings.Trim(target, "'\"")

				// Check if target dir exists under cloneDir
				candidate := filepath.Join(cloneDir, target)
				if info, err := os.Stat(candidate); err == nil && info.IsDir() {
					return rest, candidate
				}
				// Target doesn't exist as subdir — just strip the cd and use cloneDir
				return rest, cloneDir
			}
		}
	}

	return cmd, cloneDir
}

// injectNamespace adds -n {namespace} to oc/kubectl commands if not already present.
func injectNamespace(cmd, ns string) string {
	// Check if command uses oc or kubectl
	for _, prefix := range []string{"oc ", "kubectl "} {
		if strings.Contains(cmd, prefix) {
			// Replace any existing -n <namespace> or --namespace <namespace> with sandbox NS
			// This prevents AI-hardcoded namespaces from bypassing sandbox isolation
			cmd = replaceExistingNamespace(cmd, ns)
			if !strings.Contains(cmd, " -n ") && !strings.Contains(cmd, " --namespace") {
				// Insert -n after the first oc/kubectl subcommand
				cmd = strings.Replace(cmd, prefix, prefix+"-n "+ns+" ", 1)
			}
			return cmd
		}
	}
	return cmd
}

// replaceExistingNamespace replaces -n <ns> or --namespace <ns> or --namespace=<ns> with the sandbox NS.
func replaceExistingNamespace(cmd, ns string) string {
	// Handle -n <value>
	parts := strings.Fields(cmd)
	var result []string
	for i := 0; i < len(parts); i++ {
		if (parts[i] == "-n" || parts[i] == "--namespace") && i+1 < len(parts) {
			result = append(result, parts[i], ns)
			i++ // skip original namespace value
		} else if strings.HasPrefix(parts[i], "--namespace=") {
			result = append(result, "--namespace="+ns)
		} else {
			result = append(result, parts[i])
		}
	}
	return strings.Join(result, " ")
}

// --- Helm helpers ---

func isHelmCommand(cmd string) bool {
	return strings.Contains(cmd, "helm install") || strings.Contains(cmd, "helm upgrade")
}

func buildHelmDepCommand(workDir string) string {
	// Look for Chart.yaml in the work directory or common subdirs
	for _, sub := range []string{".", "chart", "charts", "helm", "deploy/helm"} {
		chartFile := filepath.Join(workDir, sub, "Chart.yaml")
		if _, err := os.Stat(chartFile); err == nil {
			return "cd " + filepath.Join(workDir, sub) + " && helm dependency build"
		}
	}
	return ""
}

// --- RBAC pre-check ---

func checkCanCreate(client kubernetes.Interface, ns, kind string) bool {
	// Only check for sensitive kinds that commonly fail
	apiGroup := ""
	resource := ""
	switch kind {
	case "Deployment", "StatefulSet":
		apiGroup = "apps"
		resource = strings.ToLower(kind) + "s"
	case "Role", "RoleBinding":
		apiGroup = "rbac.authorization.k8s.io"
		resource = strings.ToLower(kind) + "s"
	case "Ingress":
		apiGroup = "networking.k8s.io"
		resource = "ingresses"
	case "Route":
		apiGroup = "route.openshift.io"
		resource = "routes"
	default:
		return true // don't check common resources (Service, ConfigMap, etc.)
	}

	ctx := context.Background()
	review := &authv1.SelfSubjectAccessReview{
		Spec: authv1.SelfSubjectAccessReviewSpec{
			ResourceAttributes: &authv1.ResourceAttributes{
				Namespace: ns,
				Verb:      "create",
				Group:     apiGroup,
				Resource:  resource,
			},
		},
	}
	result, err := client.AuthorizationV1().SelfSubjectAccessReviews().Create(ctx, review, metav1.CreateOptions{})
	if err != nil {
		return true // if check fails, try anyway
	}
	return result.Status.Allowed
}

// applyDynamic uses the dynamic client to create a CRD-based resource in the sandbox namespace.
func applyDynamic(dynClient dynamic.Interface, disco discovery.DiscoveryInterface, ns string, doc map[string]any, kind string) error {
	apiVersion, _ := doc["apiVersion"].(string)
	if apiVersion == "" {
		return fmt.Errorf("CRD resource %s missing apiVersion", kind)
	}

	gvr, err := resolveGVR(disco, apiVersion, kind)
	if err != nil {
		slog.Warn("skipping CRD resource — cannot resolve GVR", "kind", kind, "apiVersion", apiVersion, "error", err)
		return nil
	}

	data, _ := json.Marshal(doc)
	obj := &unstructured.Unstructured{}
	if err := json.Unmarshal(data, &obj.Object); err != nil {
		return fmt.Errorf("parse CRD manifest: %w", err)
	}

	obj.SetNamespace(ns)
	labels := obj.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ManagedLabelKey] = "true"
	obj.SetLabels(labels)

	ctx := context.Background()
	_, err = dynClient.Resource(gvr).Namespace(ns).Create(ctx, obj, metav1.CreateOptions{})
	if err != nil {
		return fmt.Errorf("create %s: %w", kind, err)
	}
	slog.Info("created CRD resource via dynamic client", "kind", kind, "name", obj.GetName(), "namespace", ns)
	return nil
}

// resolveGVR finds the GroupVersionResource for a given apiVersion + kind using discovery.
func resolveGVR(disco discovery.DiscoveryInterface, apiVersion, kind string) (schema.GroupVersionResource, error) {
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("parse apiVersion %q: %w", apiVersion, err)
	}

	resources, err := disco.ServerResourcesForGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionResource{}, fmt.Errorf("discover resources for %s: %w", apiVersion, err)
	}

	for _, r := range resources.APIResources {
		if r.Kind == kind {
			return schema.GroupVersionResource{
				Group:    gv.Group,
				Version:  gv.Version,
				Resource: r.Name,
			}, nil
		}
	}
	return schema.GroupVersionResource{}, fmt.Errorf("kind %q not found in %s", kind, apiVersion)
}

func truncLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

func waitForReady(client kubernetes.Interface, ns, resourceRef string, timeout int) bool {
	parts := strings.SplitN(strings.TrimSpace(resourceRef), "/", 2)
	if len(parts) != 2 {
		return false
	}
	kind, name := strings.ToLower(parts[0]), parts[1]
	ctx := context.Background()
	deadline := time.Now().Add(time.Duration(timeout) * time.Second)

	for time.Now().Before(deadline) {
		switch kind {
		case "deployment":
			dep, err := client.AppsV1().Deployments(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				desired := int32(1)
				if dep.Spec.Replicas != nil {
					desired = *dep.Spec.Replicas
				}
				if dep.Status.ReadyReplicas >= desired {
					return true
				}
			}
		case "statefulset":
			sts, err := client.AppsV1().StatefulSets(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				desired := int32(1)
				if sts.Spec.Replicas != nil {
					desired = *sts.Spec.Replicas
				}
				if sts.Status.ReadyReplicas >= desired {
					return true
				}
			}
		case "service":
			ep, err := client.CoreV1().Endpoints(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil {
				for _, subset := range ep.Subsets {
					if len(subset.Addresses) > 0 {
						return true
					}
				}
			}
		case "job":
			j, err := client.BatchV1().Jobs(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil && j.Status.Succeeded >= 1 {
				return true
			}
		case "pod":
			pod, err := client.CoreV1().Pods(ns).Get(ctx, name, metav1.GetOptions{})
			if err == nil && pod.Status.Phase == corev1.PodRunning {
				return true
			}
		}
		time.Sleep(5 * time.Second)
	}
	return false
}
