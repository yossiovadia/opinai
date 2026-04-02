package runner

import (
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/ai"
)

const maxRetries = 3

// RetryResult tracks what happened during self-healing retries.
type RetryResult struct {
	Succeeded   bool
	Retries     int
	FixesApplied []string
}

// runWithRetry executes a shell command with self-healing retries.
// Returns stdout+stderr output, the RetryResult, and any final error.
func runWithRetry(command, workDir string, env []string) (string, RetryResult, error) {
	result := RetryResult{}
	currentCmd := command

	for attempt := 0; attempt <= maxRetries; attempt++ {
		cmd := exec.Command("sh", "-c", currentCmd)
		cmd.Dir = workDir
		cmd.Env = env
		out, err := cmd.CombinedOutput()
		output := string(out)

		if err == nil {
			result.Succeeded = true
			result.Retries = attempt
			return output, result, nil
		}

		if attempt == maxRetries {
			return output, result, fmt.Errorf("command failed after %d retries: %s", maxRetries, err)
		}

		slog.Warn("command failed, attempting self-heal",
			"attempt", attempt+1, "cmd", truncStr(currentCmd, 100), "error", err)

		// Try common auto-fixes first (no AI needed)
		fix, fixDesc := tryCommonFix(output, currentCmd, env)
		if fix != "" {
			slog.Info("applying common fix", "fix", fixDesc)
			currentCmd = fix
			result.FixesApplied = append(result.FixesApplied, fixDesc)
			// Update env if the fix changed it
			env = buildEnv()
			continue
		}

		// Ask AI for diagnosis
		fixedCmd := askAIForFix(currentCmd, output)
		if fixedCmd != "" && fixedCmd != currentCmd {
			slog.Info("applying AI-suggested fix", "original", truncStr(currentCmd, 80), "fixed", truncStr(fixedCmd, 80))
			currentCmd = fixedCmd
			result.FixesApplied = append(result.FixesApplied, "AI fix: "+truncStr(fixedCmd, 100))
			continue
		}

		// No fix available
		return output, result, fmt.Errorf("command failed, no fix found: %s", err)
	}

	return "", result, fmt.Errorf("exhausted retries")
}

// tryCommonFix applies well-known fixes without needing AI.
func tryCommonFix(errorOutput, command string, env []string) (string, string) {
	lower := strings.ToLower(errorOutput)

	// Permission denied on pip → add --user flag
	if strings.Contains(lower, "permission denied") && strings.Contains(command, "pip install") {
		if !strings.Contains(command, "--user") {
			fixed := strings.ReplaceAll(command, "pip install", "pip install --user")
			return fixed, "added --user flag to pip install (permission denied)"
		}
	}

	// pip install without python3 -m prefix
	if strings.Contains(lower, "pip: command not found") || strings.Contains(lower, "pip3: command not found") {
		if !strings.Contains(command, "python3 -m pip") {
			fixed := strings.ReplaceAll(command, "pip install", "python3 -m pip install")
			fixed = strings.ReplaceAll(fixed, "pip3 install", "python3 -m pip install")
			return fixed, "replaced pip with python3 -m pip (command not found)"
		}
	}

	// Command not found after pip install → likely PATH issue
	if strings.Contains(lower, "command not found") && !strings.Contains(lower, "pip") {
		// Extract the missing command name
		for _, line := range strings.Split(errorOutput, "\n") {
			if strings.Contains(strings.ToLower(line), "command not found") {
				parts := strings.Fields(line)
				if len(parts) > 0 {
					missingCmd := strings.TrimSuffix(strings.TrimPrefix(parts[0], "'"), "'")
					missingCmd = strings.TrimSuffix(missingCmd, ":")
					if missingCmd != "" {
						// Try running with explicit PATH
						fixed := fmt.Sprintf("export PATH=/tmp/pip-user/bin:$PATH && %s", command)
						return fixed, fmt.Sprintf("prepended pip bin to PATH (%s not found)", missingCmd)
					}
				}
			}
		}
	}

	// No module named X → try pip install
	if strings.Contains(lower, "no module named") {
		for _, line := range strings.Split(errorOutput, "\n") {
			if strings.Contains(strings.ToLower(line), "no module named") {
				// Extract module name: "No module named 'foo'"
				parts := strings.Split(line, "'")
				if len(parts) >= 2 {
					module := parts[1]
					// Only try for simple module names
					if !strings.Contains(module, " ") && len(module) < 50 {
						fixed := fmt.Sprintf("python3 -m pip install --user %s && %s", module, command)
						return fixed, fmt.Sprintf("auto-installed missing module: %s", module)
					}
				}
			}
		}
	}

	// pkg-config / build deps missing
	if strings.Contains(lower, "error: subprocess-exited-with-error") && strings.Contains(lower, "pip") {
		if !strings.Contains(command, "--no-build-isolation") {
			fixed := command + " --no-build-isolation"
			return fixed, "added --no-build-isolation flag"
		}
	}

	return "", ""
}

// askAIForFix sends the error to the AI for diagnosis and gets a fixed command.
func askAIForFix(command, errorOutput string) string {
	cfg := ai.LoadConfig()
	if !cfg.Available() {
		return ""
	}

	// Truncate error output to avoid huge prompts
	errTrunc := errorOutput
	if len(errTrunc) > 1500 {
		errTrunc = errTrunc[len(errTrunc)-1500:]
	}

	prompt := fmt.Sprintf(
		"The reproduction setup failed.\n\n"+
			"Command: %s\n\n"+
			"Error output (last 1500 chars):\n%s\n\n"+
			"Diagnose the problem and provide a fixed command that will work in a minimal container "+
			"(Debian-based, no root access, limited PATH, pip may need --user flag, "+
			"PYTHONUSERBASE=/tmp/pip-user).\n\n"+
			"Respond with ONLY the fixed shell command on a single line. No explanation.",
		command, errTrunc,
	)

	reply, err := ai.Call(prompt, 512)
	if err != nil || reply == "" {
		return ""
	}

	// Extract first non-empty line that looks like a command
	for _, line := range strings.Split(strings.TrimSpace(reply), "\n") {
		line = strings.TrimSpace(line)
		// Skip markdown fences and empty lines
		if line == "" || strings.HasPrefix(line, "```") || strings.HasPrefix(line, "#") {
			continue
		}
		// Basic sanity: must contain at least one space or common command char
		if len(line) > 5 && len(line) < 2000 {
			return line
		}
	}
	return ""
}

// formatRetryInfo produces a human-readable summary of retries for the report.
func formatRetryInfo(rr RetryResult) string {
	if rr.Retries == 0 {
		return ""
	}
	fixes := strings.Join(rr.FixesApplied, "; ")
	return fmt.Sprintf("Setup required %d retries to resolve: %s", rr.Retries, fixes)
}

// setEnvVar updates or adds an env var in a slice.
func setEnvVar(env []string, key, value string) []string {
	prefix := key + "="
	for i, kv := range env {
		if strings.HasPrefix(kv, prefix) {
			env[i] = prefix + value
			return env
		}
	}
	return append(env, prefix+value)
}

func init() {
	// Ensure /tmp/pip-user/bin exists for auto-fix PATH additions
	os.MkdirAll("/tmp/pip-user/bin", 0o755)
}
