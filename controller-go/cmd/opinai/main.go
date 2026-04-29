package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/yossiovadia/opinai/controller-go/internal/controller"
	"github.com/yossiovadia/opinai/controller-go/internal/dashboard"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
	"github.com/yossiovadia/opinai/controller-go/internal/hostprofile"
	"github.com/yossiovadia/opinai/controller-go/internal/infra"
	"github.com/yossiovadia/opinai/controller-go/internal/runner"
	"github.com/yossiovadia/opinai/controller-go/internal/sandbox"
)

func main() {
	mode := flag.String("mode", "controller", "Run mode: controller or runner")
	httpAddr := flag.String("http", ":8080", "HTTP listen address")
	httpsAddr := flag.String("https", ":8443", "HTTPS listen address")
	dbPath := flag.String("db", "", "SQLite database path (default: $OPINAI_DB_PATH or /data/opinai.db)")
	flag.Parse()

	logBuf := dashboard.NewLogBuffer(200)
	logWriter := io.MultiWriter(os.Stderr, logBuf)
	slog.SetDefault(slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: slog.LevelInfo})))

	switch *mode {
	case "controller":
		runController(*httpAddr, *httpsAddr, *dbPath, logBuf)
	case "runner":
		runner.Run()
	default:
		slog.Error("unknown mode", "mode", *mode)
		os.Exit(1)
	}
}

