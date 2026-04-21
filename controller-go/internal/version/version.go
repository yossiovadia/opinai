// Package version holds build-time version info set via ldflags.
//
// Build with:
//
//	go build -ldflags="-X github.com/yossiovadia/opinai/controller-go/internal/version.GitCommit=$(git rev-parse --short HEAD) -X github.com/yossiovadia/opinai/controller-go/internal/version.BuildTime=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
package version

import "os"

// Set via -ldflags at build time. Empty if not provided.
var (
	GitCommit string
	BuildTime string
)

// Info returns version metadata for display on the admin dashboard.
func Info() map[string]string {
	info := map[string]string{
		"git_commit":       valueOrDefault(GitCommit, "(dev)"),
		"build_time":       valueOrDefault(BuildTime, "(dev)"),
		"runner_image":     os.Getenv("OPINAI_IMAGE"),
		"controller_image": os.Getenv("HOSTNAME"), // in K8s, HOSTNAME is the pod name which contains the image SHA in the pod spec
	}

	// Try to get the actual image from the downward API or env
	if img := os.Getenv("OPINAI_CONTROLLER_IMAGE"); img != "" {
		info["controller_image"] = img
	}

	return info
}

func valueOrDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}
