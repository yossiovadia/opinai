package runner

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

const hostToolServerURL = "http://maas-default-gateway-istio.istio-system.svc.cluster.local"

type rebuildTarget struct {
	component  string
	image      string
	buildDir   string
	dockerfile string
	deployment string
	namespace  string
}

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

	var target rebuildTarget
	repoLower := strings.ToLower(repo)
	switch {
	case strings.Contains(repoLower, "models-as-a-service"):
		target = rebuildTarget{
			component:  "maas-api",
			image:      "quay.io/opendatahub/maas-api:latest",
			buildDir:   "/tmp/opinai-repo/maas-api",
			dockerfile: "Dockerfile",
			deployment: "maas-api",
			namespace:  "maas-system",
		}
	case strings.Contains(repoLower, "ai-gateway-payload-processing"):
		target = rebuildTarget{
			component:  "bbr",
			image:      "quay.io/opendatahub/odh-ai-gateway-payload-processing:odh-stable",
			buildDir:   "/tmp/opinai-repo",
			dockerfile: "Dockerfile",
			deployment: "payload-processing",
			namespace:  "istio-system",
		}
	default:
		slog.Warn("host-tool: unknown repo for component mapping", "repo", repo)
		return ""
	}

	// Check if the deployment already exists
	checkCmd := exec.Command("kubectl", "get", "deploy", target.deployment, "-n", target.namespace)
	if err := checkCmd.Run(); err != nil {
		slog.Info("host-tool: deployment not found — skipping rebuild (needs full deploy first)",
			"deployment", target.deployment)
		return hostToolServerURL
	}

	// Verify build directory exists
	if _, err := os.Stat(target.buildDir); err != nil {
		slog.Warn("host-tool: build directory not found", "dir", target.buildDir)
		return hostToolServerURL
	}

	slog.Info("host-tool: rebuilding component", "component", target.component, "image", target.image)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Step 1: docker build
	platform := "linux/" + runtime.GOARCH
	buildCmd := exec.CommandContext(ctx, "docker", "build",
		"--build-arg", "BUILDPLATFORM="+platform,
		"--build-arg", "TARGETPLATFORM="+platform,
		"-t", target.image,
		"-f", target.dockerfile,
		".")
	buildCmd.Dir = target.buildDir
	buildCmd.Env = os.Environ()
	buildOut, err := buildCmd.CombinedOutput()
	if err != nil {
		slog.Warn("host-tool: docker build failed", "component", target.component,
			"error", err, "output", truncBuildOutput(string(buildOut)))
		return hostToolServerURL
	}
	slog.Info("host-tool: docker build succeeded", "component", target.component)

	// Step 2: kind load docker-image into the cluster
	loadCmd := exec.CommandContext(ctx, "kind", "load", "docker-image", target.image,
		"--name", kindCluster)
	loadCmd.Env = os.Environ()
	loadOut, err := loadCmd.CombinedOutput()
	if err != nil {
		slog.Warn("host-tool: kind load failed", "component", target.component,
			"error", err, "output", truncBuildOutput(string(loadOut)))
		return hostToolServerURL
	}
	slog.Info("host-tool: kind load succeeded", "component", target.component)

	// Step 3: kubectl rollout restart
	restartCmd := exec.CommandContext(ctx, "kubectl", "rollout", "restart",
		fmt.Sprintf("deployment/%s", target.deployment), "-n", target.namespace)
	if out, err := restartCmd.CombinedOutput(); err != nil {
		slog.Warn("host-tool: rollout restart failed", "component", target.component,
			"error", err, "output", string(out))
		return hostToolServerURL
	}

	// Step 4: wait for rollout
	waitCmd := exec.CommandContext(ctx, "kubectl", "rollout", "status",
		fmt.Sprintf("deployment/%s", target.deployment), "-n", target.namespace,
		"--timeout=120s")
	if out, err := waitCmd.CombinedOutput(); err != nil {
		slog.Warn("host-tool: rollout status wait failed", "component", target.component,
			"error", err, "output", string(out))
		return hostToolServerURL
	}

	slog.Info("host-tool: rebuild complete", "component", target.component, "server_url", hostToolServerURL)
	return hostToolServerURL
}

func truncBuildOutput(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 15 {
		lines = lines[len(lines)-15:]
	}
	return strings.Join(lines, "\n")
}