func runController(httpAddr, httpsAddr, dbPath string, logBuf *dashboard.LogBuffer) {
	if dbPath == "" {
		dbPath = dashboard.Env("OPINAI_DB_PATH", "/data/opinai.db")
	}

	if err := database.Init(dbPath); err != nil {
		slog.Error("failed to initialize database", "error", err)
		os.Exit(1)
	}

	// Clean up duplicate PR reviews from prior dedup bugs
	if n, err := database.DeduplicatePRReviews(); err != nil {
		slog.Warn("failed to deduplicate PR reviews", "error", err)
	} else if n > 0 {
		slog.Info("cleaned up duplicate PR reviews", "removed", n)
	}

	// Backfill investigation findings from existing runs
	dashboard.BackfillFindings()

	// Initialize K8s client
	k8sClient, err := initK8sClient()
	if err != nil {
		slog.Warn("K8s client not available — Job management disabled", "error", err)
	}

	// Detect host profile (K8s + local hardware)
	profile := hostprofile.Detect(k8sClient)
	hostprofile.LogSummary(profile)
	if err := database.SaveHostProfile(profile.JSON()); err != nil {
		slog.Warn("failed to save host profile to DB", "error", err)
	}

	namespace := dashboard.Env("NAMESPACE", "opinai")
	image := dashboard.Env("OPINAI_IMAGE", fmt.Sprintf("image-registry.openshift-image-registry.svc:5000/%s/opinai-controller:latest", namespace))

	// Merge repos from env var and database for persistence across restarts
	envRepos := dashboard.ParseRepos(dashboard.Env("REPOS", ""))
	dbRepos := database.GetMonitoredRepos()
	repoSet := make(map[string]bool)
	for _, r := range envRepos {
		repoSet[r] = true
	}
	for _, r := range dbRepos {
		repoSet[r] = true
	}
	var repos []string
	for r := range repoSet {
		repos = append(repos, r)
	}
	// Persist env repos to DB so they survive restart
	for _, r := range envRepos {
		database.AddMonitoredRepo(r)
	}
	// Update REPOS env var so other code sees the full list
	os.Setenv("REPOS", strings.Join(repos, ","))

	// Shared state
	state := dashboard.NewState()
	for _, repo := range repos {
		stats, _ := database.GetStats(repo)
		state.UpdateRepo(repo, dashboard.RepoStatus{
			Processed:  stats.Processed,
			ManualOnly: stats.Processed == 0,
		})
	}

	// Job manager (nil-safe if no K8s)
	var jobMgr *controller.JobManager
	if k8sClient != nil {
		jobMgr = controller.NewJobManager(k8sClient, namespace, image)
		jobMgr.SetHostProfile(profile)
		jobMgr.CleanupOrphanedJobs(repos)
	}

	// Dashboard
	srv := dashboard.New(state, logBuf)

	// Wire WebSocket hub to job manager for push notifications
	if jobMgr != nil {
		jobMgr.SetBroadcaster(&hubAdapter{hub: srv.GetHub()})
	}

	// Wire sandbox manager
	if k8sClient != nil {
		sbMgr := sandbox.NewManager(k8sClient, namespace)
		// Set up dynamic client for CRD resource deployment
		if k8sConfig, cfgErr := getK8sConfig(); cfgErr == nil {
			if dynClient, dynErr := dynamic.NewForConfig(k8sConfig); dynErr == nil {
				sbMgr.SetDynamicClient(dynClient)
				if jobMgr != nil {
					jobMgr.SetSandboxDynamicClient(dynClient)
				}
				slog.Info("dynamic K8s client available for CRD deployment")
			}
		}
		srv.SetSandboxManager(&sandboxAdapter{mgr: sbMgr})
	}

	// Wire infrastructure dependency manager
	if k8sClient != nil {
		infraMgr := infra.NewManager(k8sClient)
		if jobMgr != nil {
			jobMgr.SetInfraManager(infraMgr)
		}
		srv.SetInfraManager(&infraAdapter{mgr: infraMgr})
		slog.Info("infrastructure dependency manager initialized")
	}

	// Wire callbacks
	if jobMgr != nil {
		srv.SetListJobsCallback(func() []dashboard.JobInfo {
			jobs := jobMgr.ListJobs()
			result := make([]dashboard.JobInfo, len(jobs))
			for i, j := range jobs {
				result[i] = dashboard.JobInfo{
					Repo: j.Repo, Issue: j.Issue, Status: j.Status,
					CreatedAt: j.CreatedAt, PodName: j.PodName, Type: j.Type,
				}
			}
			return result
		})
		srv.SetListPRJobsCallback(func() []dashboard.JobInfo {
			jobs := jobMgr.ListPRJobs()
			result := make([]dashboard.JobInfo, len(jobs))
			for i, j := range jobs {
				result[i] = dashboard.JobInfo{
					Repo: j.Repo, Issue: j.Issue, Status: j.Status,
					CreatedAt: j.CreatedAt, Type: j.Type,
				}
			}
			return result
		})
		srv.SetReproduceCallback(func(repo string, issue int) error {
			details, err := controller.FetchIssueDetails(repo, issue)
			if err != nil {
				return fmt.Errorf("fetch issue %s#%d: %w", repo, issue, err)
			}
			if details.PullRequest != nil {
				slog.Info("issue is actually a PR, routing to PR review", "repo", repo, "pr", issue)
				return jobMgr.CreatePRReviewJob(repo, details.Number, details.Title)
			}
			return jobMgr.CreateReproductionJob(repo, details.Number, details.Title)
		})
		srv.SetVerifyFixCallback(func(repo string, issue int) error {
			details, err := controller.FetchIssueDetails(repo, issue)
			if err != nil {
				return fmt.Errorf("fetch issue %s#%d: %w", repo, issue, err)
			}
			return jobMgr.CreateVerifyFixJob(repo, details.Number, details.Title)
		})
		srv.SetClearRecordedCallback(func(repo string, issue int) {
			jobMgr.ClearRecorded(repo, issue)
		})
		srv.SetRerunCallback(func(repo string, issue int) error {
			// Clear recorded state so the new job can be harvested
			jobMgr.ClearRecorded(repo, issue)
			// Delete old job to allow re-creation
			name := controller.JobName(repo, issue)
			_ = jobMgr.DeleteJob(name)
			// Create new job
			details, err := controller.FetchIssueDetails(repo, issue)
			if err != nil {
				return fmt.Errorf("fetch issue %s#%d: %w", repo, issue, err)
			}
			if details.PullRequest != nil {
				slog.Info("rerun: issue is actually a PR, routing to PR review", "repo", repo, "pr", issue)
				return jobMgr.CreatePRReviewJob(repo, details.Number, details.Title)
			}
			return jobMgr.CreateReproductionJob(repo, details.Number, details.Title)
		})
		srv.SetReviewPRCallback(func(repo string, prNumber int, title string) error {
			return jobMgr.CreatePRReviewJob(repo, prNumber, title)
		})
	}

	// Poll interval
	intervalMin, _ := strconv.Atoi(dashboard.Env("POLL_INTERVAL_MINUTES", "60"))
	interval := time.Duration(intervalMin) * time.Minute

	slog.Info("OpinAI Go controller starting",
		"http", httpAddr,
		"https", httpsAddr,
		"repos", len(repos),
		"poll_interval", interval,
		"k8s", k8sClient != nil,
	)

	// Start poller and watcher (if K8s available)
	if jobMgr != nil {
		poller := controller.NewPoller(state, jobMgr, interval, repos)

		// Wire callbacks that need the poller
		srv.SetMarkRecordedCallback(func(repo string, issue int) {
			jobMgr.MarkRecorded(repo, issue)
		})
		srv.SetMarkPRRecordedCallback(func(repo string, prNumber int) {
			jobMgr.MarkPRRecorded(repo, prNumber)
		})
		srv.SetRetryPendingCallback(func(repo string) {
			poller.RetryPendingForRepo(repo)
		})
		jobMgr.SetOnComplete(func(repo string) {
			poller.RetryPendingForRepo(repo)
		})

		// Wire check-outcomes callback
		srv.SetCheckOutcomesCallback(func() {
			controller.CheckOutcomes(repos)
		})

		go jobMgr.StartWatcher()
		go poller.Start()
		go poller.StartPendingProcessor()

		// Run outcome check once on startup after a delay
		go func() {
			time.Sleep(30 * time.Second)
			slog.Info("running startup outcome check")
			controller.CheckOutcomes(repos)
		}()
	}

	// Create HTTP servers
	httpSrv := srv.NewHTTPServer(httpAddr)
	httpsSrv := srv.NewHTTPSServer(httpsAddr)

	// Start servers in goroutines
	go func() {
		slog.Info("dashboard HTTP server starting", "addr", httpAddr)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			slog.Error("HTTP server error", "error", err)
		}
	}()
	if httpsSrv != nil {
		go func() {
			slog.Info("dashboard HTTPS server starting", "addr", httpsAddr)
			if err := httpsSrv.ListenAndServeTLS("", ""); err != nil && err != http.ErrServerClosed {
				slog.Error("HTTPS server error", "error", err)
			}
		}()
	}

	// Graceful shutdown on SIGTERM/SIGINT
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGTERM, syscall.SIGINT)
	sig := <-quit
	slog.Info("shutting down", "signal", sig)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		slog.Error("HTTP shutdown error", "error", err)
	}
	if httpsSrv != nil {
		if err := httpsSrv.Shutdown(ctx); err != nil {
			slog.Error("HTTPS shutdown error", "error", err)
		}
	}
	slog.Info("shutdown complete")
}

