package controller

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"

	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

// CheckOutcomes checks recently closed issues and merged/closed PRs for repos
// that OpinAI has investigated or reviewed. Records outcomes passively — never
// modifies OpinAI's verdicts or memory.
func CheckOutcomes(repos []string) {
	for _, repo := range repos {
		checkIssueOutcomes(repo)
		checkPROutcomes(repo)
	}
}

// checkIssueOutcomes looks at issues OpinAI investigated and checks if they've
// been closed, and whether the closure indicates the verdict was correct.
func checkIssueOutcomes(repo string) {
	// Get runs with verdicts that don't have outcomes yet
	runs, err := database.GetRuns(repo, 100)
	if err != nil {
		return
	}

	for _, run := range runs {
		if run.Verdict == "" {
			continue
		}
		// Skip if we already have an outcome for this issue
		has, _ := database.HasOutcome(repo, "issue", run.Issue)
		if has {
			continue
		}

		// Check the issue's current state on GitHub
		issue, err := FetchIssueDetails(repo, run.Issue)
		if err != nil {
			continue
		}

		// Only record outcomes for closed issues
		if issue.State != "closed" {
			continue
		}

		outcome := determineIssueOutcome(repo, run, issue)
		if outcome.ActualOutcome == "" {
			continue
		}

		if _, err := database.AddOutcome(outcome); err != nil {
			slog.Error("failed to record issue outcome", "repo", repo, "issue", run.Issue, "error", err)
		} else {
			slog.Info("recorded issue outcome", "repo", repo, "issue", run.Issue,
				"verdict", run.Verdict, "outcome", outcome.ActualOutcome, "correct", outcome.Correct)
		}
	}
}

// determineIssueOutcome evaluates a closed issue against OpinAI's verdict.
func determineIssueOutcome(repo string, run database.Run, issue *Issue) database.Outcome {
	outcome := database.Outcome{
		Repo:            repo,
		Type:            "issue",
		ReferenceNumber: run.Issue,
		OpinaiVerdict:   run.Verdict,
	}

	// Check labels for signals
	hasWontfix := false
	for _, l := range issue.Labels {
		lower := strings.ToLower(l.Name)
		if lower == "wontfix" || lower == "won't fix" || lower == "invalid" || lower == "not a bug" {
			hasWontfix = true
		}
	}

	// Check if there's a linked fix PR (look for "fix" or "close" references)
	hasFixPR := checkForFixPR(repo, run.Issue)

	switch run.Verdict {
	case "BUG_CONFIRMED":
		if hasFixPR {
			outcome.ActualOutcome = "issue_closed_with_fix"
			outcome.OutcomeDetails = "Fix PR found for this issue"
			correct := true
			outcome.Correct = &correct
		} else if hasWontfix {
			outcome.ActualOutcome = "issue_closed_wontfix"
			outcome.OutcomeDetails = "Issue closed as won't fix or invalid"
			correct := false
			outcome.Correct = &correct
		} else {
			outcome.ActualOutcome = "issue_closed"
			outcome.OutcomeDetails = "Issue closed without clear signal"
			// Ambiguous — don't set correct
		}

	case "NOT_REPRODUCIBLE", "NOT_A_BUG":
		if hasFixPR {
			// Bug was real despite our verdict — we were wrong
			outcome.ActualOutcome = "issue_closed_with_fix"
			outcome.OutcomeDetails = "Fix PR found — bug was real despite NOT_REPRODUCIBLE verdict"
			correct := false
			outcome.Correct = &correct
		} else if hasWontfix {
			outcome.ActualOutcome = "issue_closed_wontfix"
			outcome.OutcomeDetails = "Issue closed as won't fix — consistent with verdict"
			correct := true
			outcome.Correct = &correct
		} else {
			outcome.ActualOutcome = "issue_closed"
			// Ambiguous
		}

	case "FEATURE_REQUEST":
		outcome.ActualOutcome = "issue_closed"
		outcome.OutcomeDetails = "Feature request issue closed"
		// Not meaningful for correctness

	default:
		outcome.ActualOutcome = "issue_closed"
	}

	return outcome
}

