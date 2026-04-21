# Task: Deploy OpinAI into kind-maas-local cluster

## Context

OpinAI currently runs in its own Kind cluster (`kind-opinai`) on a Windows/WSL2 machine.
MaaS+BBR run in a separate Kind cluster (`kind-maas-local`).
Runner pods in `kind-opinai` can't reach services in `kind-maas-local` (different Docker networks).

**Goal:** Deploy OpinAI into `kind-maas-local` so runner pods can directly access MaaS/BBR services
for issue reproduction, AND have the ability to rebuild MaaS/BBR components via `local-deploy.sh --rebuild`.

## What needs to happen

### Part 1: Code changes in OpinAI (controller-go)

#### 1a. New file: `internal/controller/host_tools.go`

Create a helper that:
- Defines a list of repos that need "host-tool" access (Docker socket for rebuilds):
  - `opendatahub-io/models-as-a-service`
  - `opendatahub-io/ai-gateway-payload-processing`
- `repoNeedsHostTools(repo string) bool` — checks if repo is in the list
- `hostToolVolumes(needsHostTools bool) []corev1.Volume` — returns base volumes (gcp-credentials secret) PLUS Docker socket HostPath volume when needed
- `hostToolVolumeMounts(needsHostTools bool) []corev1.VolumeMount` — returns base mounts PLUS Docker socket mount when needed
- `hostToolEnvVars(needsHostTools bool) []corev1.EnvVar` — returns env vars like:
  - `OPINAI_HOST_TOOLS=true`
  - `OPINAI_KIND_CLUSTER=maas-local` (used for `kind load docker-image --name`)
  - `OPINAI_LOCAL_DEPLOY_SCRIPT=test/e2e/scripts/local-deploy.sh`

#### 1b. Modify `internal/controller/jobs.go`

In `createJob()`:
- Call `repoNeedsHostTools(repo)` early
- Replace the inline `Volumes: []corev1.Volume{...gcp-credentials...}` with `hostToolVolumes(needsHostTools)`
- Replace the inline `VolumeMounts: []corev1.VolumeMount{...gcp-credentials...}` with `hostToolVolumeMounts(needsHostTools)`
- Append `hostToolEnvVars(needsHostTools)...` to the env slice

**IMPORTANT:** There are TWO job creation functions in jobs.go — `createJob` (issue reproduction, ~line 204)
and `CreatePRReviewJob` (~line 560). Apply host-tool changes to BOTH — PR reviews also benefit from
being able to test against the live stack.

#### 1c. New file: `internal/runner/host_tools.go`

Create `hostToolDeploy() string` that:
1. Reads `OPINAI_HOST_TOOLS`, `OPINAI_KIND_CLUSTER`, `OPINAI_LOCAL_DEPLOY_SCRIPT`, `REPO` env vars
2. The repo is already cloned to `/tmp/opinai-repo` by the runner
3. Determines which component to rebuild based on repo name:
   - `models-as-a-service` → `maas-api` (or `all` if unsure)
   - `ai-gateway-payload-processing` → `bbr`
4. For BBR: the `local-deploy.sh` lives in the MaaS repo, not BBR. Clone MaaS separately:
   `git clone --depth=1 https://github.com/opendatahub-io/models-as-a-service.git /tmp/maas-deploy`
   Then use `/tmp/maas-deploy/test/e2e/scripts/local-deploy.sh`
5. For MaaS: the script is in the cloned repo itself at `/tmp/opinai-repo/test/e2e/scripts/local-deploy.sh`
6. Checks if the deployment already exists by running:
   `kubectl get deploy maas-api -n maas-system` — if it exists, just do `--rebuild <component>`
   If it doesn't exist, skip the rebuild (assume the cluster needs a full deploy first, which is out of scope)
7. Runs `bash <script> --rebuild <component>` with env: `KUBECONFIG` set if needed
8. After rebuild, determines the server URL. Since we're in the same cluster, use cluster DNS:
   `http://maas-default-gateway-istio.istio-system.svc.cluster.local`
   (port 80, no port-forward needed)
9. Returns the server URL or empty string on failure

#### 1d. Modify `internal/runner/runner.go`

In the deployment section (around line 99, the `if !skipDeployment {` block):
- Add host-tool mode check at the top:
  ```go
  hostTools := os.Getenv("OPINAI_HOST_TOOLS") == "true"
  if hostTools && serverURL == "" {
      slog.Info("host-tool mode — using live deployment in cluster")
      // Check if MaaS is already deployed
      serverURL = hostToolDeploy()
      if serverURL != "" {
          os.Setenv("SERVER_URL", serverURL)
      } else {
          // Even without rebuild, try using existing deployment
          serverURL = "http://maas-default-gateway-istio.istio-system.svc.cluster.local"
          os.Setenv("SERVER_URL", serverURL)
          slog.Info("using existing MaaS deployment", "server_url", serverURL)
      }
  }
  ```

#### 1e. Update `Dockerfile.python`

