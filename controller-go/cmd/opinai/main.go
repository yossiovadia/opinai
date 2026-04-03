package main

import (
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/yossiovadia/opinai/controller-go/internal/controller"
	"github.com/yossiovadia/opinai/controller-go/internal/dashboard"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
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

	// Initialize K8s client
	k8sClient, err := initK8sClient()
	if err != nil {
		slog.Warn("K8s client not available — Job management disabled", "error", err)
	}

	namespace := dashboard.Env("NAMESPACE", "opinai")
	image := dashboard.Env("OPINAI_IMAGE", "image-registry.openshift-image-registry.svc:5000/opinai/opinai-controller:latest")
	repos := dashboard.ParseRepos(dashboard.Env("REPOS", ""))

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
		srv.SetSandboxManager(&sandboxAdapter{mgr: sbMgr})
	}

	// Wire reproduce callback
	if jobMgr != nil {
		srv.SetReproduceCallback(func(repo string, issue int) error {
			details, err := controller.FetchIssueDetails(repo, issue)
			if err != nil {
				return fmt.Errorf("fetch issue %s#%d: %w", repo, issue, err)
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
			return jobMgr.CreateReproductionJob(repo, details.Number, details.Title)
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
		srv.SetRetryPendingCallback(func(repo string) {
			poller.RetryPendingForRepo(repo)
		})
		jobMgr.SetOnComplete(func(repo string) {
			poller.RetryPendingForRepo(repo)
		})

		go jobMgr.StartWatcher()
		go poller.Start()
	}

	// Start dashboard
	go srv.StartHTTPS(httpsAddr)
	srv.StartHTTP(httpAddr) // blocks
}

func initK8sClient() (kubernetes.Interface, error) {
	// Try in-cluster config first
	config, err := rest.InClusterConfig()
	if err == nil {
		return kubernetes.NewForConfig(config)
	}

	// Fall back to kubeconfig
	kubeconfig := os.Getenv("KUBECONFIG")
	if kubeconfig == "" {
		home, _ := os.UserHomeDir()
		kubeconfig = home + "/.kube/config"
	}
	config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
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

// hubAdapter bridges dashboard.Hub to controller.Broadcaster interface.
type hubAdapter struct {
	hub *dashboard.Hub
}

func (a *hubAdapter) Broadcast(event controller.BroadcastEvent) {
	a.hub.Broadcast(dashboard.WSEvent{Type: event.Type, Data: event.Data})
}
