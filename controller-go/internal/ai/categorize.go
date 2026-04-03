package ai

import (
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

// Categorize asks the AI to classify an issue as BUG/FEATURE/QUESTION/DOCS.
func Categorize(title, body string) string {
	cfg := LoadConfig()
	if !cfg.Available() {
		return "BUG"
	}

	prompt := prompts.Render("categorize.txt", map[string]string{
		"Title": title, "Body": body,
	})

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
