package controller

import (
	"log/slog"
	"os"
	"time"

	"github.com/yossiovadia/opinai/controller-go/internal/dashboard"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

// Poller periodically checks GitHub repos for new issues.
type Poller struct {
	state    *dashboard.State
	jobs     *JobManager
	interval time.Duration
	repos    []string
}

// NewPoller creates a poller.
func NewPoller(state *dashboard.State, jobs *JobManager, interval time.Duration, repos []string) *Poller {
	return &Poller{
		state:    state,
		jobs:     jobs,
		interval: interval,
		repos:    repos,
	}
}

// Start begins the polling loop. Blocks forever.
func (p *Poller) Start() {
	pollCount := 0

	for {
		pollCount++
		now := time.Now().UTC().Format("2006-01-02T15:04:05Z")
		p.state.SetPollInfo(pollCount, now)

		totalNew := 0
		// Re-read REPOS each cycle in case admin added/removed repos
		repos := dashboard.ParseRepos(os.Getenv("REPOS"))
		if len(repos) == 0 {
			repos = p.repos
		}

		for _, repo := range repos {
			stats, _ := database.GetStats(repo)

			// Ensure repo has a "monitored_since" timestamp
			since := ensureMonitoredSince(repo, stats)

			slog.Info("checking repo for issues", "repo", repo, "since", since)
			issues, err := FetchOpenIssues(repo, since)
			if err != nil {
				slog.Error("failed to fetch issues", "repo", repo, "error", err)
				continue
			}

			// Filter already-processed
			var newIssues []Issue
			for _, issue := range issues {
				processed, _ := database.IsProcessed(repo, issue.Number)
				if !processed {
					newIssues = append(newIssues, issue)
				}
			}

			slog.Info("found unprocessed issues", "repo", repo, "count", len(newIssues))
			totalNew += len(newIssues)

			p.state.UpdateRepo(repo, dashboard.RepoStatus{
				Pending:   len(newIssues),
				Processed: stats.Processed,
				LastCheck:  now,
			})

			for _, issue := range newIssues {
				if err := p.jobs.CreateReproductionJob(repo, issue.Number, issue.Title); err != nil {
					slog.Error("failed to create job", "repo", repo, "issue", issue.Number, "error", err)
				}
			}
		}

		// Harvest completed jobs
		p.jobs.HarvestCompletedJobs()

		// Clean up orphaned sandboxes (older than 30 min)
		if p.jobs.HasSandbox() {
			cleaned := p.jobs.CleanupSandboxes(1800)
			if cleaned > 0 {
				slog.Info("auto-cleaned orphaned sandboxes", "count", cleaned)
			}
		}

		// Check deployment plan freshness
		checkPlanStaleness(repos)

		// Update check result for dashboard
		p.state.SetCheckResult(&dashboard.CheckResult{Total: totalNew})

		// Wait for next poll or manual trigger
		select {
		case <-p.state.CheckNow:
			slog.Info("manual check triggered from dashboard")
			continue
		case <-time.After(p.interval):
		}
	}
}


// ensureMonitoredSince returns the "monitored_since" timestamp for a repo.
// If none exists, sets it to now (new repo) or earliest known activity (existing repo).
func ensureMonitoredSince(repo string, stats database.RepoStats) string {
	mem, _ := database.GetRepoMemory(repo, strPtr("monitored_since"))
	if v, ok := mem["monitored_since"]; ok && v != "" {
		return v
	}

	// No monitored_since yet — determine the right timestamp
	var since string
	if stats.Processed > 0 || stats.TotalRuns > 0 {
		// Existing repo with history — use earliest run timestamp as baseline
		runs, _ := database.GetRuns(repo, 1)
		if len(runs) > 0 {
			since = runs[0].CreatedAt
		}
		if since == "" {
			since = time.Now().UTC().Format("2006-01-02T15:04:05Z")
		}
		// Mark all currently-open issues as processed to prevent backlog flood
		markBacklogProcessed(repo)
	} else {
		// Brand new repo — start from now
		since = time.Now().UTC().Format("2006-01-02T15:04:05Z")
	}

	database.SetRepoMemory(repo, "monitored_since", since)
	slog.Info("set monitored_since for repo", "repo", repo, "since", since)
	return since
}

// markBacklogProcessed marks all currently-open issues as processed (DB only, no analysis).
// This prevents the auto-poller from creating Jobs for the entire backlog.
func markBacklogProcessed(repo string) {
	issues, err := FetchOpenIssues(repo, "")
	if err != nil {
		return
	}
	count := 0
	for _, issue := range issues {
		already, _ := database.IsProcessed(repo, issue.Number)
		if !already {
			database.MarkProcessed(repo, issue.Number, "backlog-skip")
			count++
		}
	}
	if count > 0 {
		slog.Info("marked backlog issues as processed", "repo", repo, "count", count)
	}
}

// StartPendingProcessor runs a loop that checks pending_reproductions every 10s
// and creates jobs for queued items. This ensures the /api/reproduce endpoint
// returns immediately while jobs are created in the background.
func (p *Poller) StartPendingProcessor() {
	slog.Info("pending processor started")
	for {
		pending := database.GetAllPending()
		for _, item := range pending {
			// Skip if already processed
			processed, _ := database.IsProcessed(item.Repo, item.Issue)
			if processed {
				database.RemovePending(item.Repo, item.Issue)
				continue
			}

			// Check concurrency — skip if repo already has a running job
			_, repoActive := p.jobs.countRunningJobs(item.Repo)
			if repoActive {
				continue
			}

			slog.Info("pending processor: creating job", "repo", item.Repo, "issue", item.Issue)
			// Remove from pending BEFORE creating job (prevents duplicate triggers during long builds)
			database.RemovePending(item.Repo, item.Issue)
			title := item.Title
			if title == "" {
				if details, err := FetchIssueDetails(item.Repo, item.Issue); err == nil {
					title = details.Title
				}
			}
			if err := p.jobs.CreateReproductionJob(item.Repo, item.Issue, title); err != nil {
				slog.Error("pending processor: failed to create job", "repo", item.Repo, "issue", item.Issue, "error", err)
			}
			// Process one at a time, then re-check on next cycle
			break
		}

		// Also process pending PR reviews
		pendingPRs := database.GetAllPendingPRs()
		for _, pr := range pendingPRs {
			_, repoActive := p.jobs.countRunningJobs(pr.Repo)
			if repoActive {
				continue
			}

			slog.Info("pending processor: creating PR review job", "repo", pr.Repo, "pr", pr.PRNumber)
			if err := p.jobs.CreatePRReviewJob(pr.Repo, pr.PRNumber, pr.Title); err != nil {
				slog.Error("pending processor: failed to create PR review job", "repo", pr.Repo, "pr", pr.PRNumber, "error", err)
			}
			// Process one at a time, then re-check on next cycle
			break
		}

		time.Sleep(10 * time.Second)
	}
}

// RetryPendingForRepo checks for unprocessed issues in a repo and creates a job
// for the next one. Called when a job completes so queued issues don't wait for
// the next poll cycle.
func (p *Poller) RetryPendingForRepo(repo string) {
	slog.Info("retry pending: checking repo", "repo", repo)

	// Check the pending_reproductions queue first (manually triggered issues)
	pending := database.GetPendingForRepo(repo)
	for _, item := range pending {
		processed, _ := database.IsProcessed(repo, item.Issue)
		if !processed {
			slog.Info("retry pending: creating job for queued issue", "repo", repo, "issue", item.Issue)
			if err := p.jobs.CreateReproductionJob(repo, item.Issue, item.Title); err != nil {
				slog.Error("retry pending: failed to create job", "repo", repo, "issue", item.Issue, "error", err)
			}
			return // one at a time per repo
		}
		// Already processed — clean up stale entry
		database.RemovePending(repo, item.Issue)
	}

	// Also check polled issues from GitHub
	since := ""
	if mem, _ := database.GetRepoMemory(repo, strPtr("monitored_since")); len(mem) > 0 {
		since = mem["monitored_since"]
	}
	issues, err := FetchOpenIssues(repo, since)
	if err != nil {
		slog.Warn("retry pending: failed to fetch issues", "repo", repo, "error", err)
		return
	}
	for _, issue := range issues {
		processed, _ := database.IsProcessed(repo, issue.Number)
		if !processed {
			slog.Info("retry pending: creating job for polled issue", "repo", repo, "issue", issue.Number)
			if err := p.jobs.CreateReproductionJob(repo, issue.Number, issue.Title); err != nil {
				slog.Error("retry pending: failed to create job", "repo", repo, "issue", issue.Number, "error", err)
			}
			return // one at a time per repo
		}
	}

	// Nothing pending — update state
	stats, _ := database.GetStats(repo)
	p.state.UpdateRepo(repo, dashboard.RepoStatus{
		Pending:   0,
		Processed: stats.Processed,
	})
}

func checkPlanStaleness(repos []string) {
	for _, repo := range repos {
		plan, err := database.GetDeploymentPlan(repo)
		if err != nil || plan == nil || plan.CommitSHA == "" {
			continue
		}
		if plan.Status == "stale" {
			continue // already marked
		}
		headSHA, err := GetRepoHeadSHA(repo)
		if err != nil {
			continue
		}
		if headSHA != plan.CommitSHA {
			database.UpdateDeploymentPlanStatus(repo, "stale")
			slog.Info("deployment plan marked stale — repo has new commits", "repo", repo)
		}
	}
}
