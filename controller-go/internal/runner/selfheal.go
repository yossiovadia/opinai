package runner

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
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

	prompt := fmt.Sprintf(
		"This install/start command failed in an OpenShift container:\n\n"+
			"Command: %s\n\n"+
			"Error output (last 1500 chars):\n%s\n\n"+
			"The container has: python3, pip3, git, curl, make, bash.\n"+
			"It does NOT have root access.\n\n"+
			"Provide the exact fixed shell command that would work. "+
			"Consider: --user flag for pip, python3 -m pip, PYTHONUSERBASE, PATH adjustments.\n\n"+
			"Respond with ONLY the fixed shell command on a single line. No explanation.",
		command, errTrunc,
	)

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
