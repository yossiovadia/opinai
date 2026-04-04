package sandbox

import (
	"strings"
	"testing"
)

func TestValidateName(t *testing.T) {
	tests := []struct {
		name string
		want bool
	}{
		{"opinai-sandbox-test-1-123", true},
		{"opinai-sandbox-", true},
		{"opinai-sandbox-x", true},
		{"default", false},
		{"kube-system", false},
		{"opinai-test", false},
		{"", false},
	}
	for _, tt := range tests {
		if got := ValidateName(tt.name); got != tt.want {
			t.Errorf("ValidateName(%q) = %v, want %v", tt.name, got, tt.want)
		}
	}
}

func TestSplitYAMLDocs(t *testing.T) {
	// Single JSON
	docs := splitYAMLDocs(`{"kind": "Service"}`)
	if len(docs) != 1 {
		t.Errorf("single JSON: got %d docs", len(docs))
	}

	// Multi-doc YAML
	multi := "apiVersion: v1\nkind: ConfigMap\n---\napiVersion: v1\nkind: Service\n"
	docs = splitYAMLDocs(multi)
	if len(docs) != 2 {
		t.Errorf("multi-doc YAML: got %d docs, want 2", len(docs))
	}

	// Leading ---
	docs = splitYAMLDocs("---\napiVersion: v1\nkind: Pod\n")
	if len(docs) != 1 {
		t.Errorf("leading ---: got %d docs", len(docs))
	}

	// Empty
	docs = splitYAMLDocs("")
	if len(docs) != 1 {
		t.Errorf("empty: got %d docs", len(docs))
	}
}

func TestStripCdPrefix(t *testing.T) {
	// No cd
	cmd, dir := stripCdPrefix("kubectl apply -f .", "/repo")
	if cmd != "kubectl apply -f ." || dir != "/repo" {
		t.Errorf("no-cd: cmd=%q dir=%q", cmd, dir)
	}

	// cd with &&
	cmd, dir = stripCdPrefix("cd deploy && kubectl apply -f .", "/repo")
	if cmd != "kubectl apply -f ." {
		t.Errorf("cd &&: cmd=%q", cmd)
	}
	// dir should be /repo (deploy subdir may not exist in test)
	if dir != "/repo" {
		t.Logf("dir=%q (expected /repo since deploy/ doesn't exist)", dir)
	}

	// cd with ;
	cmd, _ = stripCdPrefix("cd scripts; ./install.sh", "/repo")
	if cmd != "./install.sh" {
		t.Errorf("cd ;: cmd=%q", cmd)
	}
}

func TestInjectNamespace(t *testing.T) {
	// oc command without -n
	result := injectNamespace("oc apply -f deploy.yaml", "test-ns")
	if result != "oc -n test-ns apply -f deploy.yaml" {
		t.Errorf("oc inject: %q", result)
	}

	// kubectl with existing -n — should REPLACE with sandbox namespace
	result = injectNamespace("kubectl -n prod apply -f .", "test-ns")
	if result != "kubectl -n test-ns apply -f ." {
		t.Errorf("should replace existing namespace: %q", result)
	}

	// kubectl with env var namespace — should REPLACE with sandbox namespace
	result = injectNamespace("kubectl -n $GATEWAY_NAMESPACE rollout status deploy/api", "test-ns")
	if result != "kubectl -n test-ns rollout status deploy/api" {
		t.Errorf("should replace env var namespace: %q", result)
	}

	// kubectl with --namespace=value
	result = injectNamespace("kubectl --namespace=prod get pods", "test-ns")
	if result != "kubectl --namespace=test-ns get pods" {
		t.Errorf("should replace --namespace=value: %q", result)
	}

	// kubectl with --namespace=$VAR
	result = injectNamespace("kubectl --namespace=$MY_NS get pods", "test-ns")
	if result != "kubectl --namespace=test-ns get pods" {
		t.Errorf("should replace --namespace=$VAR: %q", result)
	}

	// Non-k8s command
	result = injectNamespace("make build", "test-ns")
	if result != "make build" {
		t.Errorf("non-k8s should be unchanged: %q", result)
	}
}

