package runner

import (
	"log/slog"
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

// askAIForFix sends a failed command + error to the AI and gets a fixed command back.
func askAIForFix(command, errorOutput string) string {
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		return ""
	}

	errTrunc := errorOutput
	if len(errTrunc) > 1500 {
		errTrunc = errTrunc[len(errTrunc)-1500:]
	}

	prompt := prompts.Render("selfheal_install.txt", map[string]string{
		"Command": command, "ErrorOutput": errTrunc,
	})

	reply, err := ai.Call(prompt, 512)
	if err != nil || reply == "" {
		slog.Warn("AI fix request failed", "error", err)
		return ""
	}

	for _, line := range strings.Split(strings.TrimSpace(reply), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "#") {
			continue
		}
		if len(line) > 5 && len(line) < 2000 {
			return line
		}
	}
	return ""
}
