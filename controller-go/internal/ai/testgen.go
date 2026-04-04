package ai

import (
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

// GenerateTests asks the AI to write a Python test script for an issue.
func GenerateTests(title, body, serverURL, profileContext, repoContext string) (string, error) {
	cfg := LoadConfig()
	if !cfg.Available() {
		return "", nil
	}

	var serverCtx string
	if serverURL != "" {
		serverCtx = "\n\nThe server is already running. Check health with:\n" +
			"  requests.get(\"" + serverURL + "/health\", timeout=5)\n\n" +
			"If the health check succeeds (HTTP 200), proceed directly to testing.\n" +
			"If the health check fails, you may need to start the server first.\n" +
			"Environment: PYTHONUSERBASE=/tmp/pip-user PATH=/tmp/pip-user/bin:$PATH\n\n" +
			"Always check health before installing. Never install if the server is already running.\n"
	}

	prompt := prompts.Render("generate_test.txt", map[string]string{
		"Title": title, "Body": body, "ServerURL": serverURL,
		"ServerContext": serverCtx, "ProfileContext": profileContext, "RepoContext": repoContext,
	})

	content, err := callWithConfig(cfg, prompt, 4096)
	if err != nil {
		return "", err
	}
	if content == "" {
		return "", nil
	}

	return stripCodeFences(content), nil
}

// CritiqueTest asks the AI to review a test script and either approve it or return a corrected version.
func CritiqueTest(script, title, body string) (string, error) {
	cfg := LoadConfig()
	if !cfg.Available() {
		return script, nil
	}

	prompt := prompts.Render("critique_test.txt", map[string]string{
		"Title": title, "Body": body, "Script": script,
	})

	content, err := callWithConfig(cfg, prompt, 4096)
	if err != nil {
		return script, err
	}
	content = strings.TrimSpace(content)
	if content == "" || strings.ToUpper(content) == "APPROVED" {
		return script, nil
	}
	return stripCodeFences(content), nil
}

// RegenerateTest asks the AI to fix a crashed test script given the error output.
func RegenerateTest(title, body, script, errorOutput string) (string, error) {
	cfg := LoadConfig()
	if !cfg.Available() {
		return "", nil
	}

	prompt := "Your previous Python test script crashed. Fix it.\n\n" +
		"Bug report:\nTitle: " + title + "\nBody: " + body + "\n\n" +
		"Original script:\n```python\n" + script + "\n```\n\n" +
		"Error output:\n```\n" + errorOutput + "\n```\n\n" +
		"Generate a fixed Python test script that avoids this error.\n" +
		"Each test must print a JSON line: {\"test\": \"name\", \"status\": \"pass|fail\", \"details\": \"...\"}\n" +
		"Handle errors gracefully — report them as test results, don't crash.\n" +
		"Output ONLY the Python script, no explanation."

	content, err := callWithConfig(cfg, prompt, 4096)
	if err != nil {
		return "", err
	}
	if content == "" {
		return "", nil
	}
	return stripCodeFences(content), nil
}

func stripCodeFences(content string) string {
	var cleaned []string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue
		}
		cleaned = append(cleaned, line)
	}
	return strings.Join(cleaned, "\n")
}
