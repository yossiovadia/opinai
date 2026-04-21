package runner

import (
	"log/slog"
	"os"
	"os/exec"
	"strings"
)

func hostToolDeploy() string {
	if os.Getenv("OPINAI_HOST_TOOLS") != "true" {
		return ""
	}

	kindCluster := os.Getenv("OPINAI_KIND_CLUSTER")
	repo := os.Getenv("REPO")
	if repo == "" || kindCluster == "" {
		slog.Warn("host-tool deploy: missing REPO or OPINAI_KIND_CLUSTER")
		return ""
	}

	component := "all"
	var scriptPath string

	repoLower := strings.ToLower(repo)
	switch {
	case strings.Contains(repoLower, "models-as-a-service"):
		component = "maas-api"
		scriptPath = "/tmp/opinai-repo/test/e2e/scripts/local-deploy.sh"
	case strings.Contains(repoLower, "ai-gateway-payload-processing"):
		component = "bbr"
		slog.Info("host-tool: BBR repo — cloning MaaS for deploy script")
		cmd := exec.Command("git", "clone", "--depth=1",
			"https://github.com/opendatahub-io/models-as-a-service.git",
			"/tmp/maas-deploy")
		if out, err := cmd.CombinedOutput(); err != nil {
			slog.Warn("host-tool: failed to clone MaaS for deploy script", "error", err, "output", string(out))
			return ""
		}
		scriptPath = "/tmp/maas-deploy/test/e2e/scripts/local-deploy.sh"
	default:
		slog.Warn("host-tool: unknown repo for component mapping", "repo", repo)
		return ""
	}

	// Check if the deployment already exists
	checkCmd := exec.Command("kubectl", "get", "deploy", "maas-api", "-n", "maas-system")
	if err := checkCmd.Run(); err != nil {
		slog.Info("host-tool: maas-api deployment not found — skipping rebuild (needs full deploy first)")
		return "http://maas-default-gateway-istio.istio-system.svc.cluster.local"
	}

	// Verify script exists
	if _, err := os.Stat(scriptPath); err != nil {
		slog.Warn("host-tool: deploy script not found", "path", scriptPath, "error", err)
		return "http://maas-default-gateway-istio.istio-system.svc.cluster.local"
	}

	slog.Info("host-tool: rebuilding component", "component", component, "script", scriptPath)
	rebuildCmd := exec.Command("bash", scriptPath, "--rebuild", component)
	rebuildCmd.Env = os.Environ()
	rebuildCmd.Stdout = os.Stderr
	rebuildCmd.Stderr = os.Stderr
	if err := rebuildCmd.Run(); err != nil {
		slog.Warn("host-tool: rebuild failed", "component", component, "error", err)
		return ""
	}

	serverURL := "http://maas-default-gateway-istio.istio-system.svc.cluster.local"
	slog.Info("host-tool: rebuild complete", "component", component, "server_url", serverURL)
	return serverURL
}
