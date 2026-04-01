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
		serverCtx = "\nThe server is already running at " + serverURL + ". Do NOT start the server yourself — just test it with curl."
	}

	prompt := "You are OpinAI, an automated bug reproduction system. " +
		"A user filed this bug report:\n\n" +
		"Title: " + title + "\n" +
		"Body: " + body + "\n" +
		serverCtx + profileContext + repoContext + "\n\n" +
		"Your task:\n" +
		"1. Analyze what the bug claims\n" +
		"2. Write a bash test script that would prove or disprove this bug\n" +
		"3. The script should use curl to test endpoints and capture results\n" +
		"4. Print each test result as a JSON line: " +
		`{"test": "name", "status": "pass|fail", "details": "..."}` + "\n\n" +
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