func getK8sConfig() (*rest.Config, error) {
	config, err := rest.InClusterConfig()
	if err == nil {
		return config, nil
	}
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}
	return clientcmd.BuildConfigFromFlags("", kubeconfig)
}

func initK8sClient() (kubernetes.Interface, error) {
	config, err := getK8sConfig()
	if err != nil {
		return nil, fmt.Errorf("no K8s config available: %w", err)
	}
	return kubernetes.NewForConfig(config)
}

// sandboxAdapter bridges sandbox.Manager to dashboard.SandboxManagerIface.
type sandboxAdapter struct {
	mgr *sandbox.Manager
}

func (a *sandboxAdapter) ListSandboxes() []dashboard.SandboxInfo {
	sbList := a.mgr.ListSandboxes()
	result := make([]dashboard.SandboxInfo, len(sbList))
	for i, sb := range sbList {
		result[i] = dashboard.SandboxInfo{
			Namespace:  sb.Namespace,
			Repo:       sb.Repo,
			Issue:      sb.Issue,
			CreatedAt:  sb.CreatedAt,
			AgeSeconds: sb.AgeSeconds,
		}
	}
	return result
}

func (a *sandboxAdapter) TeardownSandbox(ns string) bool {
	return a.mgr.TeardownSandbox(ns)
}

func (a *sandboxAdapter) AutoCleanup(maxAge int) int {
	return a.mgr.AutoCleanup(maxAge)
}

// infraAdapter bridges infra.Manager to dashboard.InfraManagerIface.
type infraAdapter struct {
	mgr *infra.Manager
}

func (a *infraAdapter) EnsureRunning(dep string) (string, error) {
	return a.mgr.EnsureRunning(dep)
}

func (a *infraAdapter) Stop(dep string) error {
	return a.mgr.Stop(dep)
}

func (a *infraAdapter) Teardown(dep string) error {
	return a.mgr.Teardown(dep)
}

func (a *infraAdapter) ListAll() ([]dashboard.InfraDepInfo, error) {
	deps, err := a.mgr.ListAll()
	if err != nil {
		return nil, err
	}
	result := make([]dashboard.InfraDepInfo, len(deps))
	for i, d := range deps {
		result[i] = dashboard.InfraDepInfo{
			Name:        d.Name,
			Namespace:   d.Namespace,
			Status:      d.Status,
			InstalledAt: d.InstalledAt,
			LastUsedAt:  d.LastUsedAt,
			Connection:  d.Connection,
			HelmRelease: d.HelmRelease,
		}
	}
	return result, nil
}

// hubAdapter bridges dashboard.Hub to controller.Broadcaster interface.
type hubAdapter struct {
	hub *dashboard.Hub
}

func (a *hubAdapter) Broadcast(event controller.BroadcastEvent) {
	a.hub.Broadcast(dashboard.WSEvent{Type: event.Type, Data: event.Data})
}