Add Docker CLI and Kind CLI to the runner image (after helm install):
```dockerfile
# docker CLI (for host-tool mode — rebuilds into local Kind clusters)
RUN curl -fsSL https://download.docker.com/linux/static/stable/$(uname -m)/docker-27.5.1.tgz \
    | tar xz --strip-components=1 -C /usr/local/bin docker/docker
# kind CLI (for loading images into Kind clusters)
RUN ARCH=$(dpkg --print-architecture) && \
    curl -Lo /usr/local/bin/kind "https://kind.sigs.k8s.io/dl/v0.27.0/kind-linux-${ARCH}" && \
    chmod +x /usr/local/bin/kind
```

#### 1f. Update prompts (optional but valuable)

In `internal/prompts/agent_investigate.txt`, before the verdict format section, add:
```
{{if .HostTools}}
HOST-TOOL MODE:
This project is deployed in the same Kubernetes cluster. You have direct access to the services.
- MaaS API: http://maas-api.maas-system.svc.cluster.local:8000
- MaaS Gateway: http://maas-default-gateway-istio.istio-system.svc.cluster.local
- BBR (payload-processing): deployed as ext_proc filter behind the gateway
- You can inspect pods: kubectl get pods -n maas-system && kubectl get pods -n istio-system
- You can check logs: kubectl logs -n <namespace> deploy/<name>
- The project was rebuilt with latest code before this session started.
{{end}}
```

Update `internal/agent/agent.go` to pass `HostTools`, `KindContext` template vars (add `"os"` import).

### Part 2: Deployment script

#### 2a. Create `scripts/deploy-to-maas-cluster.sh`

A script that:
1. Backs up existing secrets from `kind-opinai` (opinai-credentials, opinai-gcp-credentials, opinai-config):
   ```bash
   kubectl --context kind-opinai get secret opinai-credentials -n opinai -o yaml > /tmp/opinai-creds-backup.yaml
   kubectl --context kind-opinai get secret opinai-gcp-credentials -n opinai -o yaml > /tmp/opinai-gcp-backup.yaml
   kubectl --context kind-opinai get configmap opinai-config -n opinai -o yaml > /tmp/opinai-config-backup.yaml
   ```
2. Builds OpinAI images: `docker build -t opinai-controller:latest .` and `docker build -f Dockerfile.python -t opinai-runner-python:latest .`
3. Loads images into `kind-maas-local`: `kind load docker-image ... --name maas-local`
4. Creates `opinai` namespace in `kind-maas-local`
5. Runs `setup.sh` against `kind-maas-local` context (creates RBAC)
6. Applies backed-up secrets (strip resourceVersion/uid/creationTimestamp first)
7. Applies deployment manifests from `controller/manifests/` (deployment.yaml, service.yaml)
8. Waits for rollout

**The script should NOT delete `kind-opinai`** — leave that to the user as a separate step.

### Part 3: Kind cluster config change

The `local-deploy.sh` in MaaS needs Docker socket mounted into the Kind node for runner pods
to be able to do `docker build` and `kind load`.

**DO NOT modify local-deploy.sh.** Instead, document that when the cluster is recreated
(after a `--teardown`), it should be created with this config:

Create a file `scripts/kind-maas-config.yaml`:
```yaml
# Kind cluster config for maas-local with Docker socket access.
# Use with: kind create cluster --name maas-local --config kind-maas-config.yaml
# Then run local-deploy.sh which skips cluster creation if it already exists.
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
- role: control-plane
  extraPortMappings:
  - containerPort: 80
    hostPort: 8080
    protocol: TCP
  - containerPort: 443
    hostPort: 8443
    protocol: TCP
  extraMounts:
  - hostPath: /var/run/docker.sock
    containerPath: /var/run/docker.sock
```

Usage: tear down existing cluster, recreate with this config, then run local-deploy.sh:
```bash
kind delete cluster --name maas-local
kind create cluster --name maas-local --config scripts/kind-maas-config.yaml --wait 120s
cd /path/to/models-as-a-service && ./test/e2e/scripts/local-deploy.sh
cd /path/to/opinai/controller-go && ./scripts/deploy-to-maas-cluster.sh
```

## Important constraints

- All code is in Go. The project uses Go modules. Run `go build ./...` to verify compilation.
- Do NOT modify anything in the `models-as-a-service` repo (local-deploy.sh stays untouched).
- The existing `kind-opinai` cluster and generic OpinAI must continue to work unchanged.
- Runner pods use image `opinai-runner-python:latest` (Dockerfile.python).
- Controller uses image `opinai-controller:latest` (Dockerfile).
- Test everything compiles: `cd controller-go && go build ./...`
- Commit all changes to the opinai repo with a clear commit message. Do NOT push.

## File locations

- OpinAI repo: `/Users/mooki/code/opinai/controller-go/`
- MaaS repo: `/Users/mooki/code/models-as-a-service/` (READ ONLY — do not modify)
- Go module: `github.com/yossiovadia/opinai/controller-go`

## Verification

After implementation:
1. `cd /Users/mooki/code/opinai/controller-go && go build ./...` must pass
2. `git diff --stat` should show the new/changed files
3. No changes to `models-as-a-service/`
