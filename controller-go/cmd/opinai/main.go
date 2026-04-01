package main

import (
	"flag"
	"log/slog"
	"os"

	"github.com/yossiovadia/opinai/controller-go/internal/dashboard"
	"github.com/yossiovadia/opinai/controller-go/internal/database"
)

func main() {
	mode := flag.String("mode", "controller", "Run mode: controller or runner")
	httpAddr := flag.String("http", ":8081", "HTTP listen address")
	httpsAddr := flag.String("https", ":8444", "HTTPS listen address")
	dbPath := flag.String("db", "", "SQLite database path (default: $OPINAI_DB_PATH or /data/opinai.db)")
	flag.Parse()

	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo})))

	switch *mode {
	case "controller":
		runController(*httpAddr, *httpsAddr, *dbPath)
	case "runner":
		slog.Info("runner mode not yet implemented")
		os.Exit(1)
	default:
		slog.Error("unknown mode", "mode", *mode)
		os.Exit(1)
	}
}

func runController(httpAddr, httpsAddr, dbPath string) {
	// Database path
	if dbPath == "" {
		dbPath = dashboard.Env("OPINAI_DB_PATH", "/data/opinai.db")
	}

	// Initialize database
	if err := database.Init(dbPath); err != nil {
		slog.Error("failed to initialize database", "error", err)
		os.Exit(1)
	}

	// Shared state
	state := dashboard.NewState()

	// Populate repos from env
	reposStr := dashboard.Env("REPOS", "")
	for _, repo := range dashboard.ParseRepos(reposStr) {
		stats, _ := database.GetStats(repo)
		state.UpdateRepo(repo, dashboard.RepoStatus{
			Pending:    0,
			Processed:  stats.Processed,
			ManualOnly: stats.Processed == 0,
			LastCheck:  "",
		})
	}

	// Start dashboard
	srv := dashboard.New(state)

	slog.Info("OpinAI Go controller starting",
		"http", httpAddr,
		"https", httpsAddr,
		"repos", reposStr,
	)

	go srv.StartHTTPS(httpsAddr)
	srv.StartHTTP(httpAddr) // blocks
}
