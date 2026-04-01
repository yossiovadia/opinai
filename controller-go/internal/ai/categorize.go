package ai

import (
	"strings"
)

// Categorize asks the AI to classify an issue as BUG/FEATURE/QUESTION/DOCS.
func Categorize(title, body string) string {
	cfg := LoadConfig()
	if !cfg.Available() {
		return "BUG"
	}

	prompt := "You are OpinAI. Categorize this GitHub issue:\n\n" +
		"Title: " + title + "\n" +
		"Body: " + body + "\n\n" +
		"Categorize this issue: BUG (defect in existing behavior), " +
		"FEATURE (request for new functionality), QUESTION (asking for help/clarification), " +
		"or DOCS (documentation issue).\n\n" +
		"Respond with ONLY one line in this exact format:\n" +
		"Category: BUG\n" +
		"(or FEATURE, QUESTION, DOCS)"

	content, err := callWithConfig(cfg, prompt, 256)
	if err != nil {
		return "BUG"
	}

	upper := strings.ToUpper(content)
	for _, line := range strings.Split(upper, "\n") {
		if strings.Contains(line, "CATEGORY:") {
			for _, cat := range []string{"BUG", "FEATURE", "QUESTION", "DOCS"} {
				if strings.Contains(line, cat) {
					return cat
				}
			}
		}
	}
	// Fallback: scan for keywords
	for _, cat := range []string{"FEATURE", "QUESTION", "DOCS"} {
		if strings.Contains(upper, cat) {
			return cat
		}
	}
	return "BUG"
}
