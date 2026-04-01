package main

import (
	"flag"
	"io"
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

	// Log buffer captures lines for the admin /api/admin/logs endpoint
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

	state := dashboard.NewState()

	// Populate repos from env
	for _, repo := range dashboard.ParseRepos(dashboard.Env("REPOS", "")) {
		stats, _ := database.GetStats(repo)
		state.UpdateRepo(repo, dashboard.RepoStatus{
			Processed:  stats.Processed,
			ManualOnly: stats.Processed == 0,
		})
	}

	srv := dashboard.New(state, logBuf)

	slog.Info("OpinAI Go controller starting",
		"http", httpAddr,
		"https", httpsAddr,
	)

	go srv.StartHTTPS(httpsAddr)
	srv.StartHTTP(httpAddr) // blocks
}
