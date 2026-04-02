package ai

import (
	"strings"
)

// GenerateTests asks the AI to write a bash test script for an issue.
func GenerateTests(title, body, serverURL, profileContext, repoContext string) (string, error) {
	cfg := LoadConfig()
	if !cfg.Available() {
		return "", nil
	}

	var serverCtx string
	if serverURL != "" {
		serverCtx = "\n\nCRITICAL: The server is ALREADY running and healthy at " + serverURL + ". " +
			"Do NOT install anything. Do NOT start any server. Do NOT use pip. Do NOT use apt-get. " +
			"Just use curl to test the running server. The following environment is pre-configured: " +
			"SERVER_URL=" + serverURL + ". The server is already responding to health checks.\n"
	}

	prompt := "You are OpinAI, an automated bug reproduction system. " +
		"A user filed this bug report:\n\n" +
		"Title: " + title + "\n" +
		"Body: " + body + "\n" +
		serverCtx + profileContext + repoContext + "\n\n" +
		"Your task:\n" +
		"1. Analyze what the bug claims\n" +
		"2. Write a bash test script that ONLY contains curl commands and result checking\n" +
		"3. The script should use curl to test the server at " + serverURL + " and capture results\n" +
		"4. Print each test result as a JSON line: " +
		`{"test": "name", "status": "pass|fail", "details": "..."}` + "\n\n" +
		"IMPORTANT: Do NOT include any pip install, apt-get, git clone, or server startup commands. " +
		"The server is already running. Just test it.\n\n" +
		"Output ONLY the bash script, no explanation."

	content, err := callWithConfig(cfg, prompt, 4096)
	if err != nil {
		return "", err
	}
	if content == "" {
		return "", nil
	}

	// Strip markdown code fences
	var cleaned []string
	for _, line := range strings.Split(content, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			continue
		}
		cleaned = append(cleaned, line)
	}
	return strings.Join(cleaned, "\n"), nil
}
