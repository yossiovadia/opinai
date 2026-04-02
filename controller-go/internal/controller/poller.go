package controller

import (
	"encoding/json"
	"log/slog"
	"os"
	"strings"
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
	doneLabel := envOr("DONE_LABEL", "opinai-done")
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
			profile := loadRepoProfile(repo)
			stats, _ := database.GetStats(repo)

			// Skip k8s-required repos (manual only)
			if profile != nil && getBool(profile, "k8s") {
				p.state.UpdateRepo(repo, dashboard.RepoStatus{
					Processed:  stats.Processed,
					ManualOnly: true,
					LastCheck:  now,
				})
				continue
			}

			// Skip newly added repos (no processed issues yet = manual only)
			if stats.Processed == 0 {
				p.state.UpdateRepo(repo, dashboard.RepoStatus{
					Processed:  0,
					ManualOnly: true,
					LastCheck:  now,
				})
				continue
			}

			slog.Info("checking repo for issues", "repo", repo)
			issues, err := FetchOpenIssues(repo, doneLabel)
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

func loadRepoProfile(repo string) map[string]any {
	r := strings.NewReplacer("/", "_", "-", "_", ".", "_")
	key := "REPO_PROFILE_" + r.Replace(repo)
	raw := os.Getenv(key)
	if raw == "" {
		return nil
	}
	var profile map[string]any
	if err := json.Unmarshal([]byte(raw), &profile); err != nil {
		return nil
	}
	return profile
}

func getBool(m map[string]any, key string) bool {
	v, ok := m[key]
	if !ok {
		return false
	}
	switch b := v.(type) {
	case bool:
		return b
	case string:
		return strings.EqualFold(b, "true")
	default:
		return false
	}
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
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
