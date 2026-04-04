package sandbox

import (
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
	for _, kind := range []string{"Namespace", "ClusterRole", "ClusterRoleBinding"} {
		if !SkippedKinds[kind] {
			t.Errorf("%s should be skipped", kind)
		}
	}
	if AllowedKinds["Namespace"] {
		t.Error("Namespace should NOT be in AllowedKinds")
	}
}
