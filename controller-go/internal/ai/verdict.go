package ai

import (
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/prompts"
)

// VerdictResult holds the AI's verdict on test results.
type VerdictResult struct {
	Text       string // full AI response text
	Verdict    string // BUG_CONFIRMED, NOT_A_BUG, NOT_REPRODUCIBLE, BUG_FIXED, BUG_REGRESSION
	Confidence string // HIGH, MEDIUM, LOW
}

// All valid verdict strings.
var AllVerdicts = []string{
	"BUG_CONFIRMED", "NOT_A_BUG", "NOT_REPRODUCIBLE",
	"FEATURE_REQUEST", "ERROR", "INCONCLUSIVE", "BUG_FIXED", "BUG_REGRESSION",
}

// GetVerdict asks the AI to summarize test results with verdict and confidence.
// issueState should be "open" or "closed".
func GetVerdict(title, body, testOutput, issueState string) VerdictResult {
	cfg := LoadConfig()
	if !cfg.Available() {
		return VerdictResult{Verdict: "ERROR", Confidence: "LOW"}
	}

	var verdictOptions string
	if issueState == "closed" {
		verdictOptions = "Use EXACTLY one of these verdicts:\n" +
			"- BUG_FIXED — the issue was closed and the fix appears correct (tests pass)\n" +
			"- BUG_REGRESSION — the issue was closed but the bug STILL exists (tests fail)\n" +
			"- NOT_REPRODUCIBLE — tests ran but could not trigger the bug either way\n" +
			"- INCONCLUSIVE — test infrastructure failed (script crashed, server unreachable, etc.)\n"
	} else {
		verdictOptions = "Use EXACTLY one of these verdicts:\n" +
			"- BUG_CONFIRMED — tests prove the bug exists\n" +
			"- NOT_A_BUG — tests prove behavior is correct\n" +
			"- NOT_REPRODUCIBLE — tests ran but could not trigger the bug\n" +
			"- INCONCLUSIVE — test infrastructure failed (script crashed, server unreachable, etc.)\n"
	}

	var stateCtx string
	if issueState == "closed" {
		stateCtx = "\nThis issue is CLOSED (presumably fixed). Analyze whether the fix is correct.\n"
	} else {
		stateCtx = "\nThis is an OPEN issue.\n"
	}

	prompt := prompts.Render("verdict.txt", map[string]string{
		"Title": title, "Body": body,
		"StateContext": stateCtx, "TestOutput": testOutput, "VerdictOptions": verdictOptions,
	})

	content, err := callWithConfig(cfg, prompt, 2048)
	if err != nil {
		return VerdictResult{Verdict: "ERROR", Confidence: "LOW"}
	}
	if content == "" {
		return VerdictResult{Verdict: "ERROR", Confidence: "LOW"}
	}

	result := VerdictResult{Text: content, Verdict: "NOT_REPRODUCIBLE", Confidence: "MEDIUM"}

	upper := strings.ToUpper(content)
	for _, line := range strings.Split(upper, "\n") {
		if strings.Contains(line, "VERDICT:") {
			for _, v := range AllVerdicts {
				if strings.Contains(line, v) {
					result.Verdict = v
					break
				}
			}
		}
		if strings.Contains(line, "CONFIDENCE:") {
			for _, c := range []string{"HIGH", "LOW", "MEDIUM"} {
				if strings.Contains(line, c) {
					result.Confidence = c
					break
				}
			}
		}
	}

	// Fallback: scan for keywords
	if result.Verdict == "NOT_REPRODUCIBLE" {
		if strings.Contains(upper, "INCONCLUSIVE") {
			result.Verdict = "INCONCLUSIVE"
		} else if strings.Contains(upper, "BUG_REGRESSION") || strings.Contains(upper, "BUG REGRESSION") {
			result.Verdict = "BUG_REGRESSION"
		} else if strings.Contains(upper, "BUG_FIXED") || strings.Contains(upper, "BUG FIXED") {
			result.Verdict = "BUG_FIXED"
		} else if strings.Contains(upper, "BUG_CONFIRMED") || strings.Contains(upper, "BUG CONFIRMED") {
			result.Verdict = "BUG_CONFIRMED"
		} else if strings.Contains(upper, "NOT_A_BUG") || strings.Contains(upper, "NOT A BUG") {
			result.Verdict = "NOT_A_BUG"
		}
	}

	return result
}