// checkForFixPR checks if there are any PRs that reference this issue number.
// Uses the GitHub search API to find PRs mentioning "fixes #N" or "closes #N".
func checkForFixPR(repo string, issueNumber int) bool {
	// Use the timeline API to check for cross-references
	body, code, err := ghGet(fmt.Sprintf("/repos/%s/issues/%d/timeline", repo, issueNumber))
	if err != nil || code != 200 {
		return false
	}

	var events []struct {
		Event  string `json:"event"`
		Source *struct {
			Type  string `json:"type"`
			Issue *struct {
				PullRequest *struct{} `json:"pull_request"`
				State       string    `json:"state"`
			} `json:"issue"`
		} `json:"source"`
	}
	if err := json.Unmarshal(body, &events); err != nil {
		return false
	}

	for _, e := range events {
		if e.Event == "cross-referenced" && e.Source != nil && e.Source.Issue != nil {
			if e.Source.Issue.PullRequest != nil && e.Source.Issue.State == "closed" {
				return true // A merged PR references this issue
			}
		}
	}
	return false
}

// checkPROutcomes checks PRs that OpinAI reviewed to see if they were merged or closed.
func checkPROutcomes(repo string) {
	reviews, err := database.GetPRReviews(repo, 100)
	if err != nil {
		return
	}

	for _, review := range reviews {
		if review.Verdict == "" {
			continue
		}
		has, _ := database.HasOutcome(repo, "pr_review", review.PRNumber)
		if has {
			continue
		}

		// Check PR state
		pr, err := FetchPRDetails(repo, review.PRNumber)
		if err != nil {
			continue
		}

		// Only record outcomes for closed/merged PRs
		if pr.State == "open" {
			continue
		}

		outcome := determinePROutcome(repo, review, pr)
		if outcome.ActualOutcome == "" {
			continue
		}

		if _, err := database.AddOutcome(outcome); err != nil {
			slog.Error("failed to record PR outcome", "repo", repo, "pr", review.PRNumber, "error", err)
		} else {
			slog.Info("recorded PR outcome", "repo", repo, "pr", review.PRNumber,
				"verdict", review.Verdict, "outcome", outcome.ActualOutcome, "correct", outcome.Correct)
		}
	}
}

// determinePROutcome evaluates a closed/merged PR against OpinAI's review verdict.
func determinePROutcome(repo string, review database.PRReview, pr *PullRequest) database.Outcome {
	outcome := database.Outcome{
		Repo:            repo,
		Type:            "pr_review",
		ReferenceNumber: review.PRNumber,
		OpinaiVerdict:   review.Verdict,
	}

	// Check if PR was merged by looking at the merged_at field
	merged := pr.State == "closed" && isMerged(repo, review.PRNumber)

	switch review.Verdict {
	case "APPROVE":
		if merged {
			outcome.ActualOutcome = "pr_merged"
			outcome.OutcomeDetails = "PR merged — consistent with APPROVE"
			correct := true
			outcome.Correct = &correct
		} else {
			outcome.ActualOutcome = "pr_closed"
			outcome.OutcomeDetails = "PR closed without merge despite APPROVE"
			// Ambiguous — PR could be closed for non-review reasons
		}

	case "CHANGES_REQUESTED":
		if merged {
			outcome.ActualOutcome = "pr_merged"
			outcome.OutcomeDetails = "PR merged despite CHANGES_REQUESTED"
			// Ambiguous — changes may have been made, or feedback was optional
		} else {
			outcome.ActualOutcome = "pr_closed"
			outcome.OutcomeDetails = "PR closed — may be consistent with CHANGES_REQUESTED"
			correct := true
			outcome.Correct = &correct
		}

	case "COMMENT":
		if merged {
			outcome.ActualOutcome = "pr_merged"
		} else {
			outcome.ActualOutcome = "pr_closed"
		}
		outcome.OutcomeDetails = "PR " + outcome.ActualOutcome + " (OpinAI had COMMENT verdict)"
		// COMMENT is neutral — no correctness signal

	default:
		if merged {
			outcome.ActualOutcome = "pr_merged"
		} else {
			outcome.ActualOutcome = "pr_closed"
		}
	}

	return outcome
}

// isMerged checks if a PR was merged (not just closed).
func isMerged(repo string, prNumber int) bool {
	_, code, err := ghGet(fmt.Sprintf("/repos/%s/pulls/%d/merge", repo, prNumber))
	if err != nil {
		return false
	}
	return code == 204 // 204 = merged, 404 = not merged
}
