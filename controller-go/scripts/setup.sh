#!/usr/bin/env bash
# OpinAI Controller setup script — creates K8s resources for sandbox management.
# Usage: ./scripts/setup.sh [NAMESPACE]
set -euo pipefail

NAMESPACE="${1:-opinai}"
SA="opinai-controller"

echo "Setting up OpinAI in namespace: $NAMESPACE"

# Create namespace if it doesn't exist
kubectl get namespace "$NAMESPACE" &>/dev/null || kubectl create namespace "$NAMESPACE"

# Create ServiceAccount
kubectl -n "$NAMESPACE" get sa "$SA" &>/dev/null || \
  kubectl -n "$NAMESPACE" create serviceaccount "$SA"

# ClusterRole for sandbox management
cat <<'EOF' | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRole
metadata:
  name: opinai-sandbox-manager
rules:
  # Namespace lifecycle for sandboxes
  - apiGroups: [""]
    resources: ["namespaces"]
    verbs: ["create", "get", "list", "delete"]
  # Standard resources in sandbox namespaces
  - apiGroups: [""]
    resources: ["pods", "services", "configmaps", "secrets", "serviceaccounts",
                "persistentvolumeclaims", "endpoints", "resourcequotas",
                "limitranges", "events"]
    verbs: ["create", "get", "list", "watch", "update", "patch", "delete"]
  - apiGroups: ["apps"]
    resources: ["deployments", "statefulsets", "replicasets"]
    verbs: ["create", "get", "list", "watch", "update", "delete"]
  - apiGroups: ["batch"]
    resources: ["jobs", "cronjobs"]
    verbs: ["create", "get", "list", "watch", "delete"]
  - apiGroups: ["networking.k8s.io"]
    resources: ["networkpolicies", "ingresses"]
    verbs: ["create", "get", "list", "delete"]
  # Namespace-scoped and cluster-scoped RBAC
  - apiGroups: ["rbac.authorization.k8s.io"]
    resources: ["roles", "rolebindings", "clusterroles", "clusterrolebindings"]
    verbs: ["create", "get", "list", "watch", "delete"]
  - apiGroups: ["autoscaling"]
    resources: ["horizontalpodautoscalers"]
    verbs: ["create", "get", "list", "delete"]
  - apiGroups: ["policy"]
    resources: ["poddisruptionbudgets"]
    verbs: ["create", "get", "list", "delete"]
  # Controller-runtime / operator-sdk leader election
  - apiGroups: ["coordination.k8s.io"]
    resources: ["leases"]
    verbs: ["create", "get", "list", "watch", "update", "delete"]
  # OpenShift routes
  - apiGroups: ["route.openshift.io"]
    resources: ["routes"]
    verbs: ["create", "get", "list", "delete"]
  # OpenShift BuildConfig + ImageStream (for building container images)
  - apiGroups: ["build.openshift.io"]
    resources: ["buildconfigs", "buildconfigs/instantiate", "builds", "builds/log"]
    verbs: ["create", "get", "list", "watch", "delete"]
  - apiGroups: ["image.openshift.io"]
    resources: ["imagestreams", "imagestreamtags", "imagestreamimages"]
    verbs: ["create", "get", "list", "watch", "delete"]
  # CRD discovery (read-only, for dynamic client)
  - apiGroups: ["apiextensions.k8s.io"]
    resources: ["customresourcedefinitions"]
    verbs: ["get", "list"]
  # Common operator CRDs (Envoy, Gateway API, Istio, Kuadrant)
  - apiGroups: ["networking.istio.io"]
    resources: ["envoyfilters", "virtualservices", "destinationrules", "gateways"]
    verbs: ["create", "get", "list", "delete"]
  - apiGroups: ["gateway.networking.k8s.io"]
    resources: ["gateways", "httproutes", "grpcroutes", "referencegrants"]
    verbs: ["create", "get", "list", "delete"]
  - apiGroups: ["extensions.istio.io"]
    resources: ["wasmplugins"]
    verbs: ["create", "get", "list", "delete"]
  - apiGroups: ["kuadrant.io"]
    resources: ["authpolicies", "ratelimitpolicies", "dnspolicies", "tlspolicies"]
    verbs: ["create", "get", "list", "delete"]
  # OLM operator subscriptions (read-only, for cluster state)
  - apiGroups: ["operators.coreos.com"]
    resources: ["subscriptions"]
    verbs: ["get", "list"]
  # Self-subject access review (RBAC pre-checks)
  - apiGroups: ["authorization.k8s.io"]
    resources: ["selfsubjectaccessreviews"]
    verbs: ["create"]
  # Pod logs for job harvesting
  - apiGroups: [""]
    resources: ["pods/log"]
    verbs: ["get"]
  # MaaS CRDs (for live testing against deployed models-as-a-service)
  - apiGroups: ["maas.opendatahub.io"]
    resources: ["*"]
    verbs: ["create", "get", "list", "watch", "update", "patch", "delete"]
  # KServe CRDs
  - apiGroups: ["serving.kserve.io"]
    resources: ["*"]
    verbs: ["create", "get", "list", "watch", "update", "patch", "delete"]
EOF

# Bind ClusterRole to ServiceAccount
cat <<EOF | kubectl apply -f -
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: opinai-sandbox-manager
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: opinai-sandbox-manager
subjects:
  - kind: ServiceAccount
    name: $SA
    namespace: $NAMESPACE
EOF

# Create credentials secret (placeholder — user fills in)
kubectl -n "$NAMESPACE" get secret opinai-credentials &>/dev/null || \
  kubectl -n "$NAMESPACE" create secret generic opinai-credentials \
    --from-literal=GITHUB_TOKEN="" \
    --from-literal=AI_API_KEY="" \
    --from-literal=AI_PROVIDER="anthropic" \
    --from-literal=AI_MODEL="claude-sonnet-4-20250514"

echo ""
echo "Setup complete. Next steps:"
echo "  1. Edit the opinai-credentials secret with your GITHUB_TOKEN and AI_API_KEY"
echo "  2. Deploy the controller: kubectl -n $NAMESPACE apply -f deploy/"
echo "  3. Add repos via the admin dashboard or REPOS env var"
