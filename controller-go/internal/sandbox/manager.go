// Package sandbox manages temporary K8s namespaces for deployment testing.
package sandbox

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os/exec"
	"strings"
	"time"

	sigYAML "sigs.k8s.io/yaml"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/kubernetes"

	appsv1 "k8s.io/api/apps/v1"
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
	namespace string // controller namespace (for network policy)
}

// NewManager creates a sandbox manager.
func NewManager(client kubernetes.Interface, controllerNS string) *Manager {
	return &Manager{client: client, namespace: controllerNS}
}

// ValidateName returns true if the namespace starts with the sandbox prefix.
func ValidateName(ns string) bool {
	return strings.HasPrefix(ns, SandboxPrefix)
}

// CreateSandbox creates an isolated namespace with quotas and network policy.
func (m *Manager) CreateSandbox(repo string, issue int) (string, error) {
	repoShort := repo
	if idx := strings.LastIndex(repo, "/"); idx >= 0 {
		repoShort = repo[idx+1:]
	}
	if len(repoShort) > 20 {
		repoShort = repoShort[:20]
	}
	repoShort = strings.ToLower(strings.ReplaceAll(repoShort, ".", "-"))
	ts := fmt.Sprintf("%d", time.Now().Unix()%1000000)

	ns := fmt.Sprintf("%s%s-%d-%s", SandboxPrefix, repoShort, issue, ts)
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
				"opinai.dev/created-at": time.Now().UTC().Format(time.RFC3339),
				"opinai.dev/repo-full":  repo,
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
				corev1.ResourceRequestsCPU:              resource.MustParse("1"),
				corev1.ResourceRequestsMemory:           resource.MustParse("1Gi"),
				corev1.ResourceLimitsCPU:                resource.MustParse("2"),
				corev1.ResourceLimitsMemory:             resource.MustParse("2Gi"),
				corev1.ResourcePods:                     resource.MustParse("10"),
				corev1.ResourceServices:                 resource.MustParse("5"),
				corev1.ResourcePersistentVolumeClaims:   resource.MustParse("3"),
			},
		},
	}, metav1.CreateOptions{})
	if err != nil {
		slog.Warn("failed to create resource quota", "namespace", ns, "error", err)
	}

	// NetworkPolicy
	protocol := corev1.ProtocolUDP
	protocolTCP := corev1.ProtocolTCP
	port53 := intstr.FromInt(53)
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
				{To: []networkingv1.NetworkPolicyPeer{{PodSelector: &metav1.LabelSelector{}}}},
				{Ports: []networkingv1.NetworkPolicyPort{
					{Port: &port53, Protocol: &protocol},
					{Port: &port53, Protocol: &protocolTCP},
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

	result := map[string]any{
		"success":         true,
		"steps_completed": 0,
		"steps_total":     len(steps),
		"errors":          []string{},
		"endpoints":       map[string]string{},
	}

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
		case "manifest":
			stepErr = applyManifests(m.client, ns, content)
		case "wait":
			timeout := 120
			if t, ok := step["timeout_seconds"].(float64); ok {
				timeout = int(t)
			}
			if !waitForReady(m.client, ns, content, timeout) {
				stepErr = fmt.Errorf("timeout waiting for %s", content)
			}
		case "shell", "command":
			slog.Info("executing command step", "step", i+1, "desc", desc)
			cmdStr := injectNamespace(content, ns)
			cmd := exec.Command("sh", "-c", cmdStr)
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

// AutoCleanup deletes sandboxes older than maxAge seconds. Returns count deleted.
func (m *Manager) AutoCleanup(maxAge int) int {
	sandboxes := m.ListSandboxes()
	count := 0
	for _, sb := range sandboxes {
		if sb.AgeSeconds > maxAge {
			if m.TeardownSandbox(sb.Namespace) {
				count++
			}
		}
	}
	if count > 0 {
		slog.Info("auto-cleaned sandboxes", "count", count)
	}
	return count
}

// --- manifest helpers ---

// applyManifests handles multi-document YAML/JSON content.
func applyManifests(client kubernetes.Interface, ns, content string) error {
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
		if err := applySingleManifest(client, ns, doc); err != nil {
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
func applySingleManifest(client kubernetes.Interface, ns, content string) error {
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
		slog.Warn("skipping unsupported resource kind", "kind", kind)
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

// injectNamespace adds -n {namespace} to oc/kubectl commands if not already present.
func injectNamespace(cmd, ns string) string {
	// Check if command uses oc or kubectl
	for _, prefix := range []string{"oc ", "kubectl "} {
		if strings.Contains(cmd, prefix) {
			// Skip if -n or --namespace already specified
			if strings.Contains(cmd, " -n ") || strings.Contains(cmd, " --namespace") {
				return cmd
			}
			// Insert -n after the first oc/kubectl subcommand
			return strings.Replace(cmd, prefix, prefix+"-n "+ns+" ", 1)
		}
	}
	return cmd
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
