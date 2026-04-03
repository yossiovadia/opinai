package ai

import (
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

// GenerateTests asks the AI to write a bash test script for an issue.
func GenerateTests(title, body, serverURL, profileContext, repoContext string) (string, error) {
	cfg := LoadConfig()
	if !cfg.Available() {
		return "", nil
	}

	var serverCtx string
	if serverURL != "" {
		serverCtx = "\n\nBefore running any tests, check if the server is already running by calling:\n" +
			"  curl -s " + serverURL + "/health\n\n" +
			"If the health check succeeds (HTTP 200), the server is ready — proceed directly to testing with curl.\n\n" +
			"If the health check fails, you may need to start the server first.\n" +
			"Environment: PYTHONUSERBASE=/tmp/pip-user PATH=/tmp/pip-user/bin:$PATH\n\n" +
			"Always check health before installing. Never install if the server is already running.\n"
	}

	prompt := prompts.Render("generate_test.txt", map[string]string{
		"Title": title, "Body": body,
		"ServerContext": serverCtx, "ProfileContext": profileContext, "RepoContext": repoContext,
	})

	content, err := callWithConfig(cfg, prompt, 4096)
	if err != nil {
		return "", err
	}
	if content == "" {
		return "", nil
	}

	var cleaned []string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue
		}
		cleaned = append(cleaned, line)
	}
	return strings.Join(cleaned, "\n"), nil
}