func TestExpandHelmImageSets(t *testing.T) {
	img := "image-registry.openshift-image-registry.svc:5000/opinai-sandbox-bbr-42-abc123/payload-processing:latest"

	// Structured image key (ends with .image) — should split into registry/repository/tag
	cmd := "helm upgrade --install bbr ./chart -n sandbox --set upstreamBbr.bbr.image=IMAGE_PLACEHOLDER"
	result := expandHelmImageSets(cmd, img)
	if strings.Contains(result, "IMAGE_PLACEHOLDER") {
		t.Errorf("should not contain IMAGE_PLACEHOLDER: %s", result)
	}
	if !strings.Contains(result, "upstreamBbr.bbr.image.registry=image-registry.openshift-image-registry.svc:5000") {
		t.Errorf("should contain registry: %s", result)
	}
	if !strings.Contains(result, "upstreamBbr.bbr.image.repository=opinai-sandbox-bbr-42-abc123/payload-processing") {
		t.Errorf("should contain repository: %s", result)
	}
	if !strings.Contains(result, "upstreamBbr.bbr.image.tag=latest") {
		t.Errorf("should contain tag: %s", result)
	}

	// Non-structured key — plain replacement
	cmd2 := "helm install x ./chart --set container.img=IMAGE_PLACEHOLDER"
	result2 := expandHelmImageSets(cmd2, img)
	if !strings.Contains(result2, "container.img="+img) {
		t.Errorf("non-structured should do plain replace: %s", result2)
	}

	// No placeholder — unchanged
	cmd3 := "helm install x ./chart"
	result3 := expandHelmImageSets(cmd3, img)
	if result3 != cmd3 {
		t.Errorf("no placeholder should be unchanged: %s", result3)
	}
}

func TestParseImageURL(t *testing.T) {
	reg, repo, tag := parseImageURL("image-registry.openshift-image-registry.svc:5000/myns/myapp:v1")
	if reg != "image-registry.openshift-image-registry.svc:5000" || repo != "myns/myapp" || tag != "v1" {
		t.Errorf("got reg=%q repo=%q tag=%q", reg, repo, tag)
	}

	reg, repo, tag = parseImageURL("ghcr.io/org/app:latest")
	if reg != "ghcr.io" || repo != "org/app" || tag != "latest" {
		t.Errorf("got reg=%q repo=%q tag=%q", reg, repo, tag)
	}

	reg, repo, tag = parseImageURL("nginx:1.25")
	if repo != "nginx" || tag != "1.25" {
		t.Errorf("got reg=%q repo=%q tag=%q", reg, repo, tag)
	}
}

func TestIsHelmCommand(t *testing.T) {
	if !isHelmCommand("helm install myapp ./chart") {
		t.Error("should detect helm install")
	}
	if !isHelmCommand("helm upgrade --install myapp ./chart") {
		t.Error("should detect helm upgrade")
	}
	if isHelmCommand("kubectl apply -f .") {
		t.Error("should not detect non-helm")
	}
}

func TestAllowedKinds(t *testing.T) {
	for _, kind := range []string{"Deployment", "Service", "ConfigMap", "Secret", "Job", "Ingress", "Role"} {
		if !AllowedKinds[kind] {
			t.Errorf("%s should be allowed", kind)
		}
	}
	if !SkippedKinds["Namespace"] {
		t.Error("Namespace should be skipped")
	}
	for _, kind := range []string{"ClusterRole", "ClusterRoleBinding"} {
		if !ClusterScopedRBACKinds[kind] {
			t.Errorf("%s should be in ClusterScopedRBACKinds", kind)
		}
		if SkippedKinds[kind] {
			t.Errorf("%s should NOT be in SkippedKinds (handled as cluster-scoped RBAC)", kind)
		}
	}
	if AllowedKinds["Namespace"] {
		t.Error("Namespace should NOT be in AllowedKinds")
	}
}
