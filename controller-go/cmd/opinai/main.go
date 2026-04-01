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
)

func main() {
	mode := flag.String("mode", "controller", "Run mode: controller or runner")
	httpAddr := flag.String("http", ":8081", "HTTP listen address")
	httpsAddr := flag.String("https", ":8444", "HTTPS listen address")
	dbPath := flag.String("db", "", "SQLite database path (default: $OPINAI_DB_PATH or /data/opinai.db)")
	flag.Parse()

	logBuf := dashboard.NewLogBuffer(200)
	logWriter := io.MultiWriter(os.Stderr, logBuf)
	slog.SetDefault(slog.New(slog.NewTextHandler(logWriter, &slog.HandlerOptions{Level: slog.LevelInfo})))

	switch *mode {
	case "controller":
		runController(*httpAddr, *httpsAddr, *dbPath, logBuf)
	case "runner":
		slog.Info("runner mode not yet implemented")
		os.Exit(1)
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

	// Wire reproduce callback
	if jobMgr != nil {
		srv.SetReproduceCallback(func(repo string, issue int) error {
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

	// Start poller (if K8s available)
	if jobMgr != nil {
		poller := controller.NewPoller(state, jobMgr, interval, repos)
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
