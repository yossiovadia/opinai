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
  "install_command": "same as build_command (for backward compatibility)",
  "runtime_requirements": {
    "language": "primary language (python, go, javascript, rust, etc)",
    "needs_glibc": true/false (true if the project has Python C extensions like numpy, scipy, tokenizers, torch, tensorflow, pillow, or uses native compiled libraries that need glibc wheels — Alpine/musl will fail or be very slow for these),
    "needs_gpu": true/false (true if training ML models or running inference that requires CUDA/GPU),
    "min_cpu_cores": 1 (minimum CPU cores needed for reproduction),
    "min_ram_mb": 512 (minimum RAM in MB — increase for ML/data projects),
    "min_gpu_vram_mb": 0 (minimum GPU VRAM in MB, 0 if no GPU needed),
    "gpu_count": 0 (number of GPUs needed, 0 if no GPU),
    "heavy_deps": ["list", "of", "heavy", "native", "dependencies"] (packages with C extensions or large compile times on musl — numpy, pandas, scipy, tokenizers, transformers, torch, tensorflow, pillow, cryptography, lxml, etc),
    "preferred_base": "recommended Docker base image (python:3.12-slim for Python with C deps, alpine for Go, node:20-slim for Node with native deps, or empty string for default)",
    "infra_deps": ["list of infrastructure dependencies like postgresql, redis, rabbitmq, elasticsearch"],
    "deploy_mode": "pip-install | go-build | npm-install | docker-compose | helm | kustomize | code-review"
  }
}

IMPORTANT: Determine everything from the actual files found. If needs_cluster is true and the project cannot run locally, set run_command to 'none'. The runner will then analyze the source code instead of trying to run a server.

For runtime_requirements.needs_glibc: check the dependency files carefully. Python projects with numpy, scipy, pandas, torch, tensorflow, tokenizers, transformers, pillow, cryptography, or similar packages with C extensions MUST have needs_glibc=true. These packages distribute pre-built wheels for glibc-based systems (Debian/Ubuntu) but NOT for musl (Alpine), causing extremely slow compilation or outright failure on Alpine.