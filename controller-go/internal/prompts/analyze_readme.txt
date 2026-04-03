Analyze this project from {{.Repo}}.

README:
{{.Readme}}

Dependency files:
{{.DepsInfo}}

Deployment/infrastructure files found: {{.Indicators}}

Deployment file contents:
{{.DeployContents}}

Provide a JSON summary (no markdown fences, just raw JSON):
{
  "description": "what this project does in 1-2 sentences",
  "tech_stack": "languages and frameworks",
  "deployment_type": "the type of project based on files found (e.g. kustomize, helm, docker, pip, go, npm, operator, etc)",
  "needs_cluster": "true if this requires Kubernetes/OpenShift to run, false if it can run as a local process",
  "test_strategy": "deploy-and-curl | start-and-curl | code-review | unit-test",
  "how_to_test": "specific steps to test bugs in this project",
  "deployment_needs": "infrastructure needed (CRDs, operators, databases, etc)",
  "build_command": "the exact shell command to build/install this project for testing. Examples: 'pip install --user --no-deps myapp && pip install --user fastapi uvicorn' or 'go build -o /tmp/server ./cmd/server' or 'make build' or '' (empty if no build needed). Must work in a rootless container with 512Mi RAM, no GPU. Use --user --break-system-packages for pip.",
  "run_command": "the exact shell command to start the server for API testing. Examples: '/tmp/server --port 8000' or 'llm-katan --backend echo --port 8000' or 'none' if no server is needed (K8s operators, libraries, CLI tools — use code-review strategy instead).",
  "install_command": "same as build_command (for backward compatibility)"
}

IMPORTANT: Determine everything from the actual files found. If needs_cluster is true and the project cannot run locally, set run_command to 'none'. The runner will then analyze the source code instead of trying to run a server.