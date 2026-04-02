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
			"It does NOT have root access. It has NO GPU. It has 512Mi RAM.\n"+
			"PYTHONUSERBASE=/tmp/pip-user, HOME=/tmp/home.\n\n"+
			"Strategies to try:\n"+
			"1. Add --user --break-system-packages flags to pip\n"+
			"2. Use --no-deps to skip heavy dependencies (torch, tensorflow, cuda, vllm, triton, xformers), "+
			"then install only the lightweight deps needed for the server\n"+
			"3. If the error is OOM or timeout, the package may be too large — use --no-deps approach\n"+
			"4. If a dependency needs compilation, check if a pre-built wheel exists\n\n"+
			"Provide the exact fixed shell command that would work.\n"+
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
