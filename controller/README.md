# OpinAI Controller

Kubernetes-native controller that watches GitHub repos for new issues and orchestrates bug reproduction Jobs.

## How it works

1. **Controller** (`opinai_controller.py`) runs as a Deployment, polling GitHub for open issues
2. For each new issue, it creates a Kubernetes **Job**
3. **Runner** (`opinai_runner.py`) runs inside the Job pod — fetches the issue, calls AI to generate tests, executes them, and posts a structured report back to the issue
4. Issues are labeled `opinai-done` after processing

No CRDs, no operator framework — just a Deployment that polls and creates Jobs.

## Quick start

```bash
# Interactive setup — generates secret.yaml and configmap.yaml
./setup.sh

# Build the image
docker build -t opinai-controller:latest .

# Deploy
kubectl apply -f manifests/namespace.yaml
kubectl apply -f manifests/
```

## Configuration

### Environment variables (via ConfigMap)

| Variable | Description | Default |
|----------|-------------|---------|
| `REPOS` | Comma-separated list of repos to watch | (required) |
| `POLL_INTERVAL_MINUTES` | How often to check for new issues | `60` |
| `DONE_LABEL` | Label applied to processed issues | `opinai-done` |

### Credentials (via Secret)

| Variable | Description |
|----------|-------------|
| `GITHUB_TOKEN` | GitHub token with repo + issues permissions |
| `AI_API_KEY` | API key for AI analysis (Anthropic, OpenAI, or compatible) |
| `AI_BASE_URL` | API base URL |
| `AI_MODEL` | Model to use for analysis |

## Files

```
controller/
├── Dockerfile                  # Build image
├── requirements.txt            # Python dependencies
├── opinai_controller.py        # Main controller loop
├── opinai_runner.py            # Runs inside Job pods
├── setup.sh                    # Interactive setup
├── manifests/
│   ├── namespace.yaml
│   ├── configmap.yaml
│   ├── secret.yaml.example     # Template — NOT real credentials
│   ├── serviceaccount.yaml
│   ├── role.yaml
│   ├── rolebinding.yaml
│   ├── deployment.yaml
│   └── job-template.yaml       # Reference template
└── README.md
```

## Security

- `manifests/secret.yaml` is gitignored — never commit credentials
- API keys are never logged or echoed
- AI output is sanitized before posting to GitHub (credential strings are replaced with REDACTED)
- `setup.sh` reads secrets with `-s` flag (no terminal echo)
